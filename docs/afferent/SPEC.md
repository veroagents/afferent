# SPEC — Beacon ↔ brainsrv integration

Status: draft v0.1 · 2026-09-24 · owner: Andrew
Upstream studied: `Asymptote-Labs/agent-beacon` @ `c03b02f` (2026-09-23), MIT.

## 1. Goal

Get Beacon's cross-harness capture and "self-improving memory" loop with
**brainsrv as the memory and history store**, not Beacon's local SQLite or
Beacon Managed.

Beacon already captures 25+ agent harnesses (Claude Code, Cursor, Codex,
OpenCode, Cline, …) and normalizes them into one event model in
`~/.beacon/endpoint/logs/runtime.jsonl`. It then runs
evaluate → candidate → review → approved memory, and serves memory to agents
over MCP. We keep all of that and replace two things:

1. **Memory backend.** Approved memories are written to brainsrv, and MCP memory
   search reads from brainsrv `recall` (hybrid vector/BM25/graph ranking)
   instead of SQLite substring match.
2. **Activity destination.** The raw normalized event stream is forwarded into
   brainsrv as episodic sessions, so brainsrv holds the full cross-harness
   history with provenance and valid-time.

Two deliverables, in two repos:

| Part | Repo | What |
|---|---|---|
| A | `brainsrv` (this repo) | `POST /v1/ingest/beacon/*` endpoint + turn-model widening |
| B | **afferent** — Beacon fork (`~/projects/afferent`) | brainsrv memory backend + `beacon endpoint brainsrv` forwarder |

Build order: **A1 → B1 → B2 → A2/A3 → B3 → B4**. B1/B2 (memory) is usable
before any activity forwarding exists.

### Non-goals (v1)

- Changing any Beacon harness adapter, collector, normalizer, threat rules or
  dashboard UI.
- Forwarding `inventory_state.jsonl` (endpoint inventory).
- Replacing Beacon's evaluator (jev rubric) with brainsrv extraction. Both
  coexist, see §5.4.
- Evidence Gateway attestation of Beacon events. Noted in §8 as a follow-up
  only.
- Removing Beacon Managed code from the fork. It stays; we just never enable it.

### Fork hygiene (hard rule)

Every Beacon change is **additive**: new packages, new files, one config
switch, and minimal call-site edits (listed explicitly below). No rewrites of
upstream files. The fork must rebase cleanly on upstream weekly, because new
harness adapters are the reason we're using Beacon at all.

---

## 2. Relevant upstream facts (verified in code)

**Shared types module.** `pkg/asymptoteobserve` is a *standalone Go module*
(`github.com/asymptote-labs/agent-beacon/pkg/asymptoteobserve`, go 1.25):
- `Event` is the JSONL line.
- `LearningMemoryV1`, `LearningCandidateV1`, `LearningEvaluationV1` and
  `LearningProjectV1` are the memory types.

brainsrv imports this module directly for the event type (pin a version).

**Event line shape (key fields).**
- Top level: `timestamp`, `sequence`, `event{id,kind,action,category,fidelity}`,
  `harness{name,version,collection_method}`, `session{id,working_directory}`,
  `run{…}`, `origin`, `repository`, `branch`, `message`.
- Typed blocks: `tool`, `command`, `file`, `mcp`, `approval`, `prompt`,
  `content`, `gen_ai`.
- `event.id` is derived from the event itself, so a hook capture and an OTLP
  capture of the same action share the id. **It is the idempotency key.**

**Actions we care about** (seen in code; treat the list as open-ended):
- Session lifecycle: `session.started`, `session.ended`,
  `session.compacting`/`compacted`
- Prompts and messages: `prompt.submitted`, `agent.message`,
  `assistant.message`, `agent.reasoning`
- Tools: `tool.invoked`, `tool.completed`, `tool.failed`
- Other activity: `command.executed`, `file.read`, `file.modified`,
  `mcp.tool_invoked`, `approval.requested|allowed|denied`, `token.usage`,
  `agent.error`
