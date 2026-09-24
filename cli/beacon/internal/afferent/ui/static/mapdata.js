// afferent ui: turns a /v1/overview answer into the treemap's data tree. Pure
// (no DOM, no d3) so `go test` can run it under node.
//
// brainsrv cuts the overview tree two ways and marks the parent `truncated`:
// at 200 children per node and at a global node budget. The turns of the cut
// children still count in the parent's `turns`, so without help they show as
// an unlabelled blank area. buildMapTree gives each truncated node that still
// has listed children a synthetic "not listed" child holding the leftover
// turns, and reports whether anything was cut at all (the root included).
(function (global) {
  'use strict';

  const NOT_LISTED = 'not listed';

  function sumTurns(kids) {
    return kids.reduce((a, c) => a + (c.turns || 0), 0);
  }

  function copyNode(n, acc) {
    const kids = (n.children || []).map((c) => copyNode(c, acc));
    const out = Object.assign({}, n, { children: kids });
    if (n.truncated) {
      acc.count++;
      const left = (n.turns || 0) - sumTurns(kids);
      if (kids.length && left > 0) {
        kids.push({ scope: n.scope, label: NOT_LISTED, turns: left, synthetic: true, children: [] });
      }
    }
    return out;
  }

  // buildMapTree(ov) -> { root, truncated, truncatedNodes }.
  function buildMapTree(ov) {
    const acc = { count: 0 };
    const root = copyNode({
      scope: ov.scope,
      label: ov.scope,
      turns: (ov.totals && ov.totals.turns) || 0,
      truncated: !!ov.truncated,
      children: ov.children || [],
    }, acc);
    return { root: root, truncated: acc.count > 0, truncatedNodes: acc.count };
  }

  const api = { buildMapTree: buildMapTree, NOT_LISTED: NOT_LISTED };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else global.AfferentMap = api;
})(typeof window !== 'undefined' ? window : this);
