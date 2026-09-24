package brainsrvcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type labelCase struct {
	In   string `json:"in"`
	Kind string `json:"kind"`
	Out  string `json:"out"`
}

const labelsVector = "testdata/labels.json"

func TestLabelVector(t *testing.T) {
	raw, err := os.ReadFile(labelsVector)
	if err != nil {
		t.Fatal(err)
	}
	var cases []labelCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 40 {
		t.Fatalf("vector has %d cases, want >= 40", len(cases))
	}
	pinned := map[string]bool{}
	for _, c := range cases {
		var got string
		switch c.Kind {
		case "repo":
			got = RepoLabel(c.In)
		case "harness":
			pinned[c.In] = true
			got = HarnessLabel(c.In)
		default:
			t.Fatalf("case %q: unknown kind %q", c.In, c.Kind)
		}
		if got != c.Out {
			t.Errorf("%s(%q) = %q, want %q", c.Kind, c.In, got, c.Out)
		}
		if !ValidScope(got) || strings.Contains(got, ".") || len(got) > maxLabel {
			t.Errorf("%s(%q) = %q is not a single valid scope label", c.Kind, c.In, got)
		}
	}
	for k := range HarnessLabels {
		if !pinned[k] {
			t.Errorf("HarnessLabels[%q] has no harness case in %s", k, labelsVector)
		}
	}
}

// canonicalLabelsPath finds brainsrv's canonical vector: BRAINSRV_LABELS_PATH
// when set, else ../brainsrv next to the afferent checkout.
func canonicalLabelsPath(t *testing.T) string {
	if p := strings.TrimSpace(os.Getenv("BRAINSRV_LABELS_PATH")); p != "" {
		return p
	}
	// cli/beacon/internal/brainsrvcfg → repo root is four levels up.
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(filepath.Dir(root), "brainsrv", "testdata", "beacon", "labels.json")
}

// TestLabelVectorMatchesBrainsrv is `make check-labels` as a test (the
// Makefile is upstream's and stays untouched): the afferent copy must be
// byte-identical to brainsrv's canonical vector. Skipped when brainsrv's
// copy is not present (CI, or a brainsrv checkout that predates it).
func TestLabelVectorMatchesBrainsrv(t *testing.T) {
	canonical := canonicalLabelsPath(t)
	want, err := os.ReadFile(canonical)
	if err != nil {
		if os.Getenv("BRAINSRV_LABELS_PATH") != "" {
			t.Fatalf("BRAINSRV_LABELS_PATH: %v", err)
		}
		t.Skipf("canonical vector not found at %s (set BRAINSRV_LABELS_PATH)", canonical)
	}
	got, err := os.ReadFile(labelsVector)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from brainsrv's %s; copy it verbatim", labelsVector, canonical)
	}
}

func TestLabelLossyDistinct(t *testing.T) {
	pairs := [][2]string{{"My.Repo", "my_repo"}, {"my-repo", "my_repo"}, {"a__b", "a_b"}, {"פרויקט", "日本語"}}
	for _, p := range pairs {
		if a, b := Label(p[0]), Label(p[1]); a == b {
			t.Errorf("Label(%q) == Label(%q) == %q", p[0], p[1], a)
		}
	}
}

