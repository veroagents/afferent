# PLAN — afferent (Beacon fork) ↔ brainsrv

Status: v0.3 (authsrv end-to-end direction added; see ★) · 2026-09-24 (review changes folded in, see §7) · implements [`SPEC.md`](SPEC.md) v0.1
Verified against: `agent-beacon` @ `c03b02f` (local clone `original-agent-beacon/`)
and `brainsrv` @ `d8bcd0f` (`~/projects/brainsrv`).

The SPEC holds up in outline, but checking both codebases turned up **24
places where it assumes something that isn't true** (§0). This plan keeps the
SPEC's goals and build order. It fixes those assumptions and adds the
brainsrv prerequisites they uncovered.

---

## ★ Direction v0.3 (2026-09-24): authsrv end to end, own `afferent` CLI, no Vector

This supersedes the credential and transport parts of §1.2, §4 Phase 5 and
§5. What is already built stays: the brainsrv Phase 1 and 4 server work, the
capture mapping, the Phase 2 memory write-through, and the label sanitizer.

### What the end user does
```
brew install veroagents/tap/afferent
afferent setup          # install capture → browser sign-in (authsrv device flow) → start forwarder → configure agents' MCP
```
No key files, no scopes, no URLs, no Vector.

### Principles
1. **authsrv is the only issuer.** brainsrv never mints or stores user
   credentials; it only verifies authsrv ES256 JWTs (P10: `sub`→principal,
   claim→grant templates, `X-Context` selects the Context). There are no
   `spk_` keys on laptops.
2. **Identity decides scope.** A member's scope comes from token claims
   through brainsrv grant templates. It is never a client-chosen flag.
3. **One binary.** `afferent` is a new program inside the fork at
   `cli/beacon/cmd/afferent/`. It has its own command tree and uses Beacon's
   internal packages as libraries. All of it is new files, so upstream rebases
   stay clean. Only `afferent` ships, through `.goreleaser.afferent.yaml`.
4. **The forwarder is built in, not Vector.** It is a Go tailer in the
   `afferent` binary:
   - It keeps a checkpoint per file, by inode, across Beacon's
     `.1`–`.5` rotation.
   - It batches ≤5,000 lines, gzips, retries on 5xx and drops on 4xx.
   - At-least-once delivery is safe because brainsrv dedupes on `event.id`.
   - It refreshes its own access tokens. Vector cannot do that, which is why
     Vector goes.
5. **Agents read through brainsrv's own MCP** (`/mcp`, streamable HTTP)
   using an authsrv token. Beacon's MCP is not shipped to users.

### Flow
```
afferent login ──RFC 8628 device flow──► authsrv (/oauth/device/code → browser approve → /oauth/token)
      ◄── access JWT (aud=brainsrv, sub=user, tenant_id, account_id, roles; 15 min) + refresh token (Keychain)
afferent forwarder ──Bearer JWT + X-Context──► brainsrv /v1/ingest/beacon/runtime   (refreshes before expiry)
agents ──────────── authsrv token ───────────► brainsrv /mcp
```

### Verified facts (vero-local, 2026-09-24)
- authsrv advertises and serves the device flow. `deviced-cli` is an existing
  public device-flow client (migration 017).
- User tokens carry `sub`, `account_id`, `tenant_id`, `email`, `roles` and
  `scopes`. `aud` comes from the client's registration. Access TTL is 15 min.
- **Blocker: device-flow refresh tokens are never stored.**
  `IssueUserToken` returns a random handle, and nothing writes the
  `user_refresh:*` key its comment describes. `/oauth/token` accepts only the
  `device_code` grant, and `/oauth2/token` (fosite) only knows auth-code
  refresh tokens.
- `/oauth2/token-exchange` needs a confidential client and adds no `act`
  chain, so the CLI does not use it. It asks for `aud=brainsrv` directly.
- **brainsrv gap:** grant templates map `claim=value` to a fixed scope, with
  no `{sub}` or `{tenant_id}` substitution. So per-member scopes cannot be
  derived from claims yet.

### Work by repo
| # | Repo | Work |
|---|---|---|
| D1 | authsrv | Register public client `afferent-cli` (device_code + refresh_token, `audience={brainsrv}`, scopes openid profile email) as a migration like 017 |
| D2 | authsrv | **Persist and redeem device-flow refresh tokens.** Use the Postgres `oauth2_refresh_tokens` store, accept `grant_type=refresh_token` for them, rotate on use, revoke via `/oauth2/revoke`. Each login is its own session, which gives per-device revocation. |
| D3 | brainsrv | Grant-template substitution: `scope_path` may contain `{sub}` and `{tenant_id}`, sanitized to ltree labels with the shared label rules. Example template: any user token ⇒ `ws.{tenant_id}.people.{sub}.harness` with read+write+forget. Plus the afferent Context's `auth_config` (issuer, JWKS, `aud=brainsrv`, autoprovision). |
| D4 | brainsrv | Beacon ingest and `/mcp` accept the JWT path. Verify that X-Scope is optional for JWTs and defaults to the member scope, or keep it required and have the CLI send the templated scope it learns at login. |
| D5 | afferent | Scaffold `cmd/afferent` with `login` / `logout` / `whoami`: device flow, refresh token in the macOS Keychain (file fallback 0600 elsewhere), auto-refresh. |
| D6 | afferent | Built-in forwarder: `afferent forward` running as a launchd/systemd service, with checkpoints, rotation, batching and gzip. It reuses the Phase 5 tests (delivery, restart, backfill duplicates, brainsrv down). Delete the Vector pack. |
| D7 | afferent | `afferent setup` (capture install + login + forwarder), `afferent mcp config` (writes the brainsrv `/mcp` entry for Claude Code, Cursor and Codex), `afferent sync` (history backfill via the harness readers). The memory write-through uses the same token. |
| D8 | afferent | Packaging: a Homebrew `afferent` formula with no Vector dependency. |

**Order:** D1 → D5 (login works with 15-min tokens). Then D3 → D4, which is
end-to-end with a real token. D2 is needed before D6 can run unattended. Then
D7 and D8.

**Test plan:** each step is tested live on vero-local with a real browser
approval. The first test is D1 + D5: device login as `drew@vero.localhost`,
decode the claims, and call brainsrv with the JWT.

---

## 0. Where the SPEC is wrong, and what we'll do instead

### 0.1 brainsrv (Part A)

