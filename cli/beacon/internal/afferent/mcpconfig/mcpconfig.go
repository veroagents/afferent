// Package mcpconfig registers `afferent mcp proxy` as an MCP server named
// "brain" in each coding agent's user-level configuration (PLAN D7):
//
//   - Claude Code: ~/.claude.json "mcpServers" (user scope). When the claude
//     CLI is on PATH it does the write (`claude mcp add --scope user brain --
//     <afferent> mcp proxy`), since Claude Code rewrites that file itself;
//     otherwise the one member is edited in place.
//   - Cursor: ~/.cursor/mcp.json "mcpServers", edited in place.
//   - Codex: ~/.codex/config.toml, a delimited [mcp_servers.brain] block that
//     is appended or replaced; nothing outside the block is touched.
//
// Upstream Beacon has no MCP-config writer to reuse (it only prints a
// snippet in `beacon mcp`), so these are new.
//
// Every edit is idempotent (an identical entry is left alone), copies the
// file to <file>.afferent.bak before changing it, is written atomically with
// the file's mode, and is validated before it is written. DryRun computes
// the same result and a diff without writing or running anything.
package mcpconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
)

// ServerName is the MCP server name agents see.
const ServerName = "brain"

// Harnesses supported, in the order they are configured.
const (
	Claude = "claude"
	Cursor = "cursor"
	Codex  = "codex"
)

// All lists every supported harness.
var All = []string{Claude, Cursor, Codex}

// BackupSuffix is appended to a file's name for the copy kept before an edit.
const BackupSuffix = ".afferent.bak"

// Entry is the stdio server to register.
type Entry struct {
	Command string
	Args    []string
}

// Runner runs a command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Options for Apply.
type Options struct {
	Home     string
	LookPath func(string) (string, error) // nil: exec.LookPath
	Run      Runner                       // nil: os/exec
	DryRun   bool
	Remove   bool
}

// Result says what Apply did (or, with DryRun, would do).
type Result struct {
	Harness string
	Path    string
	// Action is added, updated, removed, unchanged or absent.
	Action string
	// Via is "claude CLI" or "file".
	Via     string
	Command string // the claude CLI command, when Via is the CLI
	Backup  string
	Diff    string
	Note    string
}

// Changed reports whether the file was (or would be) changed.
func (r Result) Changed() bool {
	return r.Action == "added" || r.Action == "updated" || r.Action == "removed"
}

func (o Options) lookPath(name string) (string, error) {
	if o.LookPath != nil {
		return o.LookPath(name)
	}
	return exec.LookPath(name)
}

func (o Options) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if o.Run != nil {
		return o.Run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Path returns the config file for harness h under home.
func Path(home, h string) string {
	switch h {
	case Claude:
		return filepath.Join(home, ".claude.json")
	case Cursor:
		return filepath.Join(home, ".cursor", "mcp.json")
	case Codex:
		return filepath.Join(home, ".codex", "config.toml")
	}
	return ""
}

// Detect lists the harnesses that look installed for this user: their
// config directory exists, or (Claude, Codex) their CLI is on PATH.
func Detect(o Options) []string {
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	var out []string
	for _, h := range All {
		var found bool
		switch h {
		case Claude:
			found = exists(filepath.Join(o.Home, ".claude")) || exists(filepath.Join(o.Home, ".claude.json"))
			if !found {
				_, err := o.lookPath("claude")
				found = err == nil
			}
		case Cursor:
			found = exists(filepath.Join(o.Home, ".cursor"))
		case Codex:
			found = exists(filepath.Join(o.Home, ".codex"))
			if !found {
				_, err := o.lookPath("codex")
				found = err == nil
			}
		}
		if found {
			out = append(out, h)
		}
	}
	return out
}

// ParseHarnesses turns "claude,codex", "all" or "auto" into a list.
func ParseHarnesses(v string, o Options) ([]string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "", "auto":
		return Detect(o), nil
	case "all":
		return append([]string(nil), All...), nil
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(v, ",") {
		h := strings.TrimSpace(part)
		switch h {
		case "claude-code", "claude_code":
			h = Claude
		case "codex-cli", "codex_cli":
			h = Codex
		}
		if h == "" || seen[h] {
			continue
		}
		if Path("/", h) == "" {
			return nil, fmt.Errorf("unsupported harness %q (supported: %s, all, auto)", part, strings.Join(All, ", "))
		}
		seen[h] = true
		out = append(out, h)
	}
	return out, nil
}

// Apply registers (or with Remove, unregisters) e for harness h.
func Apply(ctx context.Context, h string, e Entry, o Options) (Result, error) {
	switch h {
	case Claude:
		return applyClaude(ctx, e, o)
	case Cursor:
		return applyJSON(Cursor, Path(o.Home, Cursor), e, map[string]any{"command": e.Command, "args": e.Args}, o)
	case Codex:
		return applyCodex(e, o)
	}
	return Result{}, fmt.Errorf("unsupported harness %q", h)
}

// keep lists the values afferent writes, which a diff may show.
func (e Entry) keep() []string {
	return append([]string{e.Command, "stdio", "mcpServers", "mcp_servers"}, e.Args...)
}

var jsonPath = []string{"mcpServers", ServerName}

func claudeValue(e Entry) map[string]any {
	return map[string]any{"type": "stdio", "command": e.Command, "args": e.Args, "env": map[string]any{}}
}

// sameServer compares an existing entry with e on what matters (command
// and args; a stdio type if one is given).
func sameServer(raw json.RawMessage, e Entry) bool {
	var cur struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if json.Unmarshal(raw, &cur) != nil {
		return false
	}
	if cur.Type != "" && cur.Type != "stdio" {
		return false
	}
	args := cur.Args
	if args == nil {
		args = []string{}
	}
	want := e.Args
	if want == nil {
		want = []string{}
	}
	return cur.Command == e.Command && reflect.DeepEqual(args, want)
}

// readFile returns the file's bytes and mode (nil and 0600 when absent).
func readFile(path string) ([]byte, fs.FileMode, bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0o600, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("%s is not a regular file (a symlink?); edit it by hand", path)
	}
	b, err := os.ReadFile(path)
	return b, fi.Mode().Perm(), true, err
}

