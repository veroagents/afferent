# The `afferent` CLI

`afferent` is the fork's own binary (PLAN v0.3, D5). It has its own command
tree and does not reuse Beacon's root command. The code is new files only:

| Path | What |
|---|---|
| `cli/beacon/cmd/afferent/main.go` | entry point |
| `cli/beacon/internal/afferent/cli` | cobra commands |
| `cli/beacon/internal/afferent/config` | `config.json`, env and flag precedence, URL rules |
| `cli/beacon/internal/afferent/auth` | discovery, device flow, token storage, refresh, revoke |
| `cli/beacon/internal/afferent/brain` | brainsrv client (`/v1/whoami`) |
| `cli/beacon/internal/afferent/afferenttest` | fake authsrv for tests |

## Build and run

```sh
cd cli/beacon
go build -o afferent ./cmd/afferent
./afferent --help
./afferent login          # opens the browser; --no-browser just prints the link
./afferent whoami
./afferent logout
./afferent version
```

Tests: `go test ./internal/afferent/...`. They use an in-process fake authsrv and
brainsrv, a fake `security` tool, and temp directories. They never touch the
real Keychain.

The Makefile and `.goreleaser.yaml` are upstream files and do not build
`afferent`. Packaging comes in D8, through `.goreleaser.afferent.yaml`.

## Configuration

The config file is `~/.config/afferent/config.json`, or
`$XDG_CONFIG_HOME/afferent/config.json`, or `$AFFERENT_CONFIG_DIR/config.json`.
The directory is 0700 and the file 0600. Later sources win:

1. Built-in defaults (vero-local):
   - issuer `http://authsrv.vero.localhost:8801`
   - client `afferent-cli`
   - brainsrv `http://localhost:18077`
   - context `afferent-poc`
2. `config.json`
3. Environment variables:
   - `AFFERENT_ISSUER`
   - `AFFERENT_ISSUER_DIAL`
   - `AFFERENT_CLIENT_ID`
   - `AFFERENT_TOKEN_ENDPOINT`
   - `AFFERENT_BRAINSRV_URL`
   - `AFFERENT_CONTEXT`
4. Flags:
   - `--issuer`
   - `--issuer-dial`
   - `--client-id`
   - `--brainsrv-url`
   - `--context`
   - `--config-dir`

A successful `login` writes the settings it used to `config.json`. Later
commands then find the same credentials without repeating the flags.

Every URL must be `https`. Plain `http` is allowed only for loopback hosts:
`localhost`, `*.localhost`, `127.0.0.0/8` and `::1`.

### Issuer hostname vs. the address you can reach

In vero-local the issuer, as it appears in discovery and in every token's `iss`,
is `http://authsrv.vero.localhost:8801`. Go and macOS resolve `*.localhost` to
loopback, but not every resolver does. There are three ways to set it up:

- **Default:** `--issuer http://authsrv.vero.localhost:8801`. This works when
  the name resolves.
- **Real issuer plus a dial address:**
  `--issuer http://authsrv.vero.localhost:8801 --issuer-dial http://localhost:8801`.
  Identity checks use the issuer, and the connections go to the dial address.
- **Loopback alias:** `--issuer http://localhost:8801`.
  - Discovery reports a different issuer (`authsrv.vero.localhost`). That is
    accepted **only because both hosts are loopback names**.
  - The issuer from discovery becomes the canonical one, and token `iss` must
    match it.
  - `localhost:8801` becomes the dial address.
  
  Any other mismatch between the configured and the discovered issuer is
  refused, as a guard against OAuth mix-up attacks.

When the CLI dials an address other than the issuer, it rewrites discovery
endpoints under the issuer onto that address. The browser link
(`verification_uri`) is never rewritten, because the browser needs the real
issuer host for its session cookie.

## Login (RFC 8628 device flow)

1. The CLI reads `/.well-known/openid-configuration` and takes
   `device_authorization_endpoint` and `revocation_endpoint` from it.
2. It sends `POST device_authorization_endpoint` with `client_id` and
   `scope=openid profile email`.