- Fallback: `trace.unclassified`

**Memory store.** `cli/beacon/internal/learning/store.go` has a concrete
SQLite `Store{dbPath}`, opened via
`learning.Open(learning.PathForRuntimeLog(logPath))` at three call sites:
- `cmd/memory.go:485` (`memoryStore()`)
- `internal/mcpserver/server.go:382` (`memoryStore()`)
- `internal/endpoint/dashboard/memory.go:11`

It exposes `PutMemory`, `ListMemories(Query)` and `GetMemory(id)`, plus the
equivalents for evaluations and candidates.

`Query{ProjectPath, ProjectID, State, Kind, Q, Limit, Page}`:
- `ProjectPath` is the trust boundary. It is matched against known projects,
  never resolved on disk. Preserve this.
- `Q` is substring match (`matchesText`).

**Approval flow.** Memories are created in `ApproveCandidate` and read back in
`SupersedeCandidate` / `CandidateMemoryForSkill`
(`internal/learning/candidate.go`, `skills.go`).

**MCP tools.** `internal/mcpserver/server.go` exposes these tools:
- Memory: `search_memory`, `get_memory`, `get_memory_context`.
  - `search_memory` and `get_memory_context` call `ListMemories`.
  - `get_memory` calls `GetMemory`.
- Activity: `search_activity`, `summarize_activity`, `get_activity_event`,
  `list_activity_filters`.

**Managed forwarder (template for B3).** `internal/endpoint/asymptote/`:
- Vector tails `runtime.jsonl` and posts it via an `http` sink:
  - URL: `${BEACON_ASYMPTOTE_INGEST_URL}/v1/ingest/runtime`
  - `compression="gzip"`, bearer token from a Vector `SECRET[...]` file
    backend
  - `Content-Type: application/x-ndjson`, newline-delimited framing
  - batches ≤ 5 MB / 5,000 lines
- Health goes to `/v1/ingest/health`.
- Runtime forwarding starts at end-of-log.
- Runs as launchd `com.beacon.endpoint.asymptote-forwarder`.
- Pack template lives in `pack/vector.toml.tmpl`.

---

## 3. Part A — brainsrv

### A1. Turn-model widening

`POST /v1/sessions/{id}/turns` currently takes `{role, text}`. Add these
optional fields, all backward compatible:

```yaml
role: { enum: [user, assistant, tool, system] }   # unchanged
text: string                                       # now optional if attrs present
kind: string            # e.g. prompt, response, reasoning, tool_call, tool_result,
                        # command, file_read, file_edit, mcp, approval, usage, lifecycle
source_event_id: string # idempotency; unique per (context, session)
occurred_at: date-time  # valid-time of the event; ingest time stays transaction-time
attrs: object           # JSONB — typed context (tool/command/file/mcp/approval/gen_ai…)
extract: boolean        # default true; false = store in episodic record only
```

Requirements:
- **Migration.** Add columns `kind`, `source_event_id`, `occurred_at` and
  `attrs jsonb` to the turns table in every Context schema, following the
  existing per-schema migration mechanism. Add a partial unique index on
  `(session_id, source_event_id) WHERE source_event_id IS NOT NULL`.
- **Duplicates.** A duplicate `source_event_id` returns `200` with the
  existing turn. It must not create a new turn and must not re-extract.
- **Ordering.**
  - `occurred_at` becomes the ordering key for read-back
    (`GET …/turns`) when present.
  - `seq` remains insertion order.
  - The `sequence` field from Beacon goes into `attrs.beacon.sequence` as a
    tiebreaker.
- **Extraction gate.** `extract:false` skips the inline extraction hook
  entirely.
- **Contract updates.** Update `api/openapi.yaml`, the gRPC proto, and the
  TS/Python SDKs. The SDK drift test must stay green.
