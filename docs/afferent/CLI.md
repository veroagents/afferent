# The `afferent` CLI

## User guide

```sh
brew install veroagents/tap/afferent   # D8; until then: cd cli/beacon && go build -o afferent ./cmd/afferent
afferent setup
```

That's it. `setup` walks through six steps and asks before each change:

1. **capture**: installs Beacon's capture hooks for the coding agents it
   finds (Claude Code, Codex, Cursor). They write to Beacon's runtime log.
2. **login**: signs you in with your browser, if you are not signed in.
3. **scope**: shows the member scope brainsrv gives you.
4. **service**: starts the forwarder in the background. It sends the log to
   brainsrv as you.
5. **mcp**: adds a `brain` MCP server to your agents, so they can read
   brainsrv. It shows the diff first.
6. **sync**: offers to send the history your agents already keep.

Afterwards, check it with `afferent status`, then restart your agents (or run
`/mcp` in Claude Code). You can run `setup` again at any time: steps that are
already done say so. Useful flags:
- `--dry-run` shows everything and changes nothing.
- `--yes` answers every question.
- `--skip sync,mcp` leaves out steps.
- `--harness claude,codex` limits the agents.

## Code

`afferent` is the fork's own binary (PLAN v0.3, D5). It has its own command
tree and does not reuse Beacon's root command. The code is new files only:

