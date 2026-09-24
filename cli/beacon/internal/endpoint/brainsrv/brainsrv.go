// Package brainsrv is the afferent content pack and forwarder for shipping Beacon's runtime
// JSONL to brainsrv's Beacon ingest (afferent SPEC §4 B3, PLAN §4 Phase 5).
//
// It is cloned from the asymptote pack and shaped the same way: Beacon stays the local JSONL
// producer, Vector does the network, and the member's spk_ API key lives in a separate 0600
// secrets file read through Vector's secret backend. What asymptote does that this does not:
// device enrollment, account/device-key flows, reconnect, privacy-mode transforms (the
// # BEACON_PRIVACY_TRANSFORMS marker stays as the hook for later) and the inventory stream.
//
// Every piece of state is separate from the Beacon Managed forwarder (service label and unit,
// state directory, secrets file, Vector data_dir and so checkpoints and buffer), so both can
// run at once.
package brainsrv

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"strings"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/brainsrvcfg"
	endpointconfig "github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/config"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/siempack"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/writer"
)

//go:embed pack/*
var packFS embed.FS

const (
	// DefaultOutputDir is where install-pack writes when --output is omitted.
	DefaultOutputDir = "afferent-brainsrv-pack"

	// Environment references in the template. connect replaces every one with a literal, so
	// the service unit needs no environment; a hand-run Vector exports them.
	EnvURL         = "BEACON_BRAINSRV_URL"
	EnvScope       = "BEACON_BRAINSRV_SCOPE"
	EnvSecretsFile = "BEACON_BRAINSRV_SECRETS_FILE"
	EnvDataDir     = "BEACON_BRAINSRV_DATA_DIR"
	EnvReadFrom    = "BEACON_BRAINSRV_READ_FROM"

	// SecretsKey is the JSON key inside the secrets file; the template references it as
	// SECRET[beacon.brainsrv_key].
	SecretsKey = "brainsrv_key"

	// RuntimeIngestPath and HealthPath are appended to the brainsrv base URL.
	RuntimeIngestPath = "/v1/ingest/beacon/runtime"
	HealthPath        = "/v1/ingest/beacon/health"

	// ReadFromEnd and ReadFromBeginning are the two read_from values the render accepts.
	ReadFromEnd       = "end"
	ReadFromBeginning = "beginning"

	// SourceID is the Vector file source's component id; its checkpoints live under
	// <data_dir>/<SourceID>.
	SourceID = "beacon_runtime"

	// DefaultTemplateDataDir is the template's data_dir when BEACON_BRAINSRV_DATA_DIR is unset.
	DefaultTemplateDataDir = "/var/lib/vector/afferent-brainsrv"
)

// Service names for the forwarder, distinct from service.ForwarderLabel and
// service.ForwarderSystemdUnit (the Beacon Managed forwarder).
const (
	LaunchdLabel       = "com.afferent.brainsrv-forwarder"
	SystemdUnit        = "afferent-brainsrv-forwarder.service"
	ServiceDescription = "afferent: Beacon endpoint forwarder to brainsrv"
)

// DefaultLogPath is resolved per platform like the other packs.
var DefaultLogPath = endpointconfig.SystemLogPath()

const (
	vectorAsset            = "pack/vector.toml.tmpl"
	privacyTransformMarker = "# BEACON_PRIVACY_TRANSFORMS"
	logPathToken           = "{{LOG_PATH}}"
	// globMetacharacters are special in a Vector file source's include pattern.
	globMetacharacters = "*?["
)

// File is the installable pack-file type, shared with siempack.
type File = siempack.File

var pack = siempack.Pack{
	Label:            "brainsrv",
	FS:               packFS,
	DefaultLogPath:   DefaultLogPath,
	DefaultOutputDir: DefaultOutputDir,
	Assets: []siempack.Asset{
		{Source: "pack/README.md", Name: "README.md"},
		{Source: vectorAsset, Name: "vector.toml", TemplateLogPath: true},
	},
}

// filesFromFS builds the file list from the supplied FS; tests use it to inject read errors.
func filesFromFS(fsys fs.FS) ([]File, error) { return pack.WithFS(fsys).Files() }

// Files returns all pack files, propagating any embedded asset read error.
func Files() ([]File, error) { return pack.Files() }

// VectorConfig returns the Vector forwarder template with logPath substituted. URL, scope,
// secrets file, data dir and read_from stay as environment references.
func VectorConfig(logPath string) (string, error) { return pack.Render(vectorAsset, logPath) }

// InstallPack writes the pack files to outputDir with logPath substituted.
func InstallPack(outputDir, logPath string) error { return pack.Install(outputDir, logPath) }

// RenderOptions are the concrete values a forwarder unit needs in place of the template's
// environment references.
type RenderOptions struct {
	LogPath     string
	URL         string
	Scope       string
	SecretsFile string
	DataDir     string
	// Backfill renders read_from = "beginning"; otherwise "end".
	Backfill bool
}