- **Existing hook.** `adapters/claude-code-hook` keeps working unchanged.

### A2. `POST /v1/ingest/beacon/runtime`

This is wire-compatible with the Vector http sink above, so Part B3 is just a
different URL and key.

**Request contract.**
- Auth: `Authorization: Bearer spk_…`, a normal brainsrv API key. It needs
  `write` on the base scope.
- Base scope: from `X-Scope` if present, otherwise from a key-level default
  scope. Vector can send a static header; B3 sets it.
- Body: `Content-Encoding: gzip` (must also accept uncompressed),
  `Content-Type: application/x-ndjson`, one Beacon `Event` per line.
- Limits: accept ≤ 8 MiB compressed / 10,000 lines (same as upstream ingest).
  Return `413` beyond that.

**Per line:**
1. Parse into `asymptoteobserve.Event`.
   - On unknown schema_version or a malformed line, count it as rejected and
     continue. **Never fail the batch for a bad line.**
2. Derive the **session key**:
   - Normally `harness.name + ":" + session.id`.
   - If there is no session, use `run.provider + ":" + run.id`.
   - If neither exists, use the bucket `unsessioned:<harness>:<date>`.
3. Derive the **scope**: `<base>.<repo_label>.<harness_label>`.
   - `repo_label` is the sanitized repository basename, from `repository` or
     else `session.working_directory`.
   - Labels are sanitized to the ltree charset `[A-Za-z0-9_]`, lowercased,
     max 64 chars, with a stable 8-char blake3 suffix on collision or
     truncation.
   - If there is no repo, use `<base>._norepo.<harness_label>`.
4. Upsert the brainsrv session for (scope, session key):
   - `channel = "beacon:" + harness.name`
   - `meta = {harness, harness_version, origin, run, endpoint.hostname,
     repository, branch, working_directory}`
   - Keep a lookup table `beacon_session_map(session_key → session_id)` per
     Context.
5. Append a turn:
   - `source_event_id = event.id`
   - `occurred_at = timestamp`
   - `kind` / `role` / `extract` from the §3.1 mapping table
   - `attrs` = the event's typed blocks verbatim, plus
     `beacon.{action,category,fidelity,collection_method,sequence,schema_version}`
   - `text` = the best human-readable text (prompt/content text, assistant
     text, the command line, `file.path`, the MCP tool name, or `message`)

**Responses.**
- `200 {accepted, duplicate, rejected, sessions_touched}`.
- `5xx` only for whole-batch infrastructure failure. Vector retries on 5xx,
  and `event.id` idempotency makes retries safe.
- `4xx` only for auth, scope or size problems. Vector drops rather than
  retries these, which is correct.

**Batching.** Insert per batch in one transaction per session where practical.
Run extraction **after** commit, asynchronously via the existing jobs/outbox.
Ingest latency must not depend on the LLM.

**Health.** `POST /v1/ingest/beacon/health` accepts the same NDJSON and just
records last-seen per endpoint hostname (for `status`). It returns `200`.

**Privacy.**
- Beacon content arrives already locally sanitized per Beacon's privacy mode.
- brainsrv's existing PII scan runs on `text` as it does for any turn.
- `attrs` are not embedded.

#### 3.1 Action → turn mapping

| Beacon `event.action` | kind | role | extract |
|---|---|---|---|
| `prompt.submitted` | prompt | user | **true** |
| `agent.message`, `assistant.message` | response | assistant | **true** |
| `agent.reasoning`, `assistant.reasoning` | reasoning | assistant | false (see note) |
| `tool.invoked`, `mcp.tool_invoked` | tool_call | tool | false |
| `tool.completed`, `tool.failed` | tool_result | tool | false |
| `command.executed` | command | tool | false |
| `file.read` / `file.modified` | file_read / file_edit | tool | false |
| `approval.*` | approval | system | false |
| `token.usage`, `assistant.usage` | usage | system | false |
| `session.*`, `agent.detected`, `agent.error` | lifecycle | system | false |
| anything else / `trace.unclassified` | other | system | false |