| Path | What |
|---|---|
| `cli/beacon/cmd/afferent/main.go` | entry point |
| `cli/beacon/internal/afferent/cli` | cobra commands |
| `cli/beacon/internal/afferent/config` | `config.json`, env and flag precedence, URL rules |
| `cli/beacon/internal/afferent/auth` | discovery, device flow, token storage, refresh, revoke |
| `cli/beacon/internal/afferent/brain` | brainsrv client (`/v1/whoami`) |
| `cli/beacon/internal/afferent/forward` | the built-in forwarder: tail, checkpoints, batching, delivery, status (D6) |
| `cli/beacon/internal/afferent/service` | launchd / systemd `--user` unit generation and loading (D6) |
| `cli/beacon/internal/afferent/mcpproxy` | `mcp proxy`: stdio MCP ↔ brainsrv `/mcp` (D7) |
| `cli/beacon/internal/afferent/mcpconfig` | `mcp config`: the Claude Code, Cursor and Codex config writers (D7) |
| `cli/beacon/internal/afferent/history` | `sync`: Beacon's Claude Code and Codex collectors (D7) |
| `cli/beacon/internal/afferent/capture` | setup's capture step: Beacon's hook installers (D7) |
| `cli/beacon/internal/afferent/member` | the signed-in member (settings, tokens, scope) for code outside the CLI, such as the memory backend (D7) |
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
./afferent forward --once --backfill   # send the runtime log once, from the oldest archive
./afferent service install             # run `afferent forward` in the background
./afferent status
./afferent mcp config --dry-run         # what the agents' configs would get
./afferent sync --since 720h           # backfill the last 30 days of history
./afferent setup --dry-run
```

Tests: `go test ./internal/afferent/... ./internal/learning/`. They use
in-process fakes and temp directories:
- a fake authsrv and brainsrv, including a streamable-HTTP `/mcp`;
- a fake `security` tool, a fake service loader, a fake capture installer and
  a fake `claude` CLI lookup;
- a temp HOME.

They never touch the real Keychain, `~/.beacon`, `~/.claude`, `~/.cursor`,
`~/.codex`, launchd or systemd.

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
   - `AFFERENT_SCOPE`: the forwarder's X-Scope. It is usually left unset, and
     then learned from brainsrv (see below).
4. Flags:
   - `--issuer`
   - `--issuer-dial`
   - `--client-id`
   - `--brainsrv-url`
   - `--context`
   - `--config-dir`

The forwarder keeps its state in `<config dir>/state`, a 0700 directory.
`AFFERENT_STATE_DIR` overrides the location.

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

## forward (D6)

`afferent forward` replaces the Vector forwarder. It tails Beacon's runtime log
and posts it to `POST <brainsrv>/v1/ingest/beacon/runtime`. Each request has:
- `Authorization: Bearer <access token>`
- `X-Context`
- `X-Scope`
- `Content-Type: application/x-ndjson`
- `Content-Encoding: gzip`

It runs in the foreground until interrupted.

```
afferent forward [--backfill] [--once] [--scope S] [--log-path P | --system] [--flush-interval 5s]
```

- **Which log.** The log comes from the Beacon endpoint configuration, the same
  way `beacon memory` finds it: the per-user log by default, or the system
  log with `--system`. `--log-path` names a file directly. The forwarder reads
  `runtime.jsonl` and the archives Beacon rotates it into (`.1` to `.5` at
  10 MiB).
- **Where it starts.**
  - The first run starts at the end of the live log, at a line boundary. The
    checkpoints are placed when the forwarder starts, before it waits for a
    sign-in or a scope, so events captured while it waits are sent later.
  - `--backfill` starts from the oldest retained archive. brainsrv dedupes on
    `event.id`, so history it already has comes back as `duplicate`.
- **Checkpoints.** The forwarder keeps one checkpoint per file identity (device
  and inode). Each holds a byte offset and a hash of the file's first 1 KiB.
  They live in `state/checkpoints.json` (0600), written atomically, and move
  **only after brainsrv answers 200** for the batch that held those bytes.
  - A restart resumes exactly where brainsrv last acknowledged.
  - A crash between the 200 and the checkpoint write re-sends one batch.
    brainsrv's dedupe makes that harmless.
  - **Rotation:** the files are read oldest first. When the live file's inode
    changes, the old inode (now `.1`) is read to its end, then the new live
    file from 0.
  - **Truncation:** a file smaller than its offset is read again from 0, and
    a line is logged. So is a file whose first bytes changed, for example a
    reused inode.
  - **Partial lines:** a line is sent only once its newline is written.
  - A file that rotates past `.5` before it was delivered is logged with the
    number of bytes lost.
- **Batches.** A batch is sent at 5,000 lines, at 4 MiB uncompressed, or 5
  seconds (`--flush-interval`) after data first waits. It is gzipped.
  - A line over 1 MiB is skipped, counted and logged. It is not allowed to
    block the log.
- **Responses.** Nothing is dropped on an error; the batch is retried until
  brainsrv takes it.

  | brainsrv answer | forwarder |
  |---|---|
  | 200 | advance the checkpoints |
  | 401 | force one token refresh (`TokenSource.ForceRefresh`) and retry once. If the session is gone (`invalid_grant`, signed out), pause with **login required** and retry every minute. After `afferent login` it carries on. |
  | 403 | ask `/v1/whoami` for the scope again, and switch if it changed. Otherwise pause with **scope denied**. |
  | 413 | split the batch in half and send each half. A single line that still gets 413 is skipped and counted. |
  | other 4xx | log it, record it (with a body snippet) in the status file, back off, retry |
  | 5xx, network | exponential backoff with jitter (1 s doubling, capped at 5 min) |

  Cancelling (Ctrl-C, SIGTERM) stops cleanly. The status says `stopped`.
- **One at a time.** An exclusive `flock` on `state/forward.lock` allows one
  forwarder per state directory. A second one exits with "already running".
- `--once` sends what is in the log now, then exits. Any failure is returned
  instead of retried, which suits tests and CI.

### The scope (X-Scope)

X-Scope is the member base scope. By default the forwarder asks brainsrv
`GET /v1/whoami` and takes the one grant whose scope ends in `.harness` and
includes `write`. With the D3 template, that is
`ws.<tenant>.people.<sub>.harness`.
- No such grant is an error that names the Context. So is more than one,
  which lists the scopes found.
- The answer is cached in `state/scope.json`. The cache is used when brainsrv
  cannot be reached or you are signed out, never over a definite answer.
- A service started before `afferent login` pauses with "login required"
  until you sign in.
- `--scope`, `AFFERENT_SCOPE` or `"scope"` in `config.json` set it by hand.
  brainsrv still enforces the grant.

### Status file

`state/status.json` holds:
- the pid and the state (`running`, `paused`, `backoff`, `stopped`)
- the paused reason
- the last success
- the last error, with the time
- the next retry
- the totals: batches, lines and bytes sent; accepted, duplicate and rejected
  from brainsrv; lines skipped
- the lag: bytes behind, per retained file and in total

## service (D6)

```
afferent service install [--log-path P | --system] [--program PATH]
afferent service uninstall
afferent service status
```

- **macOS:** a LaunchAgent `com.veroagents.afferent.forwarder` in
  `~/Library/LaunchAgents/`.
  - It is loaded with `launchctl bootstrap gui/<uid>`, after a `bootout` of
    any earlier copy.
  - It is resident (`RunAtLoad`, `KeepAlive`, 10 s throttle).
  - Its stdout and stderr go to `state/forwarder.log`, in the 0700 state
    directory, not `/tmp`.
- **Linux:** a systemd `--user` unit `afferent-forwarder.service` in
  `~/.config/systemd/user/`.
  - It is loaded with `daemon-reload`, `enable` and `restart`.
  - `Restart=always`. Its logs go to the journal:
    `journalctl --user -u afferent-forwarder.service`.
- The job runs `<this binary> forward --config-dir <dir>`. A Homebrew Cellar
  path is replaced by the stable one (`/opt/homebrew/bin/afferent`), so an
  upgrade does not break the job. `--program` overrides the binary.
- `install` saves the current settings to `config.json`, so the service uses
  the same issuer, brainsrv and Context as the shell that installed it. It
  warns if you are not signed in.
- `uninstall` stops the job and removes the unit. It keeps the checkpoints,
  so a later install resumes where it stopped.

Unit generation is pure (`service.Plist`, `service.SystemdUnitFile`, golden
files in `internal/afferent/service/testdata`). Load, unload and status go
through a `Loader`. The real `Launchctl` and `Systemctl` loaders run their
commands through a `Runner`, so tests check the exact commands without running
them.

## status

`afferent status` shows, in one place:
- **sign-in:** who you are, the issuer, and how long the token is valid. It
  refreshes the token if needed.
- **brainsrv:** the URL and Context, the principal, and the scope the
  forwarder would use (configured, from `/v1/whoami`, or cached).
- **service:** installed or not, running, and the pid.
- **forwarder:** the status file: state and paused reason, when it was last
  updated, the log path, the last success, the last error, the totals, and
  the lag per file.

## setup (D7)

```
afferent setup [--harness claude,codex,cursor|all|auto] [--yes] [--dry-run]
               [--skip capture,login,scope,service,mcp,sync] [--since D] [--no-browser]
