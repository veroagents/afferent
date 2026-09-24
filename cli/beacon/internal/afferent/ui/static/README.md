# afferent ui static files

Served by `afferent ui` from `go:embed`. There is no build step and the page
loads nothing from the network.

| File | What |
|---|---|
| `index.html` | the page shell (no inline script or style: the CSP forbids both) |
| `app.js` | the page logic: status strip, brain map (treemap), knowledge graph, search, side panel |
| `app.css` | styles, dark by default, light under `prefers-color-scheme: light` |
| `d3.v7.min.js` | vendored d3, see below |
| `LICENSE-d3` | d3's ISC license |

## Vendored d3

- Version: d3 7.9.0 (`dist/d3.min.js`)
- Source: https://cdn.jsdelivr.net/npm/d3@7/dist/d3.min.js
  (identical to https://unpkg.com/d3@7.9.0/dist/d3.min.js)
- sha256: `f2094bbf6141b359722c4fe454eb6c4b0f0e42cc10cc7af921fc158fceb86539`
- License: ISC, Copyright 2010-2023 Mike Bostock (`LICENSE-d3`)

To update, download the new `dist/d3.min.js`, check it against a second CDN,
record the new version and sha256 here, and run the ui tests and the
screenshot check in `docs/afferent/CLI.md`.

```sh
shasum -a 256 d3.v7.min.js
```