Note on reasoning: stored but never extracted. This is consistent with the
Evidence Gateway decision to scope reasoning out of v1 records. Reasoning is
kept for incident review, not turned into memory.

### A3. Session close → reflection (optional, flag-gated)

When `session.ended` arrives, or no events arrive for a session for
`BEACON_SESSION_IDLE` (default 30m, swept by the existing jobs runner):
- Enqueue a `reflect` over that session's turns with `persist=true`.
- The result is written to the session's scope with provenance
  `source=beacon-session-reflect`.
- Flag: `BRAINSRV_BEACON_REFLECT=on|off` (default **off**). Turning it on is an
  explicit decision because it costs model calls per session.

### A4. Tests (brainsrv)

- **Fixture.** `testdata/beacon/runtime-sample.jsonl`: take upstream's
  `internal/endpoint/asymptote/pack/sample-event.jsonl` and extend it with a
  prompt/response/tool/file sequence across 2 harnesses and 2 repos.
- **Idempotency.** Post the same batch twice. The second call reports all
  duplicates, and the turn count and extraction count are unchanged.
- **Scope routing.** Events land under the expected
  `<base>.<repo>.<harness>` paths. A key without a grant on a derived
  sub-scope gets `403` for the batch. (Decide: deny, or clamp to base. Spec
  says **deny**, consistent with deny-by-default.)
- **Extraction gate.** Only prompt/response turns reach the extraction hook
  (use the model-hook double).
- **Malformed line.** Counted as rejected, and the rest of the batch still
  commits.
- **gzip.** Both gzip and plain bodies work, and `413` fires over the limit.
- **Valid-time.** `recall` with `filters.valid_at` before an event's
  `occurred_at` does not return memories extracted from it.

---

## 4. Part B — afferent (Beacon fork)

Codename: **afferent**. Afferent nerves carry signals from the periphery
into the brain; this fork carries agent signals from endpoints into brainsrv.
The codename applies to the outside only: repo, release artifacts, Homebrew
formula, launchd label (`com.afferent.brainsrv-forwarder`), README, and an
optional `afferent` command alias added in packaging. The Go module path,
package names, and the in-code `beacon` command stay upstream's, so rebases
stay clean and MIT attribution is obvious.

Setup:
- Fork upstream to your own GitHub account as `afferent` and clone it to
  `~/projects/afferent`.
- Branch `brainsrv`. Keep `main` as a clean mirror of upstream.
- All new code lives in new packages. The call-site edits are listed below;
  there should be no others.

### B1. `learning` memory backend

New file `cli/beacon/internal/learning/backend.go`:

```go
// MemoryBackend is where approved memories live canonically.
type MemoryBackend interface {
    PutMemory(ctx context.Context, m asymptoteobserve.LearningMemoryV1) error
    GetMemory(ctx context.Context, id string) (asymptoteobserve.LearningMemoryV1, bool, error)
    SearchMemories(ctx context.Context, q Query) ([]asymptoteobserve.LearningMemoryV1, error)
}
```

New file `cli/beacon/internal/learning/brainsrv.go` implements it over HTTP.

**Write-through.** `Store` gets an optional backend field. The
`OpenWithBackend(path, backend)` constructor is added; `Open` is unchanged.

`Store.PutMemory`:
- Always writes SQLite first. Local stays the source for the review workflow
  and for offline use.
- Then calls `backend.PutMemory`.
- Backend failure behaviour:
  - Log it and record it in a new `memory_sync` table:
    `(memory_id, state pending|synced|failed, attempts, last_error, updated_at)`.
  - Do **not** fail the approval.
  - `beacon memory brainsrv sync` retries pending/failed rows.