```

It runs the steps in the user guide, in order, and ends with a summary of
each step: done, already done, declined, skipped or failed. A failure in one
step does not stop the others, but it makes the exit code non-zero.

- **Questions.** Every step that changes something prints what it will
  change and asks `[Y/n]`. `--yes` answers yes. No answer (end of input)
  means no, so a script without `--yes` changes nothing.
- **capture.** Beacon's own hook installers, by import
  (`internal/endpoint/hooks`), install hooks for `claude`, `codex` and
  `cursor`, at user level, pointed at the runtime log the forwarder reads.
  - This is the hooks half of `beacon endpoint install --harness X`.
  - The OTLP half is left out: it needs an OpenTelemetry collector binary
    and service that afferent does not ship, and the hooks already record
    every prompt, tool call and session event.
  - The hook binary is the one embedded in this build.
- **login** runs the device flow only when you are not signed in.
- **scope** asks brainsrv `/v1/whoami` and caches the answer.
- **service** is `afferent service install`. It installs or updates the job
  and restarts it.
- **mcp** is `afferent mcp config`. It shows the diff, then asks.
- **sync** is `afferent sync` for the agents with history, after a
  question. `--since` passes through.
- **`--dry-run`** changes nothing anywhere:
  - no file writes and no backups;
  - no token refresh and no `/v1/whoami` call;
  - no `launchctl` or `systemctl` changes;
  - no claude CLI.

  It only reads: the stored credentials, the service status, and the
  agents' configs, whose diffs it shows.

## mcp proxy (D7)

`afferent mcp proxy` is the MCP server the agents start. It speaks stdio MCP
to the agent: one JSON-RPC message per line on stdin and stdout. For each
message it sends `POST <brainsrv>/mcp` (streamable HTTP) with:
- `Authorization: Bearer <access token>` (from the refreshing token source)
- `X-Context`
- `X-Scope`: the member base scope, found the same way as the forwarder's
  (`--scope`, `AFFERENT_SCOPE`, config, `/v1/whoami`, or the cache)
- `Accept: application/json, text/event-stream`
- `Mcp-Session-Id` and `Mcp-Protocol-Version`, once brainsrv has assigned
  them

brainsrv's answers go back to the agent:
- A JSON answer is written as one line.
- A `text/event-stream` answer: each event's `data` (multi-line data is
  joined) is written as one line. The stream is closed once every request
  it carries has its response.
- Notifications and the agent's own responses to server requests expect 202,
  and produce no output.

stdout carries only protocol messages. Logs go to stderr.

**Sessions and token refresh.** brainsrv binds a `/mcp` session to the sha256
of the bearer token, so a session dies at every refresh (about every 15 min).
The proxy keeps the agent's `initialize` params and recovers without the
agent noticing:
- When the access token changes, it opens a new session first:
  `initialize` with the saved params, then `notifications/initialized`. Both
  answers are discarded.
- When brainsrv refuses a message, it recovers and sends the message again
  once:
  - on a 401, it forces one token refresh, then opens a new session;
  - on a 403, it asks `/v1/whoami` for the scope again (a session opened with
    another token also gets 403), then opens a new session;
  - on a 404 for a session it sent, it opens a new session.

**Errors** become JSON-RPC error responses. The proxy never exits on them,
and requests run concurrently.

| case | code |
|---|---|
| a line that is not JSON, or over 32 MiB | -32700 (id null) |
| not an object or batch, or no method, result or error | -32600 |
| signed out, or refused after the one retry | -32001 (the message says to run `afferent login` when signed out) |
| brainsrv unreachable or 5xx | -32000 |

## mcp config (D7)

```
afferent mcp config [--harness claude,cursor,codex|all|auto] [--dry-run] [--remove] [--program PATH]
```

It registers an MCP server named `brain`. Its command is the absolute path of
this binary (the stable Homebrew path when installed by brew), and its args
are `["mcp", "proxy"]`, plus `--config-dir <dir>` when a non-default config
dir is in use. The entry goes in each agent's **user-level** config:

| Agent | File | How |
|---|---|---|
| Claude Code | `~/.claude.json` `mcpServers.brain` | `claude mcp add --scope user brain -- <afferent> mcp proxy` when the `claude` CLI is on PATH, because Claude Code rewrites that file itself. Otherwise the one member is edited in place. If the CLI fails, it falls back to the in-place edit. |
| Cursor | `~/.cursor/mcp.json` `mcpServers.brain` | edited in place. The file is created if missing. |
| Codex | `~/.codex/config.toml` `[mcp_servers.brain]` | a marked block (`# >>> afferent …` / `# <<< afferent <<<`) is appended, or replaced where it is. Nothing outside the block is touched. It refuses when the file already defines `brain` itself, or defines `mcp_servers` as an inline table or dotted key. |

