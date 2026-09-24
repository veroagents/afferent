# REBASE — keeping afferent on top of upstream

afferent is a fork of [`Asymptote-Labs/agent-beacon`](https://github.com/Asymptote-Labs/agent-beacon).
It must rebase cleanly every week. New work goes in **new files**, and upstream
files get only the edits listed in the table below.

## Branches and remotes

| Name | What it is |
|---|---|
| `origin` | `git@github.com:veroagents/afferent.git` |
| `upstream` | `git@github.com:Asymptote-Labs/agent-beacon.git` |
| `main` | Exact mirror of `upstream/main`. Never commit to it. |
| `brainsrv` | Working branch: afferent commits on top of upstream. |
| `brainsrv-next` | Candidate written by CI: `brainsrv` rebased onto the latest upstream. Force-pushed by CI, review only. |

## Weekly manual procedure

```sh
cd ~/projects/afferent
git fetch upstream
git fetch origin

# 1. Mirror upstream on main (fast-forward only).
git checkout main
git merge --ff-only upstream/main
git push origin main

# 2. Rebase the working branch.
git checkout brainsrv
git rebase upstream/main
#    On conflict: fix the file, `git add <file>`, `git rebase --continue`.
#    A conflict must be in a file from the allowed-edits table below. If it
#    is anywhere else, one of our commits touched a file it should not have.
#    Move that change into a new file.

# 3. Build and test (same as CI).
cd cli/beacon
make build-hooks-current
go test ./...
cd ../../pkg/asymptoteobserve
go test ./...
cd ../..

# 4. Publish the rebased branch (history rewritten, so force with lease).
git push --force-with-lease origin brainsrv
```

## What the CI job does

`.github/workflows/afferent-upstream.yml` runs every Monday at 06:00 UTC and
on manual dispatch. It only runs in `veroagents/afferent`.

1. Checks out `brainsrv` with full history and fetches upstream over https.
2. Rebases `brainsrv` onto `upstream/main` in a scratch branch, as the
   `github-actions[bot]` identity.
3. **Conflict:** records `git diff --name-only --diff-filter=U`, aborts the
   rebase, opens or updates an open issue whose title starts with
   `upstream drift` (retitled `upstream drift: <date>`). The issue lists the
   conflicting files, the upstream SHA and the run URL. The job then fails.
4. **Clean rebase:** sets up Go via `.github/actions/setup-go`, runs
   `make build-hooks-current`, then `go test ./...` in `cli/beacon` and
   `pkg/asymptoteobserve`. A failure opens or updates the same issue and
   fails the job.
5. **Green:** pushes `upstream/main` to `origin/main` without force, so a
   non-fast-forward is rejected and fails the job. It then force-pushes the
   rebased result to `brainsrv-next`. It **never pushes `brainsrv`**.

Token note: `GITHUB_TOKEN` cannot push commits that change
`.github/workflows/**`. If upstream changes a workflow, or `brainsrv-next`
gets rejected for that reason, add a repo secret `AFFERENT_PUSH_TOKEN`: a
fine-grained PAT with Contents and Workflows write on this repo. The job uses
it for checkout and pushes when present. Issues always use `GITHUB_TOKEN`.

## Promoting `brainsrv-next` to `brainsrv`

Only after reviewing the candidate:

```sh
git fetch origin
git log --oneline upstream/main..origin/brainsrv-next   # only afferent commits
git range-diff upstream/main origin/brainsrv origin/brainsrv-next   # same patches as brainsrv?
git diff --stat upstream/main origin/brainsrv-next      # only new files + allowed edits

git checkout brainsrv
git reset --hard origin/brainsrv-next
git push --force-with-lease origin brainsrv
```

If `brainsrv` got new commits after the candidate was cut, rebase
`brainsrv` yourself with the manual procedure instead of resetting.

## Allowed upstream-file edits (review gate)

Copied from `PLAN.md` §4. Paths are relative to `cli/beacon/`.
**This is every upstream-file edit allowed across all phases. Anything else
is a review failure:**

| File | Edit |
|---|---|
| `internal/learning/store.go` | one field `hooks StoreHooks`; one call-through line each in `PutMemory`, `ListMemories`, `GetMemory` |
| `cmd/memory.go` | `memoryStore()` → `learning.OpenConfigured(logPath)` |
| `internal/mcpserver/server.go` | `memoryStore()` → `OpenConfigured`; `registerBrainsrvTools()` call in `registerTools`; `degraded`/`history` fields on 2 result structs; `include_history` in 2 input schemas |
| `internal/endpoint/dashboard/memory.go` | `learning.Open` → `learning.OpenConfigured` |
| `internal/endpoint/service/forwarder.go` | optional `Label`/`SystemdUnit`/`Description` on `ForwarderManager` |

A quick check a reviewer can run. Every path it prints must be a new file
(`A`) or appear in the table above:

```sh
git diff --name-status upstream/main...brainsrv | grep -v '^A'
```

## Upstream drift log

- PLAN.md was verified against upstream `c03b02f`. Upstream `main` has since
  moved to `5303224c` (10 commits). None of those commits touch the planned
  touch points: the five files above, `cli/beacon/cmd/endpoint.go` or
  `cli/beacon/go.mod` (`git diff --stat c03b02f 5303224c -- <paths>` is
  empty).