// write backs up the old content (when the file existed) and writes the new
// content atomically with the old mode.
func write(path string, before, after []byte, mode fs.FileMode, existed bool) (string, error) {
	backup := ""
	if existed {
		backup = path + BackupSuffix
		if err := config.WriteFileAtomic(backup, before, mode); err != nil {
			return "", fmt.Errorf("back up %s: %w", path, err)
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return backup, config.WriteFileAtomic(path, after, mode)
}

// planJSON computes the edited document and the action.
func planJSON(before []byte, e Entry, val any, remove bool) (after []byte, action string, err error) {
	cur, found, err := getJSONPath(before, jsonPath)
	if err != nil {
		return nil, "", err
	}
	if remove {
		if !found {
			return before, "absent", nil
		}
		after, _, err = deleteJSONPath(before, jsonPath)
		return after, "removed", err
	}
	if found && sameServer(cur, e) {
		return before, "unchanged", nil
	}
	after, err = setJSONPath(before, jsonPath, val)
	if err != nil {
		return nil, "", err
	}
	if found {
		return after, "updated", nil
	}
	return after, "added", nil
}

func applyJSON(h, path string, e Entry, val any, o Options) (Result, error) {
	res := Result{Harness: h, Path: path, Via: "file"}
	before, mode, existed, err := readFile(path)
	if err != nil {
		return res, err
	}
	after, action, err := planJSON(before, e, val, o.Remove)
	if err != nil {
		return res, fmt.Errorf("%s: %w; not changed", path, err)
	}
	res.Action = action
	res.Diff = unifiedDiff(path, before, after, e.keep()...)
	if o.DryRun || !res.Changed() {
		return res, nil
	}
	res.Backup, err = write(path, before, after, mode, existed)
	return res, err
}

func applyClaude(ctx context.Context, e Entry, o Options) (Result, error) {
	path := Path(o.Home, Claude)
	before, mode, existed, err := readFile(path)
	if err != nil {
		return Result{Harness: Claude, Path: path}, err
	}
	after, action, err := planJSON(before, e, claudeValue(e), o.Remove)
	if err != nil {
		return Result{Harness: Claude, Path: path}, fmt.Errorf("%s: %w; not changed", path, err)
	}
	res := Result{Harness: Claude, Path: path, Via: "file", Action: action, Diff: unifiedDiff(path, before, after, e.keep()...)}
	cli, lookErr := o.lookPath("claude")
	if lookErr == nil {
		res.Via = "claude CLI"
		if o.Remove {
			res.Command = shellJoin(append([]string{"claude"}, claudeRemoveArgs()...))
		} else {
			res.Command = shellJoin(append([]string{"claude"}, claudeAddArgs(e)...))
		}
	}
	if o.DryRun || !res.Changed() {
		return res, nil
	}
	if existed {
		res.Backup = path + BackupSuffix
		if err := config.WriteFileAtomic(res.Backup, before, mode); err != nil {
			return res, fmt.Errorf("back up %s: %w", path, err)
		}
	}
	if lookErr == nil {
		cliErr := func() error {
			if action == "updated" || action == "removed" {
				if out, err := o.run(ctx, cli, claudeRemoveArgs()...); err != nil {
					return fmt.Errorf("claude mcp remove: %v: %s", err, strings.TrimSpace(string(out)))
				}
			}
			if action == "added" || action == "updated" {
				if out, err := o.run(ctx, cli, claudeAddArgs(e)...); err != nil {
					return fmt.Errorf("claude mcp add: %v: %s", err, strings.TrimSpace(string(out)))
				}
			}
			return nil
		}()
		if cliErr == nil {
			return res, nil
		}
		// Fall back to editing the file, from what is there now.
		res.Note = cliErr.Error() + "; edited the file instead"
		res.Via = "file"
		if before, mode, existed, err = readFile(path); err != nil {
			return res, err
		}
		if after, action, err = planJSON(before, e, claudeValue(e), o.Remove); err != nil {
			return res, fmt.Errorf("%s: %w; not changed", path, err)
		}
		res.Action = action
		if !res.Changed() {
			return res, nil
		}
	}
	if !existed {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return res, err
		}
	}
	return res, config.WriteFileAtomic(path, after, mode)
}

func claudeAddArgs(e Entry) []string {
	return append([]string{"mcp", "add", "--scope", "user", ServerName, "--", e.Command}, e.Args...)
}

func claudeRemoveArgs() []string {
	return []string{"mcp", "remove", "--scope", "user", ServerName}
}

func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`!*?[]{}()<>|&;#~") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

// ---- Codex (TOML) --------------------------------------------------------

const (
	tomlBegin = "# >>> afferent: brain MCP server (managed by `afferent mcp config`; this block is replaced) >>>"
	tomlEnd   = "# <<< afferent <<<"
)

var (
	tomlHeader     = regexp.MustCompile(`^\s*\[\s*([^\]]*?)\s*\]\s*(#.*)?$`)
	tomlBrainKey   = regexp.MustCompile(`^\s*("brain"|'brain'|brain)\s*[.=]`)
	tomlInlineRoot = regexp.MustCompile(`^\s*mcp_servers\s*[.=]`)
)