// RenderVectorConfig returns vector.toml with every environment reference replaced by a
// literal, for a forwarder unit that must not depend on its environment. The URL goes
// through brainsrvcfg.ValidateURL (https, or http to loopback only), so a downgraded endpoint
// can never receive the key; the scope must be a valid brainsrv scope.
func RenderVectorConfig(opts RenderOptions) (string, error) {
	base, err := brainsrvcfg.ValidateURL(opts.URL)
	if err != nil {
		return "", errors.Join(ErrInsecureURL, err)
	}
	if !brainsrvcfg.ValidScope(opts.Scope) {
		return "", ErrInvalidScope
	}
	if opts.SecretsFile == "" || opts.DataDir == "" {
		return "", ErrIncompleteRender
	}
	logPath := opts.LogPath
	if logPath == "" {
		logPath = DefaultLogPath
	}
	if strings.ContainsAny(logPath, globMetacharacters) {
		return "", ErrGlobLogPath
	}
	// The raw template, not VectorConfig: {{LOG_PATH}} must go through tomlString like every
	// other literal, or a quote in the path would end the TOML string.
	content, err := pack.Read(vectorAsset)
	if err != nil {
		return "", err
	}
	readFrom := ReadFromEnd
	include := []string{logPath}
	if opts.Backfill {
		readFrom = ReadFromBeginning
		// The writer's retained archives (runtime.jsonl.1 ... .N) hold the older history. Vector
		// fingerprints by content, so a file that rotates into an archive is not read twice, and
		// brainsrv deduplicates on event.id anyway.
		include = writer.RetainedLogPaths(logPath)
	}
	quoted := make([]string, len(include))
	for i, path := range include {
		quoted[i] = tomlString(path)
	}
	replacements := []struct{ from, to string }{
		{`["` + logPathToken + `"]`, "[" + strings.Join(quoted, ", ") + "]"},
		{`"${` + EnvDataDir + `:-` + DefaultTemplateDataDir + `}"`, tomlString(opts.DataDir)},
		{`"${` + EnvSecretsFile + `}"`, tomlString(opts.SecretsFile)},
		{`"${` + EnvReadFrom + `:-end}"`, tomlString(readFrom)},
		{`"${` + EnvURL + `}` + RuntimeIngestPath + `"`, tomlString(base + RuntimeIngestPath)},
		{`"${` + EnvURL + `}` + HealthPath + `?scope=${` + EnvScope + `}"`, tomlString(HealthURL(base, opts.Scope))},
		{`"${` + EnvScope + `}"`, tomlString(opts.Scope)},
	}
	for _, r := range replacements {
		content = strings.ReplaceAll(content, r.from, r.to)
	}
	content = strings.Replace(content, privacyTransformMarker, "# No privacy transforms: lines are forwarded byte-for-byte", 1)
	if strings.Contains(content, privacyTransformMarker) || hasUnescapedReference(content) {
		return "", ErrIncompleteRender
	}
	return content, nil
}

// HealthURL is <base>/v1/ingest/beacon/health?scope=<scope>, the URL both connect's grant
// check and Vector's sink healthcheck call. An empty scope only authenticates the key.
func HealthURL(base, scope string) string {
	u := strings.TrimRight(base, "/") + HealthPath
	if scope != "" {
		u += "?" + url.Values{"scope": {scope}}.Encode()
	}
	return u
}

// hasUnescapedReference reports a ${ that Vector would interpolate: one preceded by an even
// number of $ (tomlString writes a literal $ as $$).
func hasUnescapedReference(content string) bool {
	for i := strings.Index(content, "${"); i >= 0; {
		run := 0
		for j := i - 1; j >= 0 && content[j] == '$'; j-- {
			run++
		}
		if run%2 == 0 {
			return true
		}
		next := strings.Index(content[i+2:], "${")
		if next < 0 {
			return false
		}
		i += 2 + next
	}
	return false
}

// tomlString quotes s as a TOML basic string. Vector interpolates $VAR and ${VAR} across the
// whole config text before parsing it, so a literal $ is written as $$.
func tomlString(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`, "$", "$$")
	return `"` + replacer.Replace(s) + `"`
}

// SecretsFileContent returns the JSON document the secrets file holds for the template's
// SECRET[beacon.brainsrv_key] reference.
func SecretsFileContent(key string) string {
	// json.Marshal on a map cannot fail for string values.
	encoded, _ := json.Marshal(map[string]string{SecretsKey: key})
	return string(encoded) + "\n"
}

// Sentinel errors returned by RenderVectorConfig.
type renderError string

func (e renderError) Error() string { return string(e) }

const (
	ErrInsecureURL      renderError = "brainsrv URL must use https:// (plain http is allowed only for a loopback development server)"
	ErrInvalidScope     renderError = "brainsrv scope must match ^[a-z0-9_]+(\\.[a-z0-9_]+)*$"
	ErrIncompleteRender renderError = "brainsrv forwarder render needs URL, scope, secrets file and data dir"
	ErrGlobLogPath      renderError = "brainsrv forwarder log path must not contain *, ? or [ (Vector would treat it as a glob)"
)