- **Safe edits.** JSON is edited byte-preserving: key order, formatting and
  every other server and setting keep their exact bytes. Only the `brain`
  member is inserted, replaced or removed. The result is validated before it
  is written. A file that is not plain JSON, or a symlink, is refused.
- **Idempotent.** An identical entry is left alone (`already configured`).
  A changed binary path updates it.
- **Backups.** Before a change, the file is copied to `<file>.afferent.bak`.
  The write is atomic and keeps the file's mode.
- `--remove` takes the entry out. `--dry-run` prints a diff per file, and the
  claude command it would run, and changes nothing.
- `auto` (the default) picks the agents found in your home: `~/.claude` or
  `~/.claude.json`, `~/.cursor`, `~/.codex`, or the `claude` or `codex` CLI.
- Upstream Beacon has no MCP-config writer to reuse (`beacon mcp` only prints
  a snippet), so these writers are new.

## sync (D7)

```
afferent sync [--harness claude,codex|all|auto] [--since D] [--no-wait] [--log-path P | --system]
```

`sync` backfills the session history Claude Code (`~/.claude/projects`) and
Codex (`~/.codex/sessions`) already keep. It appends that history to Beacon's
runtime log, and the forwarder sends it.
- It runs Beacon's own collectors by import: `claudesession.CollectOnce` and
  `codexsession.CollectOnce`, the code behind `beacon endpoint claude sync`
  and `beacon endpoint codex sync`. Every event is marked
  `collection_method=poll`.