**Reads.**
- If a backend is configured and `Query.Q != ""`, `ListMemories` uses
  `backend.SearchMemories`, then falls back to SQLite on error, marking the
  result `degraded` in the MCP response.
- `GetMemory` reads SQLite first and uses the backend only when the memory is
  missing locally, e.g. it was approved on another machine.

**Memory → brainsrv mapping.** Use `POST /v1/remember` with both `text` and
`facts`:
- **Scope:** `<base>.<project_label>`.
  - `project_label` is the sanitized repo basename from
    `LearningProjectV1.RemoteURL`, falling back to `Path`, falling back to
    `ID`.
  - Same sanitizer rules as A2 step 3. It must yield the **same label** as A2
    for the same repo, so memories and history co-locate. **Put the sanitizer
    in one shared place**: brainsrv exposes it as
    `GET /v1/ingest/beacon/label?repo=…`, or both sides copy one tiny tested
    function with a shared test vector file. Choose the shared test vector
    (no network dependency on the approve path).
- **Idempotency:** `Idempotency-Key: beacon-memory:<memory.ID>`.
- **Text:** `text = "<title>\n\n<body>\n\nApplies when: <applicability>"`.
  This is the part that gets embedded for recall.
- **Facts:**
  - One entity `{local_id:"m", name: memory.ID, type:"beacon.memory"}`.
  - Attributes on `m`: `title`, `body`, `kind`, `applicability`, `tags`
    (json), `candidate_id`, `evidence` (json: trace ids, event ids, harness),
    `project` (json), `beacon.rubric_hash` (taken from the source evaluation
    when available).
- **Trust / time:** `trust` = 0.9 (human-reviewed); `valid_from` =
  `memory.CreatedAt`.

**Supersede** (`SupersedeCandidate`):
- Write the replacement memory as above.
- Then write attribute `superseded_by = <replacement id>` on the old entity,
  with `valid_from` = the supersede time.
- **Verify during implementation:** does brainsrv's reconcile close the old
  attribute versions' valid-time, so recall excludes superseded memories by
  default? If not, filter in `SearchMemories` on `superseded_by` and file a
  brainsrv follow-up.

**Search** (`SearchMemories`):
- Call `POST /v1/recall`:
  - `X-Scope` = the project scope, or `<base>` when no project is given
    (cross-project)
  - body `{query: q.Q, k: q.Limit, mode: "memories"}`
- Map hits back to `LearningMemoryV1` via the `beacon.memory` entity
  attributes. Drop hits that are not `beacon.memory` entities unless
  `include_history=true` (B2).
- Preserve the `ProjectPath` trust boundary: resolve `ProjectPath` →
  `ProjectID` through the local `Store.ProjectIDForPath` **before** building
  the scope. Never derive a scope from an unverified request path.

**Config.** Env first; a config-file key can come later. Following Beacon's
own pattern, secrets are never passed as flags.
- `BEACON_MEMORY_BACKEND=brainsrv`
- `BEACON_BRAINSRV_URL`
- `BEACON_BRAINSRV_SCOPE` (base scope, e.g. `org.me.coding`)
- `BEACON_BRAINSRV_KEY_FILE`: path to a `0600` file holding `spk_…`. Reject
  looser permissions.

**Call-site edits (the only edits to upstream files in B1):**
1. `cmd/memory.go` `memoryStore()` → `learning.OpenConfigured(logPath)`
2. `internal/mcpserver/server.go` `memoryStore()` → same
3. `internal/endpoint/dashboard/memory.go` → same

`OpenConfigured` lives in `backend.go`. It returns the plain `Open(...)` when
no backend is configured, so behaviour is unchanged by default.

**New CLI** (in a new file `cmd/memory_brainsrv.go`):
- `beacon memory brainsrv status`: backend reachability plus sync-table
  counts.
- `beacon memory brainsrv sync [--all]`: retry pending/failed rows; `--all`
  backfills every existing approved memory.

