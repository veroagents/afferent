package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsEnvAndSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "afferent")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != Defaults() {
		t.Fatalf("missing file should give defaults, got %+v", cfg)
	}
	cfg.Context = "from-file"
	if err := Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(Path(dir))
	di, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes file %v dir %v", fi.Mode().Perm(), di.Mode().Perm())
	}
	cfg, err = Load(dir)
	if err != nil || cfg.Context != "from-file" {
		t.Fatalf("%v %+v", err, cfg)
	}
	env := map[string]string{EnvContext: "from-env", EnvIssuer: "https://auth.example.com/"}
	cfg.ApplyEnv(func(k string) string { return env[k] })
	cfg.Normalize()
	if cfg.Context != "from-env" || cfg.Issuer != "https://auth.example.com" || cfg.ClientID != DefaultClientID {
		t.Fatalf("env overlay %+v", cfg)
	}
}

func TestCheckURL(t *testing.T) {
	ok := []string{"https://auth.example.com", "http://localhost:8801", "http://authsrv.vero.localhost:8801", "http://127.0.0.1:1", "http://[::1]:2"}
	for _, u := range ok {
		if err := CheckURL("u", u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := []string{"http://auth.example.com", "ftp://localhost", "localhost:8801", "", "http://localhost.evil.com"}
	for _, u := range bad {
		if err := CheckURL("u", u); err == nil {
			t.Errorf("%s: want error", u)
		}
	}
}

func TestDirHonorsEnv(t *testing.T) {
	t.Setenv(EnvConfigDir, "/tmp/x")
	if d, _ := Dir(); d != "/tmp/x" {
		t.Fatal(d)
	}
	t.Setenv(EnvConfigDir, "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if d, _ := Dir(); d != "/xdg/afferent" {
		t.Fatal(d)
	}
}
