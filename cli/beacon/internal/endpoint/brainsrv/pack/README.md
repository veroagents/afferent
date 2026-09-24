# afferent brainsrv Forwarding Pack

This pack forwards Beacon's local runtime JSONL (`runtime.jsonl`) to brainsrv's
Beacon ingest (`POST /v1/ingest/beacon/runtime`), where each session becomes
turns under `<scope>.<repo_label>.<harness_label>`. Vector does the network;
Beacon stays the local JSONL producer.

## What leaves the machine

Every line of `runtime.jsonl` written after the forwarder first starts (or the
whole existing log with `--backfill`), exactly as Beacon wrote it locally.
Local redaction, retention and size limits apply before a line is written, so
they apply to what is forwarded. The inventory stream is not forwarded.

## Credentials and scope

- One brainsrv API key **per member per install** (`spk_…`), minted with
  `narrow_scope` = the member's base scope `ws.<ws_id>.people.<member>.harness`
  and `read` + `write` grants there. Never a shared fleet key.
- The key lives in a JSON secrets file (`{"brainsrv_key": "spk_…"}`, mode 0600)
  read through Vector's `file` secret backend, so it is in neither the config
  nor the environment.
- Every request carries `X-Scope: <base scope>`. The startup healthcheck calls
  `GET /v1/ingest/beacon/health?scope=<base scope>`, so a revoked key (401) or
  a scope it cannot read (403) is reported when Vector starts.

## Delivery

gzip, newline-delimited, `Content-Type: application/x-ndjson`, batches of at
most 5 MB / 5,000 lines (brainsrv caps a request at 8 MiB / 10,000 lines), a
512 MiB disk buffer that blocks when full. Vector retries 5xx and never retries
4xx. brainsrv deduplicates on `event.id`, so retries and backfills are safe.

## Install

```bash
beacon endpoint brainsrv connect --url https://brain.example.com \
  --scope ws.<id>.people.<member>.harness --key-file ~/.brainsrv/beacon.key
```

`connect` checks the key against `/v1/ingest/beacon/health` before writing
anything, renders this template with literal values, validates it with Vector,
and runs it as `com.afferent.brainsrv-forwarder` (launchd) or
`afferent-brainsrv-forwarder.service` (systemd). It coexists with the Beacon
Managed forwarder: separate service, data_dir, checkpoints and secrets file.

To run it by hand instead:

```bash
beacon endpoint brainsrv install-pack --output ./afferent-brainsrv-pack
export BEACON_BRAINSRV_URL=https://brain.example.com
export BEACON_BRAINSRV_SCOPE=ws.<id>.people.<member>.harness
export BEACON_BRAINSRV_SECRETS_FILE=$HOME/.brainsrv/vector-secrets.json
vector validate --skip-healthchecks ./afferent-brainsrv-pack/vector.toml
vector --config ./afferent-brainsrv-pack/vector.toml
```

## Backfill

`read_from` only applies to a file with no checkpoint in the data_dir.
`connect --backfill` renders `read_from = "beginning"` and clears this
forwarder's file checkpoints, so the whole existing log is re-read once. A later
`connect` without `--backfill` renders `read_from = "end"` again and keeps the
checkpoints, so it resumes where it left off.