| # | SPEC assumes | Reality in code | Plan |
|---|---|---|---|
| A-1 | Extraction can run "after commit, asynchronously via the existing jobs/outbox" | `ingest.AppendTurn` (`internal/ingest/turns.go:92-170`) embeds and extracts **synchronously**. The outbox is a read-only tailer. Jobs are per-Context sweeps with no payload column and no extractor in `jobs.Env`. | New job kind `turn_extract`: sweeps turns where `extract AND NOT extracted`. **The job and the inline path call one shared function, `ingest.ExtractTurn`**, which uses the same Context model_config, the same prompt and the same `ExtractorVersion` stamp (`turns.go:27`), so facts from Beacon and from agentd chat match in quality and provenance. `jobs.Env` gets an `Extraction` hook next to `Background`. Beacon ingest never calls the LLM inline (**P-A2**). |
| A-2 | Remember `text` is embedded for recall | `/v1/remember` returns **501** for non-empty `text` (`internal/httpapi/memory.go:131`). Remember never embeds anything; `embed_backfill` fills vectors about every 10 min. | B1 sends **facts only**. Searchable content goes in a `summary` text attribute, which is BM25-indexed at once. **Vector lag is seconds, not 10 min:** after a batch commits, `/v1/remember` and Beacon ingest each enqueue `embed_backfill` for their Context directly with `jobs.Queue.Enqueue`. The idempotency key is bucketed per 30s, which debounces the enqueues. The admin route `POST /v1/admin/jobs/kick` is bootstrap-only, so ingest can't use it with a member key. |
| A-3 | brainsrv runs its "existing PII scan" on `text` | There is **no PII scan**. The only scan is for prompt injection: `pi_sweep` runs on turns after the fact, and docs are scanned at ingest. | Drop the claim. Beacon's local privacy mode is the only redaction. Record this in the brainsrv SPEC errata. |
| A-4 | `recall filters.valid_at` excludes memories not yet valid | `valid_at` only applies to tier-1 direct lookup (`internal/retrieve/retriever.go:334`). The vector and lexical scans ignore it, and turns are always included. | **P-A3**: apply `valid_at` to every scan. This must land before the A4 valid-time test can pass. |
| A-5 | Recall hits can be mapped back to `beacon.memory` entities | A hit is `{table,id,text,entity_id,…}`. There is **no entity type and no attributes**, and no public endpoint reads an entity. | **P-A4**: add `entity_type` and `entity_name` to hits, plus `GET /v1/entities/{id}` (memAuth read) that returns the current attributes. |
| A-6 | Writing `superseded_by` closes valid-time / recall hides superseded memories | A new predicate closes nothing. Reconcile supersedes only the **same predicate at the same scope** (`reconciler.go:139`), and recall doesn't filter on it. | **Move entity-level supersede into Phase 1 (P-A5)** so the server owns the truth. afferent and agentd both read brainsrv, and filtering on the client side would let them disagree. `entities` already has `state/superseded_by/supersedes` and appears in `payloadColumns`. Add `SupersedeEntity`, a version bump modelled on `AgeRow`/`ForgetRow` in `lifecycle.go`, which cascades `supersedeRow` to the entity's active attributes and relations so every recall scan drops them. **No `state` attribute convention and no client-side filter.** **Answers Q2.** |
| A-7 | API keys may have a default scope (Q1) | No `default_scope`. `memAuth` returns 400 without `X-Scope`. | **Require `X-Scope`.** B3 always sends it. No migration needed. The signing key is **one per member per install**, never a shared fleet key (§1.2). **Answers Q1.** |
| A-8 | Health is `POST /v1/ingest/beacon/health` with NDJSON | Upstream's `/v1/ingest/health` is Vector's **sink healthcheck**, which is a GET with the bearer key. There is no heartbeat stream. | `GET /v1/ingest/beacon/health` returns 200 when auth is valid, plus `last_seen` per hostname. Runtime ingest records `last_seen` from `endpoint.hostname`. |
| A-9 | Label charset is `[A-Za-z0-9_]` | Scope regex is `^[a-z0-9_]+(\.[a-z0-9_]+)*$`, lowercase only. | The sanitizer outputs `[a-z0-9_]` only. |
| A-10 | Env flag `BRAINSRV_BEACON_REFLECT` | brainsrv env vars use the `BRAIN_` prefix. | `BRAIN_BEACON_REFLECT`, `BRAIN_BEACON_SESSION_IDLE`. |
| A-11 | Reflect can run "over that session's turns" | Reflect is query → retrieve → synthesize. It has no session input, and provenance is hard-coded to `reflection`. | A3 needs a new session-scoped reflect entry point. It is flag-gated and **scheduled last** (Phase 6). |
| A-12 | One transaction per session for a 10k-line batch | `BRAIN_WRITE_TX_DEADLINE` is 5s. `seq` is assigned `max(seq)+1` in the INSERT, so concurrent appends to one session hit the unique constraint. | Chunk inserts (≤500 turns per transaction). Take `pg_advisory_xact_lock(session)` per session. Embed Beacon turns lazily via backfill, not inline. |

### 0.2 afferent (Part B)

| # | SPEC assumes | Reality in code | Plan |
|---|---|---|---|
| B-1 | brainsrv pins an `asymptoteobserve` tag | The module has **no tags**. The CLI consumes it through `replace`. | brainsrv pins a pseudo-version of upstream `c03b02f` (`go get …/pkg/asymptoteobserve@c03b02f`). A contract test parses upstream `sample-event.jsonl`. **Answers Q5.** |
| B-2 | The `Store` API takes `context.Context` | No `Store` method takes a ctx. Each call opens and closes SQLite. | `MemoryBackend` keeps the ctx signature. `Store` calls it with `context.WithTimeout(context.Background(), 5s)`. |
| B-3 | `memory_sync` is "a new table" | The schema is gated by `PRAGMA user_version` (`storeSchemaVersion = 1`). Bumping it is an upstream edit that will collide the day upstream bumps to 2. | Create `memory_sync` with `CREATE TABLE IF NOT EXISTS` in our own `ensureSyncSchema()` (in `brainsrv.go`). **Do not touch `user_version`.** |
| B-4 | `Store` gets an optional backend "additively" | `Store` is concrete and passed as `*Store` everywhere. Write-through needs hooks inside `PutMemory`, `ListMemories` and `GetMemory`. | **One hook seam.** Add one field `hooks StoreHooks` (an interface with `AfterPut`, `Search`, `GetMissing`), and each method gets a single line that calls through it. All logic lives in new files. The 3 call lines can't be avoided while `Store` is concrete, but a conflict now needs upstream to touch that exact line in that method. Listed in §4. |
| B-5 | The subcommand is registered with "one line in `cmd/endpoint.go`" | Asymptote is a table entry in `siemDestinations`, and `endpointCmd` is a package var. | **Zero edits.** `cmd/endpoint_brainsrv.go` calls `endpointCmd.AddCommand(...)` in its own `init()`. `cmd/memory_brainsrv.go` does the same for `memoryCmd`. |
| B-6 | The forwarder installs `com.afferent.brainsrv-forwarder` | `service.ForwarderManager` hard-codes `ForwarderLabel` and `ForwarderSystemdUnit`. | Small upstream edit: optional `Label`, `SystemdUnit` and `Description` fields on `ForwarderManager`. Zero values fall back to the existing constants. |
| B-7 | MCP results can carry `degraded` / `history` | `search_memory` and `get_memory_context` have result structs. `get_memory` returns a bare `LearningMemoryV1`. | Add `degraded,omitempty` and `history,omitempty` fields to the two structs. Leave **`get_memory` unchanged** (it reads local first, so it never degrades). |
| B-8 | 0600 key-file check "following Beacon's pattern" | No such helper exists. `ReadDeviceKey` doesn't check permissions. | New `brainsrvcfg.ReadKeyFile`: `Lstat`, reject symlinks, require `uid == Getuid`, require `perm & 0o077 == 0`. |
| B-9 | Session key falls back to `run.provider:run.id` | The field is `run.run_id`. | Use `run.provider + ":" + run.run_id`. |
| B-10 | B4 CI forward is "small" | Adding it touches `forward.go` (5 places), `endpointconfig.Destinations` and the collector exporter config. | Stays **deferred / post-v1**. |
| B-11 | Label suffix uses blake3 | blake3 would be a new dependency in the fork's `go.mod`, which is an upstream file and a rebase conflict magnet. | Use a **sha256** suffix on both sides. It's stdlib only. brainsrv already has blake3, but uses sha256 here too. |
| B-12 | Homebrew formula / release artifacts renamed | The GoReleaser `brews:` block publishes to `asymptote-labs/homebrew-tap`. | New file `cli/beacon/.goreleaser.afferent.yaml`. Leave upstream's file alone. |

