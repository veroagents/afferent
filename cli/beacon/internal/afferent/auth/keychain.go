package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// KeychainService is the generic-password service name for afferent items.
const KeychainService = "afferent"

// SecurityPath is the macOS security(1) tool.
const SecurityPath = "/usr/bin/security"

// keychainLineMax is the longest command line security -i reliably accepts.
// Its interactive reader splits lines at about 4 KiB; measured on macOS 26 a
// 3,980-byte line works and a ~8 KiB one is split into bogus commands.
const keychainLineMax = 3900

// securityNotFound is security(1)'s exit status for a missing item
// (errSecItemNotFound).
const securityNotFound = 44

// SecurityRunner runs /usr/bin/security. Tests substitute a fake so they
// never touch the real Keychain.
type SecurityRunner interface {
	// Run executes security with args, feeding stdin (may be nil). It returns
	// ErrNotFound when security exits 44 (item not found).
	Run(ctx context.Context, stdin []byte, args ...string) ([]byte, error)
}

// ExecSecurity runs the real /usr/bin/security.
type ExecSecurity struct{}

func (ExecSecurity) Run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, SecurityPath, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == securityNotFound {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("security %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// KeychainStore keeps credentials as one generic password in the login
// Keychain: service "afferent", account "<issuer>|<client_id>", and the
// password is base64(JSON credentials).
//
// Argv exposure. `security add-generic-password -w <secret>` would put the
// secret in the process table, visible to every local user via ps. Instead
// the CLI runs `security -i` and writes the add command on its stdin, so the
// secret travels only through a pipe. -X (hex) would double the size and
// security -i splits lines at ~4 KiB, so the secret is base64 (no quotes or
// spaces for the interactive tokenizer to trip on). A credential set too
// large for one line is refused here and the FallbackStore puts it in the
// 0600 file instead. security -i exits 0 even when the command fails, so
// Save reads the item back to confirm it.
//
// Reads use `find-generic-password -w`, which prints the secret on stdout
// (a pipe to this process), never argv.
type KeychainStore struct {
	Account string
	Runner  SecurityRunner
	Timeout time.Duration // per security call; 0 means 2 minutes (it may show an unlock prompt)
}

// KeychainAccount is the account string for an issuer and client.
func KeychainAccount(issuer, clientID string) string { return issuer + "|" + clientID }

func (k *KeychainStore) Name() string { return "macOS Keychain" }

func (k *KeychainStore) ctx() (context.Context, context.CancelFunc) {
	t := k.Timeout
	if t <= 0 {
		t = 2 * time.Minute
	}
	return context.WithTimeout(context.Background(), t)
}

func (k *KeychainStore) runner() SecurityRunner {
	if k.Runner == nil {
		return ExecSecurity{}
	}
	return k.Runner
}

func (k *KeychainStore) Load() (*Credentials, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	out, err := k.runner().Run(ctx, nil, "find-generic-password", "-s", KeychainService, "-a", k.Account, "-w")
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, errors.New("keychain item is not afferent credentials (bad base64)")
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, errors.New("keychain item is not afferent credentials (bad JSON)")
	}
	return &c, nil
}

func (k *KeychainStore) Save(c *Credentials) error {
	if strings.ContainsAny(k.Account, "\"\\\r\n") {
		return fmt.Errorf("keychain account %q contains characters security -i cannot quote", k.Account)
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	secret := base64.StdEncoding.EncodeToString(b)
	line := fmt.Sprintf("add-generic-password -U -s \"%s\" -a \"%s\" -w %s\n", KeychainService, k.Account, secret)
	if len(line) > keychainLineMax {
		return fmt.Errorf("credentials are %d bytes, too large for security -i", len(b))
	}
	ctx, cancel := k.ctx()
	defer cancel()
	if _, err := k.runner().Run(ctx, []byte(line), "-i"); err != nil {
		return err
	}
	got, err := k.Load()
	if err != nil {
		return fmt.Errorf("keychain write did not stick: %w", err)
	}
	if got.AccessToken != c.AccessToken || got.RefreshToken != c.RefreshToken {
		return errors.New("keychain write did not stick: read-back differs")
	}
	return nil
}

func (k *KeychainStore) Delete() error {
	ctx, cancel := k.ctx()
	defer cancel()
	_, err := k.runner().Run(ctx, nil, "delete-generic-password", "-s", KeychainService, "-a", k.Account)
	return err
}
