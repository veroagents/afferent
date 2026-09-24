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

Every failure also writes the same report to the run's job summary and an
`::error::` annotation, so it is visible even if the issue call fails.

Token note: `GITHUB_TOKEN` cannot push commits that change
`.github/workflows/**`. Every rebased `brainsrv-next` contains a fresh commit
that adds `afferent-upstream.yml`, and `main` picks up any upstream workflow
change, so expect to need a repo secret `AFFERENT_PUSH_TOKEN`: a fine-grained
PAT with Contents and Workflows write on this repo. The job uses it for
checkout and pushes when present. Issues always use `GITHUB_TOKEN`.

### Repo prerequisites (GitHub settings, done by hand)

The job does nothing until these are set on `veroagents/afferent`:

1. **Actions enabled.** GitHub disables workflows on a new fork until they are
   enabled in the Actions tab.
2. **Default branch = `brainsrv`.** `schedule` only fires, and the Actions tab
   only offers "Run workflow", for workflow files on the default branch.
   `main` must stay a pure upstream mirror, so this file never lands there.
3. **Issues enabled.** Forks start with issues off, and `gh issue create`
   then fails. The job summary still carries the report.
4. **`AFFERENT_PUSH_TOKEN`** secret, as above.

Enabling Actions also enables upstream's own workflows. See "Upstream
workflows on the fork" below.

### Upstream workflows on the fork

These upstream files are never edited here. What would run once Actions is on:

| Workflow | Trigger on the fork | Risk |
|---|---|---|
| `ci.yml` | `pull_request`, `push` to `main` | Tests only, no secrets. Runs on the `main` fast-forward (when pushed with the PAT or by hand) and on PRs. Harmless, costs runner minutes (macOS + Windows jobs). |
| `release-check.yml` | `pull_request` on release paths, `workflow_dispatch` | Build checks, no secrets. Harmless. |
| `release.yml` | `push` of a `v*` tag | GoReleaser publishes a GitHub release on the fork, then needs `HOMEBREW_TAP_TOKEN` and Apple signing secrets we do not have. Fails part-way, possibly after a release is created on the fork. |
| `release-extension.yml` | `push` of an `ext-v*` tag, `workflow_dispatch` | Creates a GitHub release on the fork; AMO signing secrets missing. |
| `npm-publish-sdk.yml` | `push` of an `sdk-js-v*` tag, `workflow_dispatch` | npm trusted publishing (OIDC). Fails because the fork is not a trusted publisher, but should never be tried. |
| `windows-sandbox.yml` | `workflow_dispatch` only | Needs `ANTHROPIC_API_KEY` and the `windows-sandbox` environment. Only runs if dispatched by hand. |

Rules that keep the release workflows inert: never `git push --tags` or
`--follow-tags` to `origin` (`git fetch upstream` brings upstream's `v*`,
`ext-v*` and `sdk-js-v*` tags into the local clone), and never dispatch the
release or publish workflows. Optionally disable them in the Actions tab
(`gh workflow disable`); that is a repo setting, not a file edit.

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