### B2. MCP memory tools on brainsrv

Keep the existing tool names and schemas so agents and skills don't change.
- `search_memory` and `get_memory_context` get recall-ranked results for free
  via B1.
- Add an optional `include_history: bool` argument (default false). When true,
  `get_memory_context` also returns top episodic hits (non-memory recall hits:
  past prompts, responses and extracted facts from A2) in a separate
  `history` array, clearly labelled. This is how
  "Cursor sessions improve Codex" works end to end.
- Add one new tool `recall_brain(query, scope?)`: a thin pass-through to
  brainsrv `recall` for agents that want everything, with no Beacon-shape
  mapping. Mark it experimental.
- Do **not** proxy brainsrv's own MCP server. Agents that want the full brain
  can register brainsrv MCP directly. This tool exists only for the
  Beacon-only setup.

### B3. `beacon endpoint brainsrv` forwarder

New package `cli/beacon/internal/endpoint/brainsrv/`, cloned from
`internal/endpoint/asymptote/`. **Drop:** enrollment, account/device-key
flows, reconnect, and privacy-mode transforms (keep the hook for later).

- **Pack:** `pack/vector.toml.tmpl` is the asymptote template with these
  changes:
  - `uri = "${BEACON_BRAINSRV_URL}/v1/ingest/beacon/runtime"` and `…/health`
  - bearer token = `SECRET[beacon.brainsrv_key]` read from the 0600 secrets
    file
  - static header `X-Scope: ${BEACON_BRAINSRV_SCOPE}`
  - no inventory source or sink
- **Commands:**
  `beacon endpoint brainsrv connect --url <u> --scope <s> --key-file <f> [--backfill]`
  plus `status`, `disconnect`, `print-config`, `install-pack --output <dir>`
  and `validate`.
  - `connect` writes the secrets file (0600), renders the Vector config, and
    runs the same preflight/`ValidateVectorConfig` as asymptote.
  - `connect` installs launchd `com.afferent.brainsrv-forwarder`
    (systemd: `afferent-brainsrv-forwarder.service`).
  - `--backfill` sets `read_from="beginning"` for the first run (safe because
    of `event.id` idempotency). Default is `end`, like managed.
- **Status:** `status` shows service state, the Vector checkpoint offset, and
  brainsrv last-seen from `/v1/ingest/beacon/health`.
- **Wiring:** register the subcommand in `cmd/endpoint.go`. This is one edit
  to an upstream file; keep it a single registration line.
- **Coexistence:** Beacon Managed and brainsrv forwarding must be able to run
  at the same time (separate services, separate checkpoints). No shared state.

### B4. CI forward (later, small)

Add `brainsrv` to `internal/ci/forward.go` as a `--forward` target:
- Posts the completed `runtime.jsonl` to `/v1/ingest/beacon/runtime` in
  5,000-line gzip chunks.
- Token comes from env `BEACON_CI_BRAINSRV_KEY` only, never a flag.
- Best-effort: never fails the job, matching the Splunk/Falcon behaviour.

### B5. Tests (Beacon fork)

- **Unit.** Test the mapping `LearningMemoryV1` ↔ `/v1/remember` body with a
  golden JSON fixture. Test the label sanitizer against the shared test vector
  file, which is also used by brainsrv.
- **Fake brainsrv.** Use an `httptest.Server`:
  - Approve → exactly one remember call, carrying the idempotency key.
  - Backend down → approval still succeeds and a `memory_sync` row goes
    `pending`; `sync` flushes it.
  - Supersede → the replacement write and the `superseded_by` write, in that
    order.
- **MCP.** `search_memory` with a backend returns recall order; with a
  failing backend it falls back to SQLite, marked `degraded`.
  `internal/mcpserver/memory_cross_harness_test.go` must still pass unchanged
  with no backend configured.
- **Forwarder.** `validate` passes with Vector ≥ 0.50, and the rendered config
  matches a golden file.