3. It prints `verification_uri` and `user_code`. It then opens
   `verification_uri_complete` in the browser, or `verification_uri?user_code=…`
   when there is no complete URI:
   - It uses `open` on macOS and `xdg-open` on Linux, with the Beacon display
     check.
   - It opens `http(s)` URLs only.
   - `--no-browser` skips this step.
4. It polls the token endpoint every `interval` seconds, 5 by default:
   - `authorization_pending` → keep polling.
   - `slow_down` → add 5 seconds to the interval.
   - `access_denied` or `expired_token` → stop with an error.
   - Reaching `expires_in` locally also stops it.
5. It checks that the access token's `iss` matches the canonical issuer, then
   stores the credentials under the refresh lock. If this machine already had
   a session, its refresh token is revoked on a best-effort basis.

**Token endpoint.** authsrv serves the device grant at `/oauth/token`, next to
`/oauth/device/code`. Its discovery `token_endpoint` is fosite's `/oauth2/token`,
which rejects the device grant. So when the device endpoint ends in
`/oauth/device/code`, the CLI uses the sibling `/oauth/token`, for both polling
and refresh. `AFFERENT_TOKEN_ENDPOINT` overrides this.

## Credential storage

Each stored record holds `access_token`, `refresh_token`, `expiry`, the
canonical `issuer` and `client_id`.

- **macOS: the login Keychain.** The record is a generic password:
  - service `afferent`
  - account `<configured issuer>|<client_id>`
  - password = base64 of the JSON credentials

  The CLI talks to the Keychain through `/usr/bin/security`:
  - **Writes** run `security -i` and send the `add-generic-password -U … -w
    <base64>` command on **stdin**. The secret never appears in argv, where any
    local user could see it with `ps`.
    - `-X` (hex) is not used. It doubles the size, and `security -i` splits
      lines at about 4 KiB.
    - If a record is too big for one line, it goes to the file instead.
  - `security -i` exits 0 even when the command inside fails. So every write
    is read back and compared.
  - **Reads** use `find-generic-password -w`, which returns the secret on the
    tool's stdout, through a pipe.
  - If the Keychain fails (for example a locked Keychain with no GUI, or an
    SSH session), the CLI warns and falls back to the file.
- **Everywhere else, and as the fallback:** `~/.config/afferent/credentials.json`,
  mode 0600.
  - It is written atomically. Writing through a symlink is refused.
  - On read, the file is opened with `O_NOFOLLOW` and rejected unless it is a
    regular file, owned by the current user, with no group or other permission
    bits.

## Token source and refresh

Other commands, and later the forwarder (D6), call `auth.TokenSource.Token`:

- If the access token is valid for more than 60 seconds, it is returned as is.
- Otherwise the source takes an exclusive `flock` on
  `~/.config/afferent/credentials.lock` and **reloads** the stored credentials.
  - If another process already refreshed, that result is used. A rotating
    refresh token is therefore redeemed exactly once. Tests cover this with 8
    concurrent sources.
  - If not, it posts `grant_type=refresh_token&client_id=…&refresh_token=…` to
    the token endpoint and saves the rotated pair.
- An `invalid_grant` answer deletes the stored credentials and returns "run
  `afferent login`".
- `unsupported_grant_type` means authsrv does not have D2 yet. The source also
  asks for a new login in that case, but keeps the credentials.

## whoami

`whoami` first decodes the access token without verifying it and shows:

- `sub`
- `email`
- `tenant_id`
- `account_id`
- `aud`
- `exp`

It then calls brainsrv `GET /v1/whoami` with `Authorization: Bearer` and
`X-Context`, and prints:

- the principal
- the principal kind
- the Context
- every grant, with its scope, verbs and template

A 404 or 405 from `/v1/whoami` means an older brainsrv. In that case the command
still shows the identity and notes that the scopes are unknown. Any other
non-2xx response is an error.

## logout

`logout` revokes the refresh token on a best-effort basis (RFC 7009) at the
discovery `revocation_endpoint`, with `token_type_hint=refresh_token` and
`client_id`. It then deletes the stored credentials, under the lock.

Until D2 lands, authsrv's `/oauth2/revoke` does not know device-flow refresh
handles. The call still succeeds, but it has no effect on the server.
