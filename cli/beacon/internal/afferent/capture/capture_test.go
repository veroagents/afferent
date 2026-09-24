package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Status only reads; with a temp HOME it never sees the real agent configs.
func TestHooksStatusOnTempHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	for _, h := range Harnesses {
		installed, path, err := Hooks{}.Status(h)
		if err != nil {
			t.Fatalf("%s: %v", h, err)
		}
		if installed {
			t.Fatalf("%s reported installed on an empty home", h)
		}
		if path != "" && !strings.HasPrefix(path, home) {
			t.Fatalf("%s settings path %s is outside the temp home", h, path)
		}
	}
	if _, _, err := (Hooks{}).Status("vim"); err == nil {
		t.Fatal("unsupported harness accepted")
	}
	if _, err := (Hooks{}).Install("vim", filepath.Join(home, "log"), true); err == nil {
		t.Fatal("unsupported harness accepted")
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("Status wrote to the home: %v", entries)
	}
}
