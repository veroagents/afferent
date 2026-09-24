// Package brainsrvcfg holds the afferent ↔ brainsrv configuration shared by
// the memory backend (B1) and the forwarder (B3): environment config, the
// API key file reader and the scope-label sanitizer (PLAN §1, §4 Phase 2).
package brainsrvcfg

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Environment variables that configure the brainsrv memory backend.
const (
	EnvBackend = "BEACON_MEMORY_BACKEND"
	EnvURL     = "BEACON_BRAINSRV_URL"
	EnvScope   = "BEACON_BRAINSRV_SCOPE"
	EnvKeyFile = "BEACON_BRAINSRV_KEY_FILE"

	// BackendBrainsrv is the only BEACON_MEMORY_BACKEND value understood.
	BackendBrainsrv = "brainsrv"

	// KeyPrefix is the prefix every brainsrv API key carries.
	KeyPrefix = "spk_"
)

// ErrNotConfigured reports that BEACON_MEMORY_BACKEND is unset: the caller
// keeps upstream behaviour (local SQLite only).
var ErrNotConfigured = errors.New("brainsrv memory backend not configured")

// scopeRe is brainsrv's ltree scope rule (authz.ValidScope).
var scopeRe = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*$`)

// ValidScope reports whether s is a valid brainsrv scope path.
func ValidScope(s string) bool { return scopeRe.MatchString(s) }

// Config is a validated brainsrv backend configuration.
type Config struct {
	// URL is the brainsrv base URL without a trailing slash.
	URL string
	// Scope is the member's base scope, e.g. ws.<id>.people.<m>.harness.
	Scope string
	// KeyFile is the path of the file holding the member's spk_ key.
	KeyFile string
}

// FromEnv loads the configuration from the process environment.
func FromEnv() (Config, error) { return Load(os.Getenv) }

// Load builds a Config from getenv. It returns ErrNotConfigured when
// BEACON_MEMORY_BACKEND is unset or blank, and a descriptive error when the
// backend is selected but the rest of the configuration is unusable. It does
// not read the key file; see ReadKeyFile.
func Load(getenv func(string) string) (Config, error) {
	backend := strings.TrimSpace(getenv(EnvBackend))
	if backend == "" {
		return Config{}, ErrNotConfigured
	}
	if !strings.EqualFold(backend, BackendBrainsrv) {
		return Config{}, fmt.Errorf("%s=%q is not supported (only %q)", EnvBackend, backend, BackendBrainsrv)
	}
	base, err := ValidateURL(getenv(EnvURL))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", EnvURL, err)
	}
	scope := strings.TrimSpace(getenv(EnvScope))
	if !ValidScope(scope) {
		return Config{}, fmt.Errorf("%s=%q is not a valid brainsrv scope (want %s)", EnvScope, scope, scopeRe.String())
	}
	keyFile := strings.TrimSpace(getenv(EnvKeyFile))
	if keyFile == "" {
		return Config{}, fmt.Errorf("%s is required", EnvKeyFile)
	}
	return Config{URL: base, Scope: scope, KeyFile: keyFile}, nil
}

// ValidateURL checks a brainsrv base URL: https is required, except for
// plain http to a loopback host (localhost, 127.0.0.1, ::1). It returns the
// URL without a trailing slash.
func ValidateURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("URL %q has no host", raw)
	}
	if u.User != nil {
		return "", errors.New("URL must not carry credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("URL must not carry a query or fragment")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return "", fmt.Errorf("URL %q must use https (http is only allowed for localhost)", raw)
		}
	default:
		return "", fmt.Errorf("URL %q must use https", raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}

// ScopeFor returns <base>.<label>. An invalid label yields the base scope.
func (c Config) ScopeFor(label string) string {
	if label == "" || !ValidScope(label) {
		return c.Scope
	}
	return c.Scope + "." + label
}

// Covers reports whether scope is the base scope or sits under it. A scope
// that is not a valid brainsrv scope is never covered, so "base.x" matches
// but "basex" and "base..x" do not.
func (c Config) Covers(scope string) bool {
	if c.Scope == "" || !ValidScope(scope) {
		return false
	}
	return scope == c.Scope || strings.HasPrefix(scope, c.Scope+".")
}

// ReadKeyFile reads a brainsrv API key from path. The file must be a regular
// file (symlinks are rejected, checked with Lstat), and on Unix it must be
// owned by the current user and not readable or writable by group or others
// (perm & 0o077 == 0). The trimmed content must be one spk_ key.
func ReadKeyFile(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("key file path is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("key file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("key file %s is a symlink; point at the file itself", path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("key file %s is not a regular file", path)
	}
	if err := checkKeyFileOwner(path, info); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("key file: %w", err)
	}
	key := strings.TrimSpace(string(data))
	if !strings.HasPrefix(key, KeyPrefix) || len(key) == len(KeyPrefix) {
		return "", fmt.Errorf("key file %s does not hold a brainsrv key (want %s…)", path, KeyPrefix)
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return "", fmt.Errorf("key file %s must hold exactly one key", path)
	}
	return key, nil
}