func FuzzLabelAlwaysValid(f *testing.F) {
	for _, s := range []string{"", "x", "git@h:a/b.git", "https://h/a/b/", "פרויקט", strings.Repeat("ab-", 50)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		l := Label(s)
		if !ValidScope(l) || strings.Contains(l, ".") || len(l) > maxLabel {
			t.Fatalf("Label(%q) = %q invalid", s, l)
		}
	})
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad(t *testing.T) {
	good := map[string]string{
		EnvBackend: "brainsrv",
		EnvURL:     "https://brain.example.com/",
		EnvScope:   "ws.dev.people.m.harness",
		EnvKeyFile: "/tmp/k",
	}
	cfg, err := Load(envOf(good))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "https://brain.example.com" || cfg.Scope != "ws.dev.people.m.harness" || cfg.KeyFile != "/tmp/k" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if got := cfg.ScopeFor("afferent"); got != "ws.dev.people.m.harness.afferent" {
		t.Fatalf("ScopeFor = %q", got)
	}

	if _, err := Load(envOf(nil)); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unset backend: err = %v", err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"other backend": func(m map[string]string) { m[EnvBackend] = "s3" },
		"no url":        func(m map[string]string) { m[EnvURL] = "" },
		"plain http":    func(m map[string]string) { m[EnvURL] = "http://brain.example.com" },
		"ftp":           func(m map[string]string) { m[EnvURL] = "ftp://brain.example.com" },
		"userinfo":      func(m map[string]string) { m[EnvURL] = "https://u:p@brain.example.com" },
		"bad scope":     func(m map[string]string) { m[EnvScope] = "WS.Dev" },
		"empty scope":   func(m map[string]string) { m[EnvScope] = "" },
		"dotted end":    func(m map[string]string) { m[EnvScope] = "ws.dev." },
		"no key file":   func(m map[string]string) { m[EnvKeyFile] = "" },
	} {
		m := map[string]string{}
		for k, v := range good {
			m[k] = v
		}
		mutate(m)
		if _, err := Load(envOf(m)); err == nil || errors.Is(err, ErrNotConfigured) {
			t.Errorf("%s: err = %v, want a config error", name, err)
		}
	}
	for _, u := range []string{"http://localhost:8077", "http://127.0.0.1:8077/", "http://[::1]:8077"} {
		if _, err := ValidateURL(u); err != nil {
			t.Errorf("ValidateURL(%q) = %v, want loopback http allowed", u, err)
		}
	}
}

func TestReadKeyFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good.key", "spk_abc123\n", 0o600)
	key, err := ReadKeyFile(good)
	if err != nil || key != "spk_abc123" {
		t.Fatalf("ReadKeyFile = %q, %v", key, err)
	}
	if _, err := ReadKeyFile(write("noprefix.key", "abc123", 0o600)); err == nil {
		t.Error("key without spk_ prefix accepted")
	}
	if _, err := ReadKeyFile(write("bare.key", "spk_", 0o600)); err == nil {
		t.Error("bare prefix accepted")
	}
	if _, err := ReadKeyFile(write("two.key", "spk_a\nspk_b", 0o600)); err == nil {
		t.Error("two keys accepted")
	}
	if _, err := ReadKeyFile(filepath.Join(dir, "missing.key")); err == nil {
		t.Error("missing file accepted")
	}
	if _, err := ReadKeyFile(dir); err == nil {
		t.Error("directory accepted")
	}
	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(good, link); err == nil {
		if _, err := ReadKeyFile(link); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("symlink: err = %v", err)
		}
	}
	if runtime.GOOS != "windows" {
		for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660} {
			p := write("loose.key", "spk_abc", mode)
			if _, err := ReadKeyFile(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
				t.Errorf("mode %#o: err = %v", mode, err)
			}
		}
		if _, err := ReadKeyFile(write("owner-only.key", "spk_abc", 0o400)); err != nil {
			t.Errorf("0400: %v", err)
		}
	}
}

func TestConfigCovers(t *testing.T) {
	c := Config{Scope: "ws.dev.people.m.harness"}
	for scope, want := range map[string]bool{
		"ws.dev.people.m.harness":            true,
		"ws.dev.people.m.harness.repo":       true,
		"ws.dev.people.m.harness.repo.codex": true,
		"ws.dev.people.m.harnessx":           false,
		"ws.dev.people.m":                    false,
		"ws.dev.people.other.harness":        false,
		"ws.dev.people.m.harness..repo":      false,
		"ws.dev.people.m.harness.Repo":       false,
		"ws.dev.people.m.harness.":           false,
		"":                                   false,
	} {
		if got := c.Covers(scope); got != want {
			t.Errorf("Covers(%q) = %v, want %v", scope, got, want)
		}
	}
	if (Config{}).Covers("ws") {
		t.Error("an empty base scope must cover nothing")
	}
}