- **Default behaviour.** With no backend configured, the full upstream
  `go test ./...` is green and behaviour is byte-identical.

---

## 5. Design notes

1. **Why the forwarder uses a server-side mapper.** Session grouping, dedupe,
   and scope routing are stateful. Doing them in brainsrv keeps Vector as a
   dumb pipe. The same endpoint then serves CI artifacts and S3/GCS
   cloud-agent snapshots for free, since all of them use the JSONL contract.
2. **Why write-through, not replace.** Beacon's review workflow (evaluations,
   candidates, approve/reject/supersede) and its dashboard all read the local
   SQLite store. Replacing the whole store would touch far more upstream code.
   brainsrv owns canonical approved memory and search; SQLite keeps workflow
   state and an offline cache.
3. **Valid-time is the differentiator.** `occurred_at` on turns and
   `valid_from` on memories let brainsrv answer "what did the brain know when
   this agent acted". Beacon's own replay can't answer that.
4. **Two learning loops coexist.**
   - Beacon's evaluator produces **reviewed lessons**: high trust, human
     approved.
   - brainsrv inline extraction on prompt/response turns produces **raw facts**:
     lower trust, automatic.
   - Both are recallable and distinguished by `trust` and entity type. If
     noise is a problem, turn off A2 extraction per Context and rely on
     Beacon's loop plus A3 reflection.
5. **Hebrew caveat.** Lexical/BM25 fusion is still english-only (the P14 gap).
   Hebrew prompts recall via the vector signal only. This is acceptable for v1.

## 6. Acceptance (end to end, on the dev-local stack)

1. `brew install` or build the fork, then run
   `beacon endpoint install --harness claude,codex,cursor`.
2. Run
   `beacon endpoint brainsrv connect --url http://localhost:8077 --scope org.me.coding --key-file ~/.brainsrv/beacon.key`.
3. Run a Claude Code session in repo X and a Codex session in repo X. Both show
   up as brainsrv sessions under `org.me.coding.x.claude_code` and
   `org.me.coding.x.codex`, with ordered turns and tool/file attrs.
4. Evaluate a trace, then approve a candidate with
   `beacon memory approve …`. The memory appears in brainsrv
   `recall` under `org.me.coding.x`.
5. In a **Cursor** session in repo X, the agent calls `search_memory` and gets
   the lesson learned from the Claude session.
6. Kill brainsrv and approve another candidate. The approval succeeds and
   `beacon memory brainsrv status` shows 1 pending. Restart brainsrv, run
   `sync`, and it shows 0 pending.
7. Re-run `connect --backfill`. There are no duplicate turns.

## 7. Open questions for the implementing session

- **Q1.** Do API keys support a default scope (for A2's missing-`X-Scope`
  case)? If not, require `X-Scope` and have B3 always send it.
- **Q2.** Does reconcile close valid-time on the old attribute for
  supersede (see B1)?
- **Q3.** Are text-valued attributes embedded and BM25-indexed, or only
  `text`? The B1 mapping assumes `text` carries the searchable content.
  Confirm.
- **Q4.** Do we retire `adapters/claude-code-hook` once A2 ships? Proposal:
  keep it for Beacon-less setups and document A2 as the preferred path.
- **Q5.** Pin which `asymptoteobserve` version? Proposal: pin a tag, and add a
  contract test that parses upstream's `sample-event.jsonl` so schema drift
  fails loudly.

## 8. Follow-ups (not v1)

- Evidence Gateway: attest `approval.*` and `mcp.tool_invoked` events from
  Beacon as evidence records. Endpoint agents' actions then become verifiable
  alongside mcpx-routed ones.
- Beacon dashboard "Memory" tab reading from brainsrv (currently SQLite via
  write-through, which is fine).
- Hebrew BM25 (P14) once Beacon traffic shows real Hebrew prompts.
