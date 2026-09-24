# afferent

afferent is a fork of [Beacon](https://github.com/Asymptote-Labs/agent-beacon)
(`Asymptote-Labs/agent-beacon`). It feeds agent activity and reviewed
memories from Beacon into **brainsrv**. Everything else behaves as upstream
Beacon does.

## Codename only

"afferent" is the repository name and nothing more. The Go module path
(`github.com/asymptote-labs/agent-beacon/...`), the package names and the
`beacon` command all stay the same as upstream. This keeps weekly rebases
small. Fork code lives in new files, and upstream files get only the edits
listed in [REBASE.md](REBASE.md#allowed-upstream-file-edits-review-gate).

## License and attribution

Beacon is © Asymptote Labs and released under the MIT License. See the
upstream [LICENSE](https://github.com/Asymptote-Labs/agent-beacon/blob/main/LICENSE),
also kept unchanged at [`/LICENSE`](../../LICENSE) in this repository.
afferent's changes are distributed under the same license, and the upstream
copyright notice must stay in place.

## Branches

| Branch | Role |
|---|---|
| `main` | Mirror of upstream `main`. No afferent commits. |
| `brainsrv` | Working branch: afferent commits on top of upstream. Must be the repo default branch so the weekly job can run (see REBASE.md). |
| `brainsrv-next` | Rebase candidate that the weekly CI job pushes (`.github/workflows/afferent-upstream.yml`). Review it, then promote it. |

## Documents

- [SPEC.md](SPEC.md): what afferent and brainsrv must do.
- [PLAN.md](PLAN.md): how, phase by phase, including corrections to the SPEC.
- [REBASE.md](REBASE.md): the weekly rebase procedure, the CI job, and the allowed upstream-file edits.
