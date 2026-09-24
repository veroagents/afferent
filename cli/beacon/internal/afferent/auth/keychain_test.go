package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSecurity emulates the subset of /usr/bin/security the store uses.
type fakeSecurity struct {
	mu    sync.Mutex
	items map[string]string // service|account -> password
	argvs [][]string
	fail  error // returned by every call when set
	// failAdd / failDelete fail only writes / deletes.
	failAdd, failDelete error
}

func newFakeSecurity() *fakeSecurity { return &fakeSecurity{items: map[string]string{}} }

func (f *fakeSecurity) Run(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.argvs = append(f.argvs, append([]string(nil), args...))
	if f.fail != nil {
		return nil, f.fail
	}
	if len(args) == 1 && args[0] == "-i" {
		if f.failAdd != nil {
			return nil, f.failAdd
		}
		// Parse "add-generic-password -U -s "svc" -a "acct" -w secret".
		fields := strings.Fields(strings.TrimSpace(string(stdin)))
		if len(fields) == 0 || fields[0] != "add-generic-password" {
			return nil, errors.New("fake: unexpected interactive command")
		}
		kv := map[string]string{}
		for i := 1; i < len(fields); i++ {
			if fields[i] == "-U" {
				continue
			}
			if i+1 < len(fields) {
				kv[fields[i]] = strings.Trim(fields[i+1], `"`)
				i++
			}
		}
		f.items[kv["-s"]+"|"+kv["-a"]] = kv["-w"]
		return nil, nil
	}
	flag := func(name string) string {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == name {
				return args[i+1]
			}
		}
		return ""
	}
	key := flag("-s") + "|" + flag("-a")
	switch args[0] {
	case "find-generic-password":
		v, ok := f.items[key]
		if !ok {
			return nil, ErrNotFound
		}
		return []byte(v + "\n"), nil
	case "delete-generic-password":
		if f.failDelete != nil {
			return nil, f.failDelete
		}
		if _, ok := f.items[key]; !ok {
			return nil, ErrNotFound
		}
		delete(f.items, key)
		return nil, nil
	}
	return nil, errors.New("fake: unexpected command " + args[0])
}

func TestKeychainStoreRoundTripKeepsSecretOffArgv(t *testing.T) {
	fake := newFakeSecurity()
	ks := &KeychainStore{Account: KeychainAccount("http://authsrv.vero.localhost:8801", "afferent-cli"), Runner: fake}
	in := &Credentials{AccessToken: "secret-access", RefreshToken: "secret-refresh", Issuer: "i", ClientID: "c"}
	if err := ks.Save(in); err != nil {
		t.Fatal(err)
	}
	got, err := ks.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != in.AccessToken || got.RefreshToken != in.RefreshToken {
		t.Fatalf("round trip %+v", got)
	}
	for _, argv := range fake.argvs {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "secret") || strings.Contains(joined, "-w ") && argv[0] == "add-generic-password" {
			t.Fatalf("secret material on argv: %v", argv)
		}
	}
	if err := ks.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := ks.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := ks.Delete(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestKeychainStoreRefusesOversizedLine(t *testing.T) {
	ks := &KeychainStore{Account: "a|b", Runner: newFakeSecurity()}
	err := ks.Save(&Credentials{AccessToken: strings.Repeat("x", 4000)})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("got %v", err)
	}
}

func TestFallbackStoreUsesFileWhenKeychainFails(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeSecurity()
	fake.fail = errors.New("security: user interaction is not allowed")
	file := &FileStore{Path: filepath.Join(dir, "credentials.json")}
	var warned []error
	s := &FallbackStore{Primary: &KeychainStore{Account: "a|b", Runner: fake}, Secondary: file, Warn: func(e error) { warned = append(warned, e) }}
	if err := s.Save(&Credentials{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	if len(warned) == 0 {
		t.Fatal("no warning about the keychain failure")
	}
	if c, err := file.Load(); err != nil || c.RefreshToken != "r" {
		t.Fatalf("file fallback: %v %+v", err, c)
	}
	if c, err := s.Load(); err != nil || c.RefreshToken != "r" {
		t.Fatalf("fallback load: %v %+v", err, c)
	}
	if err := s.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestFallbackStorePrefersKeychainAndRemovesStaleFile(t *testing.T) {
	dir := t.TempDir()
	file := &FileStore{Path: filepath.Join(dir, "credentials.json")}
	if err := file.Save(&Credentials{AccessToken: "old"}); err != nil {
		t.Fatal(err)
	}
	s := &FallbackStore{Primary: &KeychainStore{Account: "a|b", Runner: newFakeSecurity()}, Secondary: file}
	if err := s.Save(&Credentials{AccessToken: "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale file kept: %v", err)
	}
	if c, err := s.Load(); err != nil || c.AccessToken != "new" {
		t.Fatalf("%v %+v", err, c)
	}
}

// A failed Keychain write must not leave an older Keychain item shadowing the
// newer credentials that went to the file.
func TestFallbackStoreKeychainWriteFailureDoesNotResurrectStaleItem(t *testing.T) {
	now := time.Now()
	stale := &Credentials{AccessToken: "old-at", RefreshToken: "spent-rt", Expiry: now.Add(-time.Minute)}
	fresh := &Credentials{AccessToken: "new-at", RefreshToken: "new-rt", Expiry: now.Add(15 * time.Minute)}

	for _, tc := range []struct {
		name       string
		failDelete error
	}{
		{"stale item removed", nil},
		{"stale item cannot be removed", errors.New("security: delete failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeSecurity()
			ks := &KeychainStore{Account: "a|b", Runner: fake}
			if err := ks.Save(stale); err != nil {
				t.Fatal(err)
			}
			fake.failAdd = errors.New("security: write failed")
			fake.failDelete = tc.failDelete
			file := &FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}
			s := &FallbackStore{Primary: ks, Secondary: file}
			if err := s.Save(fresh); err != nil {
				t.Fatal(err)
			}
			c, err := s.Load()
			if err != nil || c.RefreshToken != "new-rt" {
				t.Fatalf("load returned stale credentials: %v %+v", err, c)
			}
			if tc.failDelete == nil {
				if _, err := ks.Load(); !errors.Is(err, ErrNotFound) {
					t.Fatalf("stale keychain item kept: %v", err)
				}
			}
		})
	}
}
