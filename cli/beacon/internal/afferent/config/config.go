// Package config holds the afferent CLI's settings: which authsrv issues its
// tokens, which OAuth client it is, and which brainsrv Context it talks to.
//
// The file lives at $AFFERENT_CONFIG_DIR/config.json, else
// $XDG_CONFIG_HOME/afferent/config.json, else ~/.config/afferent/config.json
// (the same path on macOS and Linux). The directory is 0700 and the file 0600.
// Precedence, lowest first: built-in defaults, the file, AFFERENT_* env vars,
// then command-line flags (applied by the caller).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Local-dev defaults (vero-local docker).
const (
	DefaultIssuer      = "http://authsrv.vero.localhost:8801"
	DefaultClientID    = "afferent-cli"
	DefaultBrainsrvURL = "http://localhost:18077"
	DefaultContext     = "afferent-poc"
)

// Environment variables that override the file.
const (
	EnvConfigDir     = "AFFERENT_CONFIG_DIR"
	EnvIssuer        = "AFFERENT_ISSUER"
	EnvIssuerDial    = "AFFERENT_ISSUER_DIAL"
	EnvClientID      = "AFFERENT_CLIENT_ID"
	EnvTokenEndpoint = "AFFERENT_TOKEN_ENDPOINT"
	EnvBrainsrvURL   = "AFFERENT_BRAINSRV_URL"
	EnvContext       = "AFFERENT_CONTEXT"
)

// Config is the on-disk shape of config.json.
type Config struct {
	// Issuer is the authsrv issuer identifier. Tokens' iss must match what
	// discovery at this issuer reports (see auth.Discover for the loopback
	// alias rule).
	Issuer string `json:"issuer"`
	// IssuerDial, when set, is the base URL the CLI actually connects to for
	// the issuer. Use it when the issuer's hostname does not resolve from
	// this machine; issuer-relative endpoints from discovery are rewritten
	// onto it. Empty means dial the issuer itself.
	IssuerDial string `json:"issuer_dial,omitempty"`
	// ClientID is the public OAuth client (default afferent-cli).
	ClientID string `json:"client_id"`
	// TokenEndpoint overrides the endpoint used for device-code polling and
	// refresh. Empty means derive it (see auth.Discover).
	TokenEndpoint string `json:"token_endpoint,omitempty"`
	// BrainsrvURL is the brainsrv base URL.
	BrainsrvURL string `json:"brainsrv_url"`
	// Context is the brainsrv Context slug sent as X-Context.
	Context string `json:"context"`
}

// Defaults returns the built-in local-dev configuration.
func Defaults() Config {
	return Config{
		Issuer:      DefaultIssuer,
		ClientID:    DefaultClientID,
		BrainsrvURL: DefaultBrainsrvURL,
		Context:     DefaultContext,
	}
}

// Dir returns the afferent config directory.
func Dir() (string, error) {
	if d := os.Getenv(EnvConfigDir); d != "" {
		return d, nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "afferent"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "afferent"), nil
}

// Path returns the config.json path inside dir.
func Path(dir string) string { return filepath.Join(dir, "config.json") }

// Load reads dir/config.json over the defaults. A missing file is not an
// error. Env overrides are not applied; call ApplyEnv.
func Load(dir string) (Config, error) {
	cfg := Defaults()
	b, err := os.ReadFile(Path(dir))
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	var file Config
	if err := json.Unmarshal(b, &file); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", Path(dir), err)
	}
	cfg.merge(file)
	return cfg, nil
}

// ApplyEnv overlays AFFERENT_* environment variables.
func (c *Config) ApplyEnv(getenv func(string) string) {
	if getenv == nil {
		getenv = os.Getenv
	}
	c.merge(Config{
		Issuer:        getenv(EnvIssuer),
		IssuerDial:    getenv(EnvIssuerDial),
		ClientID:      getenv(EnvClientID),
		TokenEndpoint: getenv(EnvTokenEndpoint),
		BrainsrvURL:   getenv(EnvBrainsrvURL),
		Context:       getenv(EnvContext),
	})
}

// merge copies every non-empty field of o onto c.
func (c *Config) merge(o Config) {
	set := func(dst *string, v string) {
		if v = strings.TrimSpace(v); v != "" {
			*dst = v
		}
	}
	set(&c.Issuer, o.Issuer)
	set(&c.IssuerDial, o.IssuerDial)
	set(&c.ClientID, o.ClientID)
	set(&c.TokenEndpoint, o.TokenEndpoint)
	set(&c.BrainsrvURL, o.BrainsrvURL)
	set(&c.Context, o.Context)
}

// Normalize trims trailing slashes from the URLs.
func (c *Config) Normalize() {
	c.Issuer = strings.TrimRight(c.Issuer, "/")
	c.IssuerDial = strings.TrimRight(c.IssuerDial, "/")
	c.BrainsrvURL = strings.TrimRight(c.BrainsrvURL, "/")
}

// Validate checks every URL (https, or http on a loopback host) and that the
// required fields are present.
func (c Config) Validate() error {
	if c.ClientID == "" {
		return errors.New("client_id is empty")
	}
	if err := CheckURL("issuer", c.Issuer); err != nil {
		return err
	}
	if c.IssuerDial != "" {
		if err := CheckURL("issuer_dial", c.IssuerDial); err != nil {
			return err
		}
	}
	if c.TokenEndpoint != "" {
		if err := CheckURL("token_endpoint", c.TokenEndpoint); err != nil {
			return err
		}
	}
	if err := CheckURL("brainsrv_url", c.BrainsrvURL); err != nil {
		return err
	}
	return nil
}

// Save writes cfg to dir/config.json atomically, creating dir 0700 and the
// file 0600.
func Save(dir string, cfg Config) error {
	if err := EnsureDir(dir); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(Path(dir), append(b, '\n'), 0o600)
}

// EnsureDir creates dir with mode 0700 and tightens an existing one that is
// group- or world-accessible. It refuses a symlinked dir.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; afferent needs a real directory", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("tighten %s to 0700: %w", dir, err)
		}
	}
	return nil
}

// WriteFileAtomic writes data to a temp file in path's directory with perm
// and renames it over path, so readers never see a partial file and an
// existing symlink at path is replaced rather than followed.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// CheckURL requires an absolute http(s) URL with a host; plain http is only
// allowed for loopback hosts.
func CheckURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s %q: %w", name, raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("%s %q: not an absolute URL", name, raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if IsLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%s %q: https is required for non-loopback hosts", name, raw)
	default:
		return fmt.Errorf("%s %q: scheme must be https (or http on loopback)", name, raw)
	}
}

// IsLoopbackHost reports whether host is localhost, a *.localhost name
// (RFC 6761 reserves these for loopback), or a loopback IP literal.
func IsLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}