- **Cursors are Beacon's.** It uses `~/.beacon/endpoint/state/claude.json`
  and `codex.json`, the same defaults as Beacon's commands, not files owned
  by afferent. The runtime log is shared. With separate cursors, a user who
  also runs Beacon's sync would get every session written twice. With one
  set, a session is written once, whoever sweeps first. Running `sync` again
  only adds what is new.
- **`--since 720h`** skips sessions not written in that window. It marks
  them as read at their current end in the cursor file, so later runs skip
  them too, and a resumed old session continues from its end.
- **It never loses the backfill to a first run.** A first forwarder run
  starts at the end of the log. So before writing, `sync` places the
  forwarder's checkpoints (`forward.Prime`) when no forwarder has run yet.
  A running forwarder places its own when it starts.
- **Large histories.** The runtime log keeps about 60 MiB (live file plus 5
  archives). When Beacon's collector stops because more would rotate its
  own output away, `sync` drains the log and sweeps again:
  - with no forwarder running, it sends the log itself (`forward --once`);
  - otherwise it waits, up to 30 min, for the running forwarder's lag to
    reach 0.

  `--no-wait` stops instead and says to run it again.

## Memory write-through on the afferent token (D7)

Beacon's memory store (`beacon memory`, and Beacon's MCP and dashboard)
writes approved memories through to brainsrv (PLAN Phase 2). It picks the
credential this way:

| Setting | Backend |
|---|---|
| `BEACON_MEMORY_BACKEND=local` (or `off`, `none`) | local SQLite only |
| `BEACON_BRAINSRV_KEY_FILE` set | the `spk_` key file, as before (needs `BEACON_MEMORY_BACKEND=brainsrv`, `BEACON_BRAINSRV_URL`, `BEACON_BRAINSRV_SCOPE`) |
| otherwise, signed in to afferent | the afferent token |
| otherwise | local only, with a warning when `BEACON_MEMORY_BACKEND=brainsrv` |

With the afferent token:
- Every request carries `Authorization: Bearer <access JWT>` from the
  refreshing token source (one forced refresh and a retry on 401) and
  `X-Context` from the afferent config.
- The URL is the afferent `brainsrv_url`.
- The base scope is the member scope: the configured `scope`, else the
  forwarder's cached `/v1/whoami` answer, else `/v1/whoami`.
- `BEACON_BRAINSRV_URL` and `BEACON_BRAINSRV_SCOPE` still override the URL
  and the scope.
- The sign-in is looked up at most every 30 s per process: stores are opened
  per request, and on macOS reading the Keychain runs `/usr/bin/security`.
- `beacon memory brainsrv status` shows `auth afferent` or `auth key`.

The code is in new files: `internal/learning/brainsrv_afferent.go` and
`internal/afferent/member`. The fork's own `backend.go`, `brainsrv.go` and
`cmd/memory_brainsrv.go` call into them. The upstream seam files
(`store.go`, `cmd/memory.go`, `mcpserver/server.go`, `dashboard/memory.go`)
are unchanged.