Q3 (are text attributes searchable?) **Yes**, BM25 on `predicate || value_text`,
and they get embeddings eventually via backfill. That's why A-2's `summary`
attribute works. Q4 (retire `claude-code-hook`?) **No.** Keep it for setups
without Beacon.

---

## 1. Shared contract: the label sanitizer

The SPEC resolves this in favor of a shared test vector. Deterministic and stateless:

```
label(s):
  base = basename(strip ".git", strip URL scheme/host/trailing "/")(s)
  l    = lowercase(base); replace every char ∉ [a-z0-9_] with "_"; collapse "__+" → "_"; trim "_"
  if l == "": l = "_empty"
  lossy = (l != lowercase(base)) || len(l) > 64
  if lossy: l = l[:55] + "_" + hex(sha256(base))[:8]     // ≤ 64 chars
  return l
```

Notes:
- "Collision" in the SPEC is really stateful. This replaces it with "lossy": any
  sanitization change adds the suffix, so `My.Repo` and `my_repo` never share a
  label.
- Harness names such as `claude-code` become `claude_code_<h8>`. That's ugly in
  scope paths, so **harness labels come from a fixed allowlist map**
  (`claude-code → claude_code`, `codex → codex`, …) and use the generic rule
  only for unknown harnesses. The map is also part of the test vector file.
- **Test vector file:** `testdata/beacon/labels.json`, `[{in, kind: repo|harness, out}]`
  with about 40 cases, including SSH/HTTPS remotes, unicode and Hebrew names,
  over-long names, and empty input.
  - Canonical copy lives in brainsrv.
  - afferent keeps a byte-identical copy at
    `cli/beacon/internal/brainsrvcfg/testdata/labels.json`.
  - `make check-labels` in afferent diffs the two when `../brainsrv` exists. CI
    skips it otherwise.

### 1.2 Identity and scope for captured sessions

Beacon runs on a person's laptop, so captured activity belongs to that person.
- **Principal:** one brainsrv API key **per member per install**, minted for that
  member with the member's own grants. This matches the delegated-token model.
  There is no shared afferent service key, and no fleet key that picks scopes
  freely.
- **Bound:** mint the key with `narrow_scope` = the member's base scope.
  `authz.Decide` already enforces `NarrowScope`, so even a misconfigured
  `--scope` can't write outside the member's subtree.
- **Base scope:** `ws.<ws_id>.people.<member>.harness`. The SPEC's
  `org.me.coding` example becomes this.
  - Derived history: `<base>.<repo_label>.<harness_label>`.
  - Memories: `<base>.<repo_label>`.
  - Repo-first ordering puts memories and every harness's history for a repo
    under one subtree.
- **Rules:**
  - `connect` refuses a `--scope` that isn't under the key's grant. It checks
    with GET health before writing any config.
  - brainsrv checks every derived scope against the key (403 on the whole batch).
- **Out of v1:** team-shared memories, meaning promoting a member's approved
  memory to `ws.<id>.projects.<repo>`. That will be a separate, explicit action.

---

## 2. Phases and order

**brainsrv Phase 1 lands first and is validated with agentd, the client that
already exists, before the fork starts.** Phase 1 also fixes agentd directly:
`valid_at` everywhere, entity reads and entity supersede, and the advisory lock.
The lock matters because of how agentd appends turns
(`agentloop/services/agentd/src/brain/client.ts:539`). Within one call, the user
turn and the assistant turn are posted one after the other, which is safe. But
the end-of-run append is fire-and-forget (`src/lane/lane-manager.ts:516`), so it
can overlap:
- the next run's append for the same conversation
- the compaction-summary append (`lane-manager.ts:384`)

Because brainsrv extraction is currently inline and slow, that overlap window is
seconds wide, and overlapping appends collide on `UNIQUE(session_id, seq)`.

| Phase | Repo | Deliverable | Blocks |
|---|---|---|---|
| **1** | brainsrv | P-A1 turn widening + session lock + shared `ExtractTurn` · P-A3 `valid_at` everywhere · P-A4 hit enrichment + entity read · P-A5 entity supersede · remember → `embed_backfill` enqueue · label sanitizer | gate |
| **gate** | brainsrv + agentd | Deploy Phase 1 to dev and run the agentd live smoke (§2.1) against it. Everything is green before any fork work. | 0 |
| **0** | afferent | Fork, repo layout, weekly upstream-rebase CI, CI green on unchanged upstream | 2 |
| **2** | afferent | B1 memory backend + `memory brainsrv status/sync` | B2 |
| **3** | afferent | B2 MCP changes (`degraded`, `include_history`, `recall_brain`) | — |
| **4** | brainsrv | P-A2 `turn_extract` job · A2 `/v1/ingest/beacon/runtime` + GET health | B3 |
| **5** | afferent | B3 `beacon endpoint brainsrv` forwarder | acceptance |
| **6** | brainsrv | A3 session-close reflect (flag-gated, default off) | — |
| **7** | both | End-to-end acceptance (SPEC §6) | — |
| later | afferent | B4 CI forward | — |

Phase 4 can run in parallel with Phases 0–3 because they touch different repos.

### 2.1 The agentd gate

