package ui

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A brainsrv that cut the overview tree (200 children per node, or its
// global node budget) marks the parent truncated. The proxy must pass the
// root's flag through, and the page must load mapdata.js before app.js.
func TestOverviewTruncatedPassesThrough(t *testing.T) {
	f := newFixture(t, nil)
	f.brain.MaxChildren = 2
	r := f.get("/api/overview")
	if r.status != 200 {
		t.Fatalf("overview: %d %s", r.status, r.body)
	}
	var ov struct {
		Truncated bool           `json:"truncated"`
		Totals    map[string]int `json:"totals"`
		Kids      []struct {
			Turns     int  `json:"turns"`
			Truncated bool `json:"truncated"`
		} `json:"children"`
	}
	if err := json.Unmarshal([]byte(r.body), &ov); err != nil {
		t.Fatal(err)
	}
	sum := 0
	for _, k := range ov.Kids {
		sum += k.Turns
	}
	if !ov.Truncated || len(ov.Kids) != 2 || sum >= ov.Totals["turns"] {
		t.Fatalf("truncated overview: truncated=%v kids=%d sum=%d total=%d", ov.Truncated, len(ov.Kids), sum, ov.Totals["turns"])
	}
	index := f.get("/").body
	m, a := strings.Index(index, `src="mapdata.js"`), strings.Index(index, `src="app.js"`)
	if m < 0 || a < 0 || m > a {
		t.Fatalf("index.html must load mapdata.js before app.js")
	}
	if js := f.get("/app.js").body; !strings.Contains(js, "AfferentMap.buildMapTree(ov)") {
		t.Fatal("app.js does not build the map from mapdata.js")
	}
}

// buildMapTree, run under node: the root's truncated flag is reported and the
// cut children's turns become a labelled "not listed" tile instead of a blank
// area, at the root and at nested nodes.
func TestMapDataTruncation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src, err := filepath.Abs(filepath.Join("static", "mapdata.js"))
	if err != nil {
		t.Fatal(err)
	}
	script := `
const m = require(process.argv[1]);
const cases = {
  cut: m.buildMapTree({scope: 'ws.m', truncated: true, totals: {turns: 100}, children: [
    {scope: 'ws.m.a', label: 'a', turns: 50, truncated: true, children: [{scope: 'ws.m.a.x', label: 'x', turns: 30, children: []}]},
    {scope: 'ws.m.b', label: 'b', turns: 20, children: []},
  ]}),
  whole: m.buildMapTree({scope: 'ws.m', totals: {turns: 70}, children: [
    {scope: 'ws.m.a', label: 'a', turns: 50, children: []},
    {scope: 'ws.m.b', label: 'b', turns: 20, children: []},
  ]}),
  budgetLeaf: m.buildMapTree({scope: 'ws.m', totals: {turns: 5}, children: [
    {scope: 'ws.m.a', label: 'a', turns: 5, truncated: true, children: []},
  ]}),
};
process.stdout.write(JSON.stringify(cases));
`
	out, err := exec.Command(node, "-e", script, src).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	type n struct {
		Scope     string `json:"scope"`
		Label     string `json:"label"`
		Turns     int    `json:"turns"`
		Synthetic bool   `json:"synthetic"`
		Truncated bool   `json:"truncated"`
		Children  []n    `json:"children"`
	}
	var got map[string]struct {
		Root           n    `json:"root"`
		Truncated      bool `json:"truncated"`
		TruncatedNodes int  `json:"truncatedNodes"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}

	cut := got["cut"]
	if !cut.Truncated || cut.TruncatedNodes != 2 {
		t.Fatalf("cut: truncated=%v nodes=%d", cut.Truncated, cut.TruncatedNodes)
	}
	kids := cut.Root.Children
	if len(kids) != 3 || !kids[2].Synthetic || kids[2].Label != "not listed" || kids[2].Turns != 30 || kids[2].Scope != "ws.m" {
		t.Fatalf("root not-listed tile: %+v", kids)
	}
	a := kids[0].Children
	if len(a) != 2 || !a[1].Synthetic || a[1].Turns != 20 || a[1].Scope != "ws.m.a" {
		t.Fatalf("nested not-listed tile: %+v", a)
	}

	whole := got["whole"]
	if whole.Truncated || len(whole.Root.Children) != 2 {
		t.Fatalf("whole tree changed: %+v", whole)
	}
	for _, k := range whole.Root.Children {
		if k.Synthetic {
			t.Fatalf("synthetic tile in an uncut tree: %+v", k)
		}
	}

	// A node cut by the node budget with nothing listed stays a leaf (its
	// tooltip says children are not listed); no lone "not listed" child.
	leaf := got["budgetLeaf"]
	if !leaf.Truncated || len(leaf.Root.Children) != 1 || len(leaf.Root.Children[0].Children) != 0 {
		t.Fatalf("budget leaf: %+v", leaf)
	}
}