// tomlQuote is a TOML basic string.
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func codexBlock(e Entry) string {
	args := make([]string, len(e.Args))
	for i, a := range e.Args {
		args[i] = tomlQuote(a)
	}
	return tomlBegin + "\n[mcp_servers." + ServerName + "]\ncommand = " + tomlQuote(e.Command) +
		"\nargs = [" + strings.Join(args, ", ") + "]\n" + tomlEnd + "\n"
}

// splitCodex removes afferent's block and returns the rest and the block.
func splitCodex(content string) (rest, block string, err error) {
	lines := strings.SplitAfter(content, "\n")
	begin, end := -1, -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == tomlBegin && begin < 0 {
			begin = i
		} else if t == tomlEnd && begin >= 0 && end < 0 {
			end = i
		}
	}
	if begin < 0 {
		return content, "", nil
	}
	if end < 0 {
		return "", "", errors.New("found the start of afferent's block but not its end; fix the file by hand")
	}
	block = strings.Join(lines[begin:end+1], "")
	from := begin
	if from > 0 && strings.TrimSpace(lines[from-1]) == "" {
		from-- // the blank line afferent put before its block
	}
	rest = strings.Join(lines[:from], "") + strings.Join(lines[end+1:], "")
	return rest, block, nil
}

// codexConflict finds a brain server defined outside afferent's block, or
// an inline mcp_servers table a new [mcp_servers.brain] table cannot join.
func codexConflict(rest string) error {
	table := ""
	for i, l := range strings.Split(rest, "\n") {
		if m := tomlHeader.FindStringSubmatch(l); m != nil && !strings.HasPrefix(strings.TrimSpace(l), "[[") {
			table = strings.NewReplacer(" ", "", "\t", "", `"`, "", "'", "").Replace(m[1])
			if table == "mcp_servers."+ServerName || strings.HasPrefix(table, "mcp_servers."+ServerName+".") {
				return fmt.Errorf("line %d already defines [%s]; remove or rename it, then run again", i+1, m[1])
			}
			continue
		}
		if table == "" && tomlInlineRoot.MatchString(l) {
			return fmt.Errorf("line %d defines mcp_servers with a dotted key or inline table, which a [mcp_servers.%s] table cannot extend; add the server by hand", i+1, ServerName)
		}
		if table == "mcp_servers" && tomlBrainKey.MatchString(l) {
			return fmt.Errorf("line %d already defines the %q server under [mcp_servers]; remove or rename it, then run again", i+1, ServerName)
		}
	}
	return nil
}

func applyCodex(e Entry, o Options) (Result, error) {
	path := Path(o.Home, Codex)
	res := Result{Harness: Codex, Path: path, Via: "file"}
	before, mode, existed, err := readFile(path)
	if err != nil {
		return res, err
	}
	rest, block, err := splitCodex(string(before))
	if err != nil {
		return res, fmt.Errorf("%s: %w", path, err)
	}
	var after string
	switch {
	case o.Remove && block == "":
		res.Action, after = "absent", string(before)
	case o.Remove:
		res.Action, after = "removed", rest
	default:
		if err := codexConflict(rest); err != nil {
			return res, fmt.Errorf("%s: %w", path, err)
		}
		want := codexBlock(e)
		if block == want {
			res.Action, after = "unchanged", string(before)
			break
		}
		if block != "" {
			// Replace the block where it is.
			res.Action, after = "updated", strings.Replace(string(before), block, want, 1)
			break
		}
		res.Action = "added"
		after = rest
		if after != "" && !strings.HasSuffix(after, "\n") {
			after += "\n"
		}
		if after != "" {
			after += "\n"
		}
		after += want
	}
	res.Diff = unifiedDiff(path, before, []byte(after), e.keep()...)
	if o.DryRun || !res.Changed() {
		return res, nil
	}
	res.Backup, err = write(path, before, []byte(after), mode, existed)
	return res, err
}