There is **no `smoke_brainsrv.sh` yet**, and agentd has no live brainsrv test:
`tests/d7-memory.test.ts` runs against `InMemoryBrainClient`. So the gate starts
by building one:
- [ ] `agentloop/services/agentd/tests/brainsrv-live.test.ts` (vitest; skipped
  unless `BRAINSRV_URL` + `BRAINSRV_TEST_KEY` are set).
  - It drives the real `HttpBrainClient` through every route agentd uses:
    - `POST /v1/sessions` (`ensureSession`)
    - `…/turns`
    - `POST /v1/facts` (agentd's `remember` writes facts, not `/v1/remember`)
    - `/v1/recall`, `/v1/context`, `/v1/forget`, `/v1/inspect/trace`
  - Add a thin `scripts/smoke_brainsrv.sh` wrapper that runs it.
- [ ] Cases:
  1. D7.2 against the real server: a fact written in one conversation is
     recalled in another.
  2. **Overlap:** run two `appendTurn` calls (user+assistant each) on one
     conversation concurrently with `Promise.all`, 10×. Expect all 40 turns
     stored, no 5xx, and `seq` strictly increasing. This reproduces the bug
     before P-A1 and passes after it.
  3. **Supersede (P-A5):** create a fact entity, supersede it, and check that
     `recall` no longer returns it while `recall` with `valid_at` before the
     supersede still does.
  4. **`valid_at` (P-A3):** a fact is invisible to `recall` before its
     `valid_from`.
  5. The legacy `{role,text}` turn body is unchanged. agentd sends only that.
- [x] **Baseline, 2026-09-24, vero-local brainsrv on `:18077`:** 2 passed, 3 failed,
  as predicted. The overlap case lost **14 of 40 turns**: brainsrv returned
  `400 duplicate key … turns_session_id_seq_key`, and agentd's fire-and-forget
  `appendTurn` swallowed the error. Run it with
  `services/agentd/scripts/smoke_brainsrv.sh`.
- [x] **Gate passed, 2026-09-24:** `beacon-phase1` @ `5e463e5` deployed to vero-local
  as image `vero-local/brainsrv:beacon-phase1` (binary layered on `:dev`).
  Migration 0014 was applied on boot to all 4 Contexts. Result: **5/5 passed on
  3 consecutive runs**, with 0 errors in the brainsrv logs.
- [x] Run it on brainsrv `main` first (cases 2–4 are expected to fail, which
  records the baseline), then on Phase 1. The gate passes when everything is
  green on Phase 1.

Side finding, **not in scope**: agentd's `ensureSession` caches the brainsrv
session id per process (`client.ts:508`), with no natural key. Two concurrent
first calls for a conversation, or a process restart, create duplicate brainsrv
sessions. Beacon's `beacon_session_map` solves the same problem on the server.
A generic `POST /v1/sessions` with a client `external_id` upsert would fix both;
file it as a brainsrv follow-up.

---

## 3. Part A — brainsrv tasks

Migrations: next ctx number is **0014**, next ctl number is **0007**. Deploys
must run `brainsrv migrate`, because `serve` only migrates contexts when
`--migrate-contexts` is set.

### Phase 1

**P-A1 · Turn-model widening (SPEC A1)**
- [ ] `ctx/0014_turn_provenance.sql`:
  - add turns columns `kind TEXT`, `source_event_id TEXT`, `occurred_at TIMESTAMPTZ`,
    `attrs JSONB`, `extract BOOLEAN NOT NULL DEFAULT true`
  - add `CREATE UNIQUE INDEX turns_source_event_uq ON turns(session_id, source_event_id) WHERE source_event_id IS NOT NULL`
  - add `turns_occurred_idx ON turns(session_id, occurred_at, seq)`
  - The index is built inside a transaction (the migration runner requires it),
    so note the lock time for large Contexts in the RUNBOOK.
- [ ] `ingest.AppendTurn` → take a `TurnInput` struct. `text` becomes optional
  when `attrs` is set; the DB `text NOT NULL` stays, stored as `""`.
- [ ] Duplicates: `INSERT … ON CONFLICT (session_id, source_event_id) WHERE … DO NOTHING RETURNING`.
  If no row comes back, SELECT the existing turn and return **200** with it.
  Don't embed or extract a duplicate.
- [ ] Take `pg_advisory_xact_lock(hashtext(session_id))` around the `seq` insert
  (fixes the A-12 race).
- [ ] Add an `extract:false` gate before the extractor call.
- [ ] Refactor the extract + reconcile half of `AppendTurn` into
  `ingest.ExtractTurn(ctx, deps, turn)`. The inline path calls it now, and
  `turn_extract` calls it in Phase 4. There is one prompt, one model resolution
  (the Context model_config) and one `ExtractorVersion` idempotency key
  (`turn:<id>:v1`), so re-running a turn through either path is a no-op.
- [ ] Thread `occurred_at` → `ingest.Input.ValidFrom` in `extract.go:147`.
  Without this, valid-time does nothing.
- [ ] `GET …/turns` → `ORDER BY coalesce(occurred_at, created_at), (attrs->'beacon'->>'sequence')::bigint NULLS LAST, seq`.
- [ ] Contract updates:
  - `api/openapi.yaml` (turn request and response)
  - proto `AppendTurnRequest` fields 5–9, then `make proto`, and update the
    grpcapi handler
  - TS `appendTurn` and Python `append_turn` optional fields
- [ ] Tests:
  - idempotent re-post
  - ordering
  - `extract:false` never reaches the `fakeExtractor`
  - the legacy `{role,text}` body is unchanged
  - **concurrent user+assistant appends to one session both succeed** (the
    agentd case)
  - `hook_e2e_test` must stay green

**P-A3 · `valid_at` on every scan**
- [ ] Apply the `valid_from <= $t AND (valid_until IS NULL OR valid_until > $t)`
  predicate in the vector, lexical and graph scans (`retriever.go:480-545`).
- [ ] Turns: `coalesce(occurred_at, created_at) <= $t`.
- [ ] Test: a fact extracted from a turn with `occurred_at=T` is invisible at
  `valid_at=T-1s`.

**P-A4 · Hit enrichment + entity read**
- [ ] Recall hits gain `entity_type`, `entity_name` (`omitempty`). Update
  OpenAPI, proto and SDKs.
- [ ] `GET /v1/entities/{id}` (memAuth `read`, scope-checked) →
  `{id,type,name,state,attributes:{predicate:{value,value_type,valid_from,confidence}}}`
  for current attributes only.
- [ ] SDK drift: add the route to both `coverage.json` manifests and both clients.

**P-A5 · Entity-level supersede (replaces the SPEC's attribute convention)**
- [ ] Add `reconcile.SupersedeEntity(tx, oldID, newID, validFrom)`, modelled on
  the `AgeRow`/`ForgetRow` version-bump pattern:
  - sets the entity to `state='superseded'`, `superseded_by=newID`,
    `valid_until=validFrom`
  - sets `supersedes=oldID` on the new entity
  - cascades `supersedeRow` to the old entity's active attributes and relations
    in the same tx
  - writes a decision trace
- [ ] `POST /v1/entities/{id}/supersede {by, valid_from}`: memAuth `write`, and
  `Idempotency-Key` required. Both entities must be in scope.
- [ ] OpenAPI, proto and SDKs. Add the route to both `coverage.json` files.
- [ ] Tests:
  - recall at `now` excludes the old entity's attributes
  - recall `valid_at=<before>` still returns them (needs P-A3)
  - idempotent re-post
  - the agentd smoke run is unaffected

**Remember → embed kick**
- [ ] `/v1/remember` enqueues `embed_backfill` for the Context after commit,
  with idempotency key `embed_backfill:<ctx>:<unix/30>`. This is debounced,
  because `Queue.Enqueue` dedupes on `ON CONFLICT (idempotency_key)`.
  Every client gets vectors within seconds.

**Label sanitizer**
- [ ] `internal/ingest/beacon/label.go` + `testdata/beacon/labels.json` (§1).

### Phase 4

**P-A2 · Deferred extraction job**
- [ ] Add jobs kind `turn_extract`. The handler sweeps
  `WHERE extract AND NOT extracted AND role IN ('user','assistant') ORDER BY created_at LIMIT n`.
  It keeps a cursor in the job ledger, reuses the `AppendTurn` extract +
  reconcile half (refactored into `ingest.ExtractTurn`), and uses the
  `testlock.JobsLedger` pattern.
- [ ] Add an `Extraction` hook to `jobs.Env` and have `BuildEnv` resolve it from
  the **same** Context model_config as the inline path, through the same budget
  guard and meter. Nil means the job is a recorded no-op, following the `Env`
  convention. `turn_extract` only calls `ingest.ExtractTurn` (P-A1).
- [ ] Beacon turns are inserted with `extracted=false` and no inline extraction.
  Existing `/v1/sessions/{id}/turns` callers keep inline behaviour.
- [ ] Embeddings: Beacon turns are inserted with `embedding NULL`.
  - Verify that `embed_backfill` covers `turns`; extend it if not.
  - `extract` turns get embedded inside `turn_extract` instead.

**A2 · `POST /v1/ingest/beacon/runtime`** (new package `internal/ingest/beacon/`,
handler `internal/httpapi/ingest_beacon.go`)
- [ ] Dependency: `github.com/asymptote-labs/agent-beacon/pkg/asymptoteobserve@c03b02f`
  (pseudo-version). Check its transitive dependencies are acceptable.
- [ ] Limits:
  - `http.MaxBytesReader(8 MiB)` on the wire body
  - gzip (`Content-Encoding: gzip`) through a **decompressed cap of 64 MiB**, as
    a zip-bomb guard
  - line cap of 10,000
  - 413 when any limit is exceeded
- [ ] Auth: memAuth `write` on `X-Scope` (required, see A-7). Derive the scopes
  for every line, then call `authz.Authorize(write)` on each **distinct derived
  scope** before writing anything. Any denial → **403 for the whole batch**.
  Grants cover sub-scopes through `@>`, so a base grant passes.
- [ ] Per line:
  1. Parse. Accept any `schema_version` whose major version is `1`; otherwise
     count the line as rejected.
  2. Session key (with the B-9 fix).
  3. Scope `<base>.<repo_label|_norepo>.<harness_label>`.
  4. Upsert `beacon_session_map`.
  5. Append the turn using the mapping table below.
- [ ] `ctx/0015_beacon.sql`:
  - `beacon_session_map(session_key TEXT PRIMARY KEY, session_id UUID, scope_path LTREE, last_event_at TIMESTAMPTZ)`
  - `beacon_endpoints(hostname TEXT PRIMARY KEY, last_seen TIMESTAMPTZ, last_batch_accepted INT)`
- [ ] Session upsert: `channel="beacon:"+harness.name`,
  `meta={harness, harness_version, origin, run, hostname, repository, branch, working_directory}`.
- [ ] Transactions: group by session, commit chunks of ≤500 turns per
  transaction under the session advisory lock (A-12).
- [ ] After commit, enqueue `turn_extract` and `embed_backfill` for the Context
  with the same 30s-bucketed idempotency keys, so a finished session is
  vector-searchable and extracted within seconds.
- [ ] Response `200 {accepted,duplicate,rejected,sessions_touched}`. Return 5xx
  only on DB or infrastructure failure.
- [ ] `GET /v1/ingest/beacon/health` (memAuth `read`) → `{ok:true,endpoints:[{hostname,last_seen}]}`.
- [ ] Mapping table. SPEC §3.1, extended with actions actually seen in upstream code:

  | action | kind | role | extract |
  |---|---|---|---|
  | `prompt.submitted` | prompt | user | ✔ |
  | `agent.message`, `assistant.message` | response | assistant | ✔ |
  | `agent.reasoning`, `assistant.reasoning` | reasoning | assistant | ✘ |
  | `tool.invoked`, `tool.call`, `tool.started`, `tool.execution_start`, `mcp.tool_invoked` | tool_call | tool | ✘ |
  | `tool.completed`, `tool.failed`, `tool.result`, `tool.execution_complete` | tool_result | tool | ✘ |
  | `command.executed` | command | tool | ✘ |
  | `file.read` | file_read | tool | ✘ |
  | `file.modified`, `file.created` | file_edit | tool | ✘ |
  | `approval.*` | approval | system | ✘ |
  | `token.usage`, `assistant.usage` | usage | system | ✘ |
  | `session.*`, `agent.detected`, `agent.error` | lifecycle | system | ✘ |
  | else | other | system | ✘ |

- [ ] `text` priority: `prompt.text` → `gen_ai.output`/message text →
  `command.command` → `file.path` → `mcp.server/mcp.tool` → `message`.
  `attrs` = typed blocks verbatim + `beacon.{action,category,fidelity,collection_method,sequence,schema_version}`.
- [ ] Tests (SPEC A4):
  - fixture `testdata/beacon/runtime-sample.jsonl`: upstream sample plus
    2 harnesses × 2 repos
  - idempotency: post the batch twice; the second response is all
    `duplicate`, and turn and extraction counts don't change
  - scope routing, and a 403 on an ungranted derived scope
  - a malformed line is counted as rejected and the rest of the batch still
    commits
  - gzip and plain bodies both work; 413 over the limits
  - only prompt/response turns are extracted, after the job runs
  - valid-time (needs P-A3)
  - `asymptoteobserve` contract test on upstream `sample-event.jsonl`

### Phase 6 — A3 session-close reflect (flag `BRAIN_BEACON_REFLECT`, default off)
- [ ] New job kind `beacon_session_close`. It sweeps `beacon_session_map` for
  sessions where `session.ended` was seen, or `last_event_at < now() - BRAIN_BEACON_SESSION_IDLE`
  (default 30m), and `sessions.closed_at IS NULL`. It then sets `closed_at`.
- [ ] Add `reflect.ReflectSession(ctx, sessionID, persist)`: the input is the
  session's prompt/response turns, not retrieval. Add a `src_kind` value
  `beacon-session-reflect` (check any CHECK constraint on `src_kind`; migrate if
  needed).
- [ ] Test with the model double: one reflect per closed session, and none when
  the flag is off.

**brainsrv docs:** SPEC errata for A-3, A-7, A-8, A-9, A-10. RUNBOOK notes on
the new job kinds and migrations 0014/0015.

---

## 4. Part B — afferent tasks

### Phase 0 — Fork & layout (starts after the Phase 1 gate)
- [ ] `gh repo fork Asymptote-Labs/agent-beacon --fork-name afferent --clone=false`
  (on your GitHub account; confirm which account/org).
- [ ] Make `~/projects/afferent` the repo root:
  - clone the fork there (move the current `SPEC.md` and `PLAN.md` aside first)
  - `git remote add upstream git@github.com:Asymptote-Labs/agent-beacon.git`
  - delete `original-agent-beacon/`, since it's the same commit
- [ ] Branches: `main` mirrors `upstream/main`. `brainsrv` is the working branch.
  Commit `SPEC.md` and `PLAN.md` under `docs/afferent/` on `brainsrv`.
- [ ] **Upstream-drift CI** in a new file `.github/workflows/afferent-upstream.yml`:
  - runs weekly (cron) and on manual dispatch
  - `git fetch upstream && git rebase upstream/main` of `brainsrv` in a scratch
    checkout
  - then runs `make build-hooks-current && go test ./...` plus our package tests
  - on a conflict or a red build: fails loudly and opens or updates a GitHub
    issue `upstream drift: <date>` that lists the conflicting files
  - on green: fast-forwards `main` to upstream and pushes the rebased
    `brainsrv` to `brainsrv-next` for review, never force-pushing `brainsrv`
    itself
  - This way a collision surfaces the week it happens.
- [ ] Add `docs/afferent/REBASE.md`:
  - weekly `git fetch upstream && git rebase upstream/main`
  - the exact list of upstream-file edits (below), so conflicts are expected
    and small
- [ ] Baseline: `cd cli/beacon && make build-hooks-current && go test ./...` green
  before any change. Record the Vector version:
  `brew install asymptote-labs/tap/beacon-vector` or vector ≥ 0.50. Vector
  isn't installed yet.

**Every upstream-file edit allowed across all phases. Anything else is a review
failure:**

| File | Edit |
|---|---|
| `internal/learning/store.go` | one field `hooks StoreHooks`; one call-through line each in `PutMemory`, `ListMemories`, `GetMemory` |
| `cmd/memory.go` | `memoryStore()` → `learning.OpenConfigured(logPath)` |
| `internal/mcpserver/server.go` | `memoryStore()` → `OpenConfigured`; `registerBrainsrvTools()` call in `registerTools`; `degraded`/`history` fields on 2 result structs; `include_history` in 2 input schemas |
| `internal/endpoint/dashboard/memory.go` | `learning.Open` → `learning.OpenConfigured` |
| `internal/endpoint/service/forwarder.go` | optional `LaunchdLabel`/`SystemdUnit`/`Description` on `ForwarderManager` (every `ForwarderLabel`/`ForwarderSystemdUnit` use becomes a defaulting accessor) |

### Phase 2 — B1 memory backend

New package `cli/beacon/internal/brainsrvcfg/`. It holds the config, key-file
reading and the label sanitizer, and is shared with B3.
- [x] Env: `BEACON_MEMORY_BACKEND=brainsrv`, `BEACON_BRAINSRV_URL` (https
  required, except `localhost`), `BEACON_BRAINSRV_SCOPE` (validated against the
  brainsrv scope regex), `BEACON_BRAINSRV_KEY_FILE`.
- [x] `ReadKeyFile` implements B-8. It must start with `spk_`.
- [x] `Label()` + `testdata/labels.json` (§1).

`internal/learning/backend.go` (new):
- [x] `MemoryBackend` interface (SPEC B1).
- [x] `OpenConfigured(logPath) *Store`: returns plain `Open(...)` when
  `BEACON_MEMORY_BACKEND` is unset. If config is present but invalid, log a
  warning and return plain `Open` (never break the CLI).
- [x] `StoreHooks` interface (`AfterPut`, `Search`, `GetMissing`) plus the
  brainsrv implementation. `OpenConfigured` sets `store.hooks`, which is nil by
  default.

`internal/learning/brainsrv.go` (new) — the HTTP client:
- [x] `PutMemory` → `POST /v1/remember`:
  - header `X-Scope: <base>.<project_label>`
  - header `Idempotency-Key: beacon-memory:<id>`
  - body: **facts only (A-2)**, `trust:0.9`, `valid_from: CreatedAt`
  - entity `{local_id:"m", name:<id>, type:"beacon.memory"}`
  - attributes:
    - `summary` = `"<title>\n\n<body>\n\nApplies when: <applicability>"` (the
      recall carrier)
    - `title`, `body`, `kind`, `applicability`, `tags`(json), `candidate_id`,
      `evidence`(json), `project`(json)
    - `beacon.rubric_hash` = `learning.RubricHash()` when the source evaluation
      has one
  - Supersede (`SupersededBy != ""` on a put): look up both entities' brainsrv
    ids, then `POST /v1/entities/{old}/supersede {by:<new>, valid_from:now}` with
    `Idempotency-Key: beacon-supersede:<old>:<new>` (P-A5).
  - Approve/Supersede already route through `PutMemory` (`candidate.go:74,140`),
    so no edits to `candidate.go` are needed.
- [x] `SearchMemories` → `POST /v1/recall {query,k,mode:"memories"}`:
  - Scope: `ProjectPath` → `store.ProjectIDForPath` → project → label. **Never
    derive a scope from the raw request path.** No project → base scope.
  - Keep hits where `entity_type=="beacon.memory"`, deduped by `entity_id`,
    preserving the fused order.
  - For each hit: `GetMemory` from local SQLite by `entity_name`. On a local
    miss, call `GET /v1/entities/{id}` and map the attributes into
    `LearningMemoryV1`.
  - No state filter on our side. Superseded entities never come back from
    recall (P-A5).
  - Non-memory hits go to the `history` slice (for B2).
- [x] `memory_sync` via `ensureSyncSchema()` (B-3): `(memory_id PK, state, attempts, last_error, updated_at)`.
  After a successful local write, a backend error sets the row to `pending` and
  logs it. **Approval never fails.** On success the row is set to `synced`.
- [x] The three call-through lines in `store.go`:
  - `PutMemory`: after the local write, `if s.hooks != nil { s.hooks.AfterPut(m) }`
  - `ListMemories`: `if s.hooks != nil && q.Q != "" { if r, ok := s.hooks.Search(q); ok { return r, nil } }`.
    The degraded state lives on the hooks value, which is safe because stores
    are created per request.
  - `GetMemory`: on a local miss, `if s.hooks != nil { return s.hooks.GetMissing(id) }`
- [x] `cmd/memory_brainsrv.go` (new, self-registering):
  - `beacon memory brainsrv status`: GET health + sync counts
  - `beacon memory brainsrv sync [--all]`: retry pending/failed rows; `--all`
    enqueues every approved memory
- [x] Tests:
  - golden JSON for the remember body
  - label vector
  - `httptest` fake brainsrv:
    - approve → exactly 1 remember call carrying the Idempotency-Key
    - brainsrv down → approval succeeds and the sync row is `pending`; `sync`
      flushes it
    - Supersede → the replacement remember, then one supersede call, in
      order
  - `go test ./...` with no env set stays byte-identical to upstream behaviour

**Phase 2 implementation notes** (as built on `brainsrv`):
- Entity-id lookup for supersede: the `/v1/remember` response's `resolved.m`
  is stored in `memory_sync.entity_id` (an extra column next to the planned
  ones). On a cache miss the backend re-sends that memory's own remember:
  brainsrv replays a recorded `Idempotency-Key` with the original result (and
  creates the entity if it was never written), so the id is exact, needs no
  name search, and the call is safe to repeat. `sync --all` ignores the cache.
- `OpenConfigured` takes the same store path as `Open`, so each call site
  change is exactly `Open` → `OpenConfigured`.
- Supersede `valid_from` is the locally recorded supersede time (the old
  memory's `UpdatedAt`), not the send time, so a retried supersede is the
  identical request under the same `Idempotency-Key`.
- A relayed `ProjectPath`/`ProjectID` the store does not know produces no
  recall at all (empty result), mirroring upstream's "scopes to nothing".
- `make check-labels` is the Go test `TestLabelVectorMatchesBrainsrv` (the
  Makefile is upstream's and not in the edit table). It diffs against
  `$BRAINSRV_LABELS_PATH`, else `../brainsrv/testdata/beacon/labels.json`,
  and skips when neither exists.
- Backend state for B2: `learning.BackendState(store)` returns `degraded`
  and the `history` hits of the last search.

### Phase 3 — B2 MCP
- [x] `search_memory` / `get_memory_context`: backend results come through
  `ListMemories`. `degraded: true` is set when the fallback ran.
- [x] `include_history` (default false) → `get_memory_context` returns a
  `history` array: `{text, table, known_at, src_kind}` from non-memory hits,
  labelled `"source":"brainsrv-episodic"`.
- [x] `internal/mcpserver/brainsrv_tools.go` (new):
  - `registerBrainsrvTools()` registers `recall_brain(query, scope?)` **only
    when a backend is configured**, so `HasExpectedTools` and the upstream
    tests don't change
  - the description is marked experimental
  - `scope` must be at or under the base scope; anything else is rejected
- [x] Tests:
  - recall order preserved
  - a failing backend → SQLite fallback + `degraded`
  - `memory_cross_harness_test.go` passes unchanged with no backend

**Phase 3 implementation notes** (as built on `brainsrv`):
- `server.go` carries only the table's edits: the `degraded`/`history`
  fields (after a blank line, so gofmt leaves upstream's struct lines alone)
  and the `registerBrainsrvTools()` call. The `include_history` schema edit
  was not needed.
- With a backend configured (`learning.BrainsrvOf` of a fresh
  `OpenConfigured` store; a broken config or unreadable key counts as none),
  `registerBrainsrvTools()` swaps the `search_memory` and
  `get_memory_context` handlers in place (list order unchanged) for copies in
  `brainsrv_tools.go` that read `learning.BackendState` of the store they
  used. Without one, tools, schemas and handlers are exactly upstream's.
  Drift guard: `TestBrainsrvMemoryToolsMatchUpstreamShape` compares the
  fallback output with upstream's handler output field for field.
- `include_history` is added to `get_memory_context`'s schema only when a
  backend is configured. History is capped at the context limit (5) and each
  text is trimmed like memory bodies (1200 chars).
- A listing without `q`/`task` never calls the backend (B1's hook), so it
  never reports `degraded`.
- `recall_brain(query, scope?, limit?)`: `scope` defaults to the base scope
  and must pass `brainsrvcfg.Config.Covers` (valid ltree, equal to the base or
  `<base>.` prefixed), checked before any request. Returns brainsrv's ranked
  hits unfiltered (memories and history together) and brainsrv's own
  `degraded` list as `brainsrv_degraded`. `limit` (default 8, max 20) is the
  recall `k`.


### Phase 5 — B3 forwarder
New package `internal/endpoint/brainsrv/`, cloned from `asymptote/` without
enrollment, account, reconnect or privacy transforms. Keep the
`# BEACON_PRIVACY_TRANSFORMS` marker as the future hook.
- [x] `pack/vector.toml.tmpl`:
  - runtime source only, `read_from` = `end` | `beginning` (`--backfill`)
  - its own `data_dir` (`…/afferent-brainsrv`), so checkpoints are separate
    from managed
  - http sink `uri=<url>/v1/ingest/beacon/runtime`
  - `SECRET[beacon.brainsrv_key]` (file backend, JSON `{"brainsrv_key":…}`, 0600)
  - `request.headers.X-Scope=<scope>`
  - gzip, ndjson, batch 5 MB / 5,000 lines, disk buffer 512 MiB
  - healthcheck `uri=<url>/v1/ingest/beacon/health`
  - Render the literals the same way as `RenderVectorConfig`, so the unit
    needs no env.
- [x] Commands, in `cmd/endpoint_brainsrv.go` (new, self-registering on
  `endpointCmd`):
  - `connect --url --scope --key-file [--backfill]`:
    - `ReadKeyFile` (the member's own key, §1.2)
    - check with GET health that the key can read and write `--scope`; refuse
      otherwise
    - write the secrets file 0600 (`writeFileAtomic`)
    - render the Vector config
    - run the same preflight and `ValidateVectorConfig`
    - `ForwarderManager{Label:"com.afferent.brainsrv-forwarder", SystemdUnit:"afferent-brainsrv-forwarder.service"}`
  - `status`: service state, Vector checkpoint offset, brainsrv
    `last_seen` from GET health
  - `disconnect`, `print-config`, `install-pack --output`, `validate`
- [x] Coexistence: different label, unit, data_dir and secrets file. Test that
  both render and that neither's state paths overlap.
- [x] Tests:
  - rendered config matches a golden file
  - `validate` against Vector ≥ 0.50 (skip if Vector is absent)
  - key-file permission rejection
- [ ] E2E: use the repo's `self-verify-beacon-in-sandbox` skill, pointed at a
  dev brainsrv.
  - Local live E2E done (2026-09-24, Vector 0.56.0, brainsrv beacon-phase4 on
    localhost, no service installed): the hand-run pack delivered the 22-line
    A2 fixture to `<scope>.{alpha,beta,repo,_norepo}.<harness>` with
    `health?scope=` listing the 3 hostnames; `turn_extract` touched only
    prompt/response turns; a restart on the same data_dir re-sent nothing; a
    fresh data_dir with `read_from = beginning` got
    `{"accepted":0,"duplicate":23}` and the turn count stayed put. The
    env-gated `internal/endpoint/brainsrv/live_test.go` repeats it through
    `Connect` (fake service manager, real Vector on the rendered config),
    including the 0644 key-file and out-of-grant scope refusals. The sandbox
    run is still open.
- Implementation notes (Phase 5):
  - B-6 field names: `ForwarderManager` already has a `Label()` method, so the
    optional fields are `LaunchdLabel`, `SystemdUnit` and `Description`
    (zero values keep `ForwarderLabel`, `ForwarderSystemdUnit` and the
    Asymptote description).
  - The healthcheck URI carries `?scope=<base>` (Vector's healthcheck sends
    the bearer key but not `request.headers`). `connect` also POSTs an empty
    NDJSON batch with `X-Scope` as the write-grant probe; 401/403 on either
    refuses.
  - `--backfill` renders `read_from = "beginning"` and clears
    `<data_dir>/beacon_runtime/` (Vector's file-source checkpoints), since
    `read_from` only applies to files without a checkpoint. A later connect
    without it renders `end` and keeps the checkpoints.
  - Verified against the official Vector 0.56.0: `vector validate` accepts the
    render and the hand-run template, and a live run sends gzip NDJSON with
    `X-Scope`, bearer key and `HEAD …/health?scope=…`; checkpoints land in
    `<data_dir>/beacon_runtime/checkpoints.json`.
  - Review fixes: `--backfill` also includes the retained archives
    (`writer.RetainedLogPaths`) and waits for the stopped forwarder to be gone
    before clearing checkpoints; a changed or unknown (url, scope) empties the
    data dir (`data-destination.json`); a failed connect restores the previous
    key, config, checkpoints and buffer and restarts the previous forwarder;
    literals escape `$` as `$$` and the log path is TOML-quoted; `status` runs
    the write probe and flags undelivered log writes; the launchd job logs to
    `<state dir>/vector.log` via a brainsrv-owned plist writer (no further
    `forwarder.go` edit); `beacon endpoint uninstall` tears the brainsrv
    forwarder down through a RunE wrapper in the new
    `cmd/endpoint_brainsrv_uninstall.go` (no edit to upstream cmd files or
    `lifecycle.go`).

### Packaging
- [ ] `cli/beacon/.goreleaser.afferent.yaml` (new):
  - `afferent` binary alias (a symlink to `beacon` in archives)
  - your own Homebrew tap
  - keeps the `beacon-vector` dependency
- [ ] README section `docs/afferent/README.md` with MIT attribution to Asymptote Labs.

### Later — B4 CI forward (B-10)
Deferred. It needs `Destinations.Brainsrv`, 5 touch points in `forward.go`, and
exporter config.

---

## 5. Acceptance (SPEC §6, adjusted)

Run on the dev-local stack. `docker compose up`, then
`brainsrv migrate`, `BRAIN_JOBS_WORKERS≥1`.

1. Build the fork, then `beacon endpoint install --harness claude,codex,cursor`.
2. Mint a member key for member `m` with `narrow_scope=ws.dev.people.m.harness`
   and `read`+`write` grants there. Save it to `~/.brainsrv/beacon.key` (0600).
   Then run
   `beacon endpoint brainsrv connect --url http://localhost:8077 --scope ws.dev.people.m.harness --key-file ~/.brainsrv/beacon.key`.
   A `--scope` outside the grant is refused.
3. Run Claude Code and Codex sessions in repo `x`:
   - sessions appear under `ws.dev.people.m.harness.x.claude_code` and
     `….x.codex`
   - turns ordered by `occurred_at`, with tool/file `attrs`
   - after `turn_extract` runs, facts exist for the prompt/response turns only
4. `beacon memory approve …` → the memory is recallable under `ws.dev.people.m.harness.x`.
   BM25 finds it immediately, and vector recall works within about 30s
   (debounced `embed_backfill` enqueue).
5. A Cursor session in repo `x` calls `search_memory` and gets the lesson from
   Claude.
6. Kill brainsrv and approve another memory. The approval succeeds and
   `status` shows 1 pending. Restart brainsrv, run `sync`, and status shows 0.
7. `connect --backfill` again → the ingest response is all `duplicate`, and the
   turn count doesn't change.
8. *(new)* Supersede a memory. `search_memory` **and agentd's recall** no longer
   return the old one, and `recall valid_at=<before supersede>` still shows it.

---

## 6. Risks

- **Upstream churn in `store.go`/`server.go`.** Edits are hook-sized and
  listed. The weekly rebase keeps the drift small.
- **`asymptoteobserve` schema drift.** The pinned pseudo-version plus the
  contract test turn it into a failing test, not silent loss. Bump the
  pseudo-version deliberately.
- **Extraction noise from high-volume prompt/response turns.** Add a
  per-Context switch (`beacon_extract=off`) in the calibration settings if
  needed (SPEC §5.4).
- **Hebrew recall** is vector-only (P14). It works once the debounced embed
  kick runs, within seconds.
- **Two consumers of one brain.** Any convention only afferent understands is
  drift waiting to happen. Rule: semantics live in brainsrv (P-A5), and clients
  don't filter server truth.
- **Index build on `turns` in migration 0014** runs inside a transaction and
  locks large Contexts briefly. Schedule the migration.

---

## 7. Changelog

**v0.2**: review folded in.
- Entity-level supersede moved server-side into Phase 1 (P-A5). The
  `state`-attribute convention and the client-side filter are removed.
- One `ingest.ExtractTurn` shared by the inline path and `turn_extract`,
  with the same model_config and `ExtractorVersion`.
- remember and Beacon ingest enqueue `embed_backfill` (30s debounce) instead
  of waiting on the 10-min schedule.
- Identity: a per-member, per-install key with `narrow_scope`, and the base
  scope `ws.<id>.people.<m>.harness` (§1.2).
- `store.go` gets a single `StoreHooks` seam.
- A weekly upstream-rebase CI job.
- Phase 1 lands first and is gated on a new agentd live test (§2.1) before the
  fork starts. (The `smoke_brainsrv.sh` the review mentioned doesn't exist
  yet; §2.1 creates it.)

**v0.3**: authsrv end-to-end direction (★ section).
- Own `afferent` binary, a built-in forwarder instead of Vector, and brainsrv
  MCP for reads.
- Recorded the authsrv refresh-token blocker, the grant-template substitution
  gap, and the D1–D8 work list.
