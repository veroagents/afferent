// afferent ui: the page. No build step, no network except this server's /api.
// Everything from brainsrv is untrusted text: it is only ever put in the page
// with textContent / SVG text, never as HTML.
(function () {
  'use strict';

  // ---- key and API -------------------------------------------------------

  const KEY_STORE = 'afferent-ui-key';
  const KEY = (function readKey() {
    let k = '';
    const m = location.hash.match(/(?:^#|&)k=([A-Za-z0-9_-]+)/);
    if (m) {
      k = m[1];
      try { sessionStorage.setItem(KEY_STORE, k); } catch (e) { /* private mode */ }
      // Drop the key from the address bar (it stays in this tab's session).
      try { history.replaceState(null, '', location.pathname); } catch (e) { /* ignore */ }
    } else {
      try { k = sessionStorage.getItem(KEY_STORE) || ''; } catch (e) { k = ''; }
    }
    return k;
  })();

  async function api(path, opts) {
    opts = opts || {};
    const headers = { 'X-Afferent-UI-Key': KEY, 'Accept': 'application/json' };
    if (opts.body) headers['Content-Type'] = 'application/json';
    let res;
    try {
      res = await fetch('/api/' + path, {
        method: opts.method || 'GET',
        headers: headers,
        body: opts.body ? JSON.stringify(opts.body) : undefined,
        cache: 'no-store',
        credentials: 'omit',
      });
    } catch (e) {
      const err = new Error('afferent ui is not running (the server stopped?). Run `afferent ui` again.');
      err.code = 'ui_down';
      throw err;
    }
    let data = null;
    try { data = await res.json(); } catch (e) { data = null; }
    if (!res.ok) {
      const err = new Error((data && data.error) || ('HTTP ' + res.status));
      err.code = (data && data.code) || 'http_' + res.status;
      err.status = res.status;
      throw err;
    }
    return data;
  }

  // ---- small helpers -----------------------------------------------------

  const $ = (id) => document.getElementById(id);

  function el(tag, attrs) {
    const n = document.createElement(tag);
    if (attrs) {
      for (const k of Object.keys(attrs)) {
        const v = attrs[k];
        if (v == null || v === false) continue;
        if (k === 'class') n.className = v;
        else if (k === 'text') n.textContent = v;
        else if (k.startsWith('on')) n.addEventListener(k.slice(2), v);
        else n.setAttribute(k, v === true ? '' : String(v));
      }
    }
    for (let i = 2; i < arguments.length; i++) {
      const c = arguments[i];
      if (c == null || c === false) continue;
      if (Array.isArray(c)) c.forEach((x) => x != null && n.append(x));
      else n.append(c);
    }
    return n;
  }

  const fmtInt = (n) => (n == null ? '–' : Number(n).toLocaleString());

  function fmtBytes(n) {
    if (n == null) return '–';
    const u = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0;
    let v = Number(n);
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return (i === 0 ? v : v.toFixed(v < 10 ? 1 : 0)) + ' ' + u[i];
  }

  function parseTime(s) {
    if (!s || s.startsWith('0001-')) return null;
    const d = new Date(s);
    return isNaN(d) ? null : d;
  }

  function ago(s) {
    const d = typeof s === 'string' ? parseTime(s) : s;
    if (!d) return 'never';
    const sec = Math.round((Date.now() - d.getTime()) / 1000);
    if (sec < 0) return 'just now';
    if (sec < 45) return sec + 's ago';
    if (sec < 90 * 60) return Math.round(sec / 60) + 'm ago';
    if (sec < 36 * 3600) return Math.round(sec / 3600) + 'h ago';
    if (sec < 60 * 86400) return Math.round(sec / 86400) + 'd ago';
    return d.toLocaleDateString();
  }

  function dur(sec) {
    if (sec == null) return '–';
    if (sec <= 0) return 'expired';
    if (sec < 90) return sec + 's';
    if (sec < 5400) return Math.round(sec / 60) + 'm';
    return Math.round(sec / 3600) + 'h';
  }

  const lastLabel = (scope) => (scope || '').split('.').pop();

  function relScope(scope, base) {
    if (!scope) return '';
    if (base && scope === base) return '(this scope)';
    if (base && scope.startsWith(base + '.')) return scope.slice(base.length + 1);
    return scope;
  }

  function clip(s, n) {
    s = String(s == null ? '' : s);
    return s.length > n ? s.slice(0, n - 1) + '…' : s;
  }

  function cssVar(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }

  function debounce(f, ms) {
    let t;
    return function () { clearTimeout(t); t = setTimeout(f, ms); };
  }

  // ---- state -------------------------------------------------------------

  const state = {
    status: null,
    overview: null,
    overviewErr: null,
    graph: null,
    graphErr: null,
    selectedScope: null,
    selectedNode: null,
    hits: null, // {entities:Set, scopes:[]}
    side: 'home',
  };

  // ---- banner and status strip -------------------------------------------

  function explain(err) {
    switch (err && err.code) {
      case 'bad_key': return 'This page has no valid key. Open the full link that `afferent ui` printed (it ends in #k=…).';
      case 'signed_out': return 'You are signed out. Run `afferent login` in a terminal, then press Refresh.';
      case 'brainsrv_down': return err.message + '. Is brainsrv running?';
      case 'unsupported': return 'This brainsrv does not serve the overview/graph API yet (' + err.message + ').';
      case 'ui_down': return err.message;
      default: return err ? err.message : '';
    }
  }

  function showBanner(msg, bad) {
    const b = $('banner');
    if (!msg) { b.hidden = true; b.textContent = ''; return; }
    b.hidden = false;
    b.className = 'banner' + (bad ? ' bad' : '');
    b.textContent = msg;
  }

  function updateBanner() {
    const errs = [];
    const s = state.status;
    if (s && !s.signed_in) errs.push({ code: 'signed_out' });
    for (const e of [state.overviewErr, state.graphErr]) {
      if (e && !errs.some((x) => x.code === e.code)) errs.push(e);
    }
    if (!errs.length) return showBanner('');
    const bad = errs.some((e) => e.code === 'bad_key' || e.code === 'ui_down' || e.code === 'brainsrv_down');
    showBanner(errs.map(explain).filter(Boolean).join(' '), bad);
  }

  function chip(dotClass, label, value, title) {
    return el('span', { class: 'chip', title: title || null },
      dotClass != null ? el('span', { class: 'dot ' + dotClass }) : null,
      label ? label + ' ' : null,
      el('b', { text: value }));
  }

  function renderStrip() {
    const s = state.status;
    const strip = $('strip');
    strip.replaceChildren();
    if (!s) { strip.append(chip('', '', 'loading status…')); return; }

    $('who').textContent = s.signed_in
      ? (s.identity || 'signed in') + (s.scope ? ' · ' + s.scope : '')
      : 'signed out' + (s.scope ? ' · ' + s.scope : '');

    const f = s.forwarder;
    if (f) {
      let cls = 'ok';
      let v = f.state || 'unknown';
      if (f.state === 'paused' || f.state === 'backoff') cls = 'warn';
      if (f.state === 'stopped') cls = 'bad';
      if (f.paused_reason) v += ': ' + f.paused_reason;
      const upd = parseTime(f.updated_at);
      if (upd && Date.now() - upd.getTime() > 5 * 60 * 1000 && f.state === 'running') {
        cls = 'warn';
        v += ' (status ' + ago(upd) + ')';
      }
      strip.append(chip(cls, 'Forwarder', v, f.last_error ? 'Last error: ' + f.last_error : null));
      strip.append(chip(null, 'Last sent', ago(f.last_success_at)));
      strip.append(chip(null, 'Lag', fmtBytes(f.lag_bytes)));
      strip.append(chip(null, 'Sent', fmtInt(f.lines_sent) + ' lines',
        'accepted ' + fmtInt(f.accepted) + ', duplicate ' + fmtInt(f.duplicate) + ', rejected ' +
        fmtInt(f.rejected) + ', skipped ' + fmtInt(f.lines_skipped) + ' in ' + fmtInt(f.batches_sent) + ' batches'));
      strip.append(chip(null, 'Accepted', fmtInt(f.accepted)));
    } else {
      strip.append(chip('warn', 'Forwarder', s.forwarder_error || 'unknown'));
    }
    const ov = state.overview;
    if (ov && ov.totals) {
      const p = ov.totals.pending_extraction || 0;
      strip.append(chip(p > 0 ? 'warn' : 'ok', 'Pending extraction', fmtInt(p) + ' turns'));
    }
    const svc = s.service || {};
    let sv = 'not installed';
    let sc = 'warn';
    if (!svc.available) { sv = 'unavailable'; }
    else if (svc.running) { sv = 'running' + (svc.pid ? ' (pid ' + svc.pid + ')' : ''); sc = 'ok'; }
    else if (svc.loaded) { sv = 'loaded, not running'; }
    else if (svc.installed) { sv = 'installed, not loaded'; }
    strip.append(chip(sc, 'Service', sv, svc.error || svc.detail || null));
    if (s.signed_in) {
      strip.append(chip(s.token_valid_seconds > 120 ? 'ok' : 'warn', 'Token', dur(s.token_valid_seconds) + ' left',
        'Refreshed automatically. Issuer ' + (s.issuer || '')));
    } else {
      strip.append(chip('bad', 'Signed in', 'no'));
    }
    strip.append(chip(null, 'brainsrv', s.brainsrv_url + ' · ' + s.context, s.scope_error || null));
  }

  async function loadStatus() {
    try {
      state.status = await api('status');
    } catch (e) {
      if (e.code === 'bad_key' || e.code === 'ui_down') {
        state.overviewErr = e;
      }
    }
    renderStrip();
    updateBanner();
  }

  // ---- tooltip -----------------------------------------------------------

  const tip = {
    show(ev, title, lines) {
      const t = $('tooltip');
      t.replaceChildren(el('div', { class: 'tt-title', text: title }),
        ...lines.filter(Boolean).map((l) => el('div', { class: 'tt-sub', text: l })));
      t.hidden = false;
      tip.move(ev);
    },
    move(ev) {
      const t = $('tooltip');
      if (t.hidden) return;
      const pad = 14;
      let x = ev.clientX + pad;
      let y = ev.clientY + pad;
      const r = t.getBoundingClientRect();
      if (x + r.width > window.innerWidth - 8) x = ev.clientX - r.width - pad;
      if (y + r.height > window.innerHeight - 8) y = ev.clientY - r.height - pad;
      t.style.left = Math.max(4, x) + 'px';
      t.style.top = Math.max(4, y) + 'px';
    },
    hide() { $('tooltip').hidden = true; },
  };

  function emptyState(container, title, text) {
    container.replaceChildren(el('div', { class: 'empty' }, el('strong', { text: title }), text ? el('span', { text: text }) : null));
  }

  // ---- brain map (treemap) -----------------------------------------------

  function recencyColor(last) {
    const d = parseTime(last);
    const recent = d3.color(cssVar('--recent')) || d3.color('#2ec4b6');
    const old = d3.color(cssVar('--old')) || d3.color('#2b3544');
    if (!d) return old.formatHex();
    const hours = Math.max(0, (Date.now() - d.getTime()) / 3.6e6);
    // 0 → now, 1 → 30 days or older, on a log scale so the last day spreads out.
    const t = Math.min(1, Math.log1p(hours) / Math.log1p(24 * 30));
    return d3.interpolateLab(recent, old)(t);
  }

  function textOn(fill) {
    const c = d3.lab(fill);
    return c.l > 62 ? '#111820' : '#f2f6fa';
  }

  function tileLines(d) {
    if (d.data.synthetic) {
      return [
        d.data.scope,
        fmtInt(d.data.turns) + ' turns in scopes brainsrv did not list',
        'brainsrv caps the map (200 children per scope, and a total node budget)',
      ];
    }
    return [
      d.data.scope,
      fmtInt(d.data.turns) + ' turns · ' + fmtInt(d.data.sessions) + ' sessions',
      fmtInt(d.data.entities) + ' entities · ' + fmtInt(d.data.facts) + ' facts · ' + fmtInt(d.data.relations) + ' relations',
      'last activity ' + ago(d.data.last_activity),
      d.data.truncated ? 'some child scopes not listed (brainsrv limit)' : null,
    ];
  }

  const MAP_HINT = 'tile size = turns · color = last activity';

  function renderMap() {
    const box = $('map');
    tip.hide();
    const ov = state.overview;
    if (state.overviewErr && !ov) {
      emptyState(box, state.overviewErr.code === 'signed_out' ? 'Signed out' : 'The map is unavailable', explain(state.overviewErr));
      return;
    }
    if (!ov) { emptyState(box, 'Loading…'); return; }
    const children = ov.children || [];
    const total = (ov.totals && ov.totals.turns) || 0;
    if (!children.length || total === 0) {
      emptyState(box, 'Your brain is empty so far',
        'Once the forwarder sends agent activity for ' + ov.scope + ', each repo and harness appears here as a tile.');
      return;
    }
    const W = Math.max(200, box.clientWidth);
    const H = Math.max(200, box.clientHeight);
    const tree = window.AfferentMap.buildMapTree(ov);
    const rootData = tree.root;
    rootData.label = lastLabel(ov.scope);
    $('map-hint').textContent = MAP_HINT + (tree.truncated ? ' · some scopes not listed (brainsrv limit)' : '');
    const root = d3.hierarchy(rootData, (d) => d.children && d.children.length ? d.children : null)
      .sum((d) => {
        // A node's turns include its children's; count only what is left over.
        const kids = (d.children || []).reduce((a, c) => a + (c.turns || 0), 0);
        return Math.max(0, (d.turns || 0) - (d.children && d.children.length ? kids : 0));
      })
      .sort((a, b) => b.value - a.value);
    d3.treemap()
      .size([W, H])
      .paddingOuter(3)
      .paddingInner(2)
      .paddingTop((d) => (d.depth >= 1 && d.children ? 20 : 3))
      .round(true)(root);

    const svg = d3.create('svg').attr('viewBox', [0, 0, W, H]).attr('width', W).attr('height', H).attr('role', 'img')
      .attr('aria-label', 'Treemap of scopes by turns');
    const nodes = root.descendants().filter((d) => d.depth >= 1 && d.x1 - d.x0 > 1 && d.y1 - d.y0 > 1);
    const g = svg.selectAll('g').data(nodes).join('g')
      .attr('class', (d) => 'tile ' + (d.children ? 'group' : 'leaf') + (d.data.synthetic ? ' not-listed' : ''))
      .attr('transform', (d) => `translate(${d.x0},${d.y0})`);

    g.append('rect')
      .attr('width', (d) => d.x1 - d.x0)
      .attr('height', (d) => d.y1 - d.y0)
      .attr('rx', 4)
      .attr('fill', (d) => (d.children ? null : recencyColor(d.data.last_activity)));

    g.each(function (d) {
      const w = d.x1 - d.x0;
      const h = d.y1 - d.y0;
      const sel = d3.select(this);
      const fill = d.children ? null : recencyColor(d.data.last_activity);
      const color = fill ? textOn(fill) : null;
      const maxChars = Math.floor((w - 10) / 7);
      if (maxChars < 3 || h < 14) return;
      const name = clip(d.data.label || lastLabel(d.data.scope), maxChars);
      if (d.children) {
        sel.append('text').attr('class', 't-name').attr('x', 6).attr('y', 14).text(name);
        const meta = fmtInt(d.data.turns) + ' turns';
        if (name.length + meta.length + 3 < maxChars) {
          sel.append('text').attr('class', 't-meta').attr('x', w - 6).attr('y', 14).attr('text-anchor', 'end').text(meta);
        }
        return;
      }
      sel.append('text').attr('class', 't-name').attr('x', 6).attr('y', 16).attr('fill', color).text(name);
      if (h > 34) {
        sel.append('text').attr('class', 't-meta').attr('x', 6).attr('y', 31).attr('fill', color)
          .text(clip(fmtInt(d.data.turns) + ' turns', maxChars));
      }
      if (h > 50 && !d.data.synthetic) {
        sel.append('text').attr('class', 't-meta').attr('x', 6).attr('y', 45).attr('fill', color)
          .text(clip(ago(d.data.last_activity), maxChars));
      }
    });

    g.on('mouseenter', (ev, d) => tip.show(ev, d.data.label || d.data.scope, tileLines(d)))
      .on('mousemove', (ev) => tip.move(ev))
      .on('mouseleave', () => tip.hide())
      .on('click', (ev, d) => { ev.stopPropagation(); if (!d.data.synthetic) selectScope(d.data); });

    box.replaceChildren(svg.node());
    applyMapMarks();
  }

  function applyMapMarks() {
    const svg = d3.select('#map svg');
    if (svg.empty()) return;
    const hitScopes = (state.hits && state.hits.scopes) || [];
    const isHit = (scope) => hitScopes.some((h) => h === scope || h.startsWith(scope + '.') || scope.startsWith(h + '.'));
    svg.classed('map-dim', hitScopes.length > 0);
    svg.selectAll('.tile')
      .classed('selected', (d) => !d.data.synthetic && state.selectedScope === d.data.scope)
      .classed('hit', (d) => !d.children && !d.data.synthetic && hitScopes.length > 0 && isHit(d.data.scope));
  }

  async function loadOverview() {
    try {
      state.overview = await api('overview?depth=3');
      state.overviewErr = null;
    } catch (e) {
      state.overviewErr = e;
      state.overview = null;
    }
    renderMap();
    renderStrip();
    updateBanner();
    if (state.side === 'home') renderHome();
  }

  // ---- knowledge graph ---------------------------------------------------

  const PALETTE = ['#4e9af1', '#f5a524', '#2ec4b6', '#e5484d', '#a78bfa', '#7cc36b', '#f472b6', '#e0c341', '#60c3e0'];
  let typeColor = () => '#8b98a8';
  let sim = null;
  let zoomState = null;

  function nodeRadius(n) { return Math.min(20, 4 + Math.sqrt(n.degree || 0) * 2.6); }

  function renderGraph() {
    const box = $('graph');
    const legend = $('legend');
    tip.hide();
    if (sim) { sim.stop(); sim = null; }
    const gr = state.graph;
    if (state.graphErr && !gr) {
      legend.replaceChildren();
      $('graph-hint').textContent = '';
      emptyState(box, state.graphErr.code === 'signed_out' ? 'Signed out' : 'The graph is unavailable', explain(state.graphErr));
      return;
    }
    if (!gr) { emptyState(box, 'Loading…'); return; }
    const nodes = (gr.nodes || []).map((n) => Object.assign({}, n));
    const byId = new Map(nodes.map((n) => [n.id, n]));
    const links = (gr.edges || []).filter((e) => byId.has(e.src) && byId.has(e.dst))
      .map((e) => Object.assign({}, e, { source: e.src, target: e.dst }));
    $('graph-hint').textContent = nodes.length
      ? fmtInt(nodes.length) + ' entities · ' + fmtInt(links.length) + ' relations' + (gr.truncated ? ' (top by degree)' : '')
      : '';
    if (!nodes.length) {
      legend.replaceChildren();
      emptyState(box, 'No entities yet', 'brainsrv extracts entities and relations from your turns. They appear here once extraction has run.');
      return;
    }

    // Colors by type: the most common types get their own color.
    const counts = d3.rollups(nodes, (v) => v.length, (n) => n.type || 'unknown').sort((a, b) => b[1] - a[1]);
    const named = counts.slice(0, PALETTE.length - (counts.length > PALETTE.length ? 1 : 0));
    const cmap = new Map(named.map((c, i) => [c[0], PALETTE[i]]));
    const otherColor = cssVar('--muted') || '#8b98a8';
    typeColor = (t) => cmap.get(t || 'unknown') || otherColor;
    const other = counts.slice(named.length).reduce((a, c) => a + c[1], 0);
    const items = named.map((c) => el('span', null, el('span', { class: 'sw', 'data-c': c[0] }), c[0] + ' ' + c[1]));
    if (other) items.push(el('span', null, el('span', { class: 'sw', 'data-c': '' }), 'other ' + other));
    legend.replaceChildren(...items);
    legend.querySelectorAll('.sw').forEach((s) => { s.style.background = s.dataset.c ? typeColor(s.dataset.c) : otherColor; });

    const W = Math.max(200, box.clientWidth);
    const H = Math.max(200, box.clientHeight);
    const svg = d3.create('svg').attr('viewBox', [-W / 2, -H / 2, W, H]).attr('width', W).attr('height', H).attr('role', 'img')
      .attr('aria-label', 'Knowledge graph');
    const world = svg.append('g');
    const maxConf = d3.max(links, (l) => l.confidence) || 1;
    const link = world.append('g').attr('class', 'links').selectAll('line').data(links).join('line')
      .attr('stroke-width', (l) => 0.8 + 1.4 * ((l.confidence || 0) / maxConf))
      .attr('stroke-opacity', (l) => 0.25 + 0.5 * ((l.confidence || 0) / maxConf));
    const node = world.append('g').selectAll('g').data(nodes, (n) => n.id).join('g').attr('class', 'node');
    node.append('circle').attr('r', nodeRadius).attr('fill', (n) => typeColor(n.type));
    const labelled = new Set(nodes.slice().sort((a, b) => (b.degree || 0) - (a.degree || 0)).slice(0, 14).map((n) => n.id));
    node.filter((n) => labelled.has(n.id)).append('text')
      .attr('x', (n) => nodeRadius(n) + 3).attr('y', 4).text((n) => clip(n.name, 24));

    sim = d3.forceSimulation(nodes)
      .force('link', d3.forceLink(links).id((n) => n.id).distance(46).strength(0.5))
      .force('charge', d3.forceManyBody().strength(-90).distanceMax(320))
      .force('collide', d3.forceCollide().radius((n) => nodeRadius(n) + 3))
      // Pull loose nodes in harder, so they do not drift to the edges.
      .force('x', d3.forceX().strength((n) => (n.degree ? 0.06 : 0.3)))
      .force('y', d3.forceY().strength((n) => (n.degree ? 0.08 : 0.3)))
      .stop();
    // Settle the layout up front, so the first paint is already readable.
    for (let i = 0; i < 260; i++) sim.tick();
    const draw = () => {
      link.attr('x1', (l) => l.source.x).attr('y1', (l) => l.source.y)
        .attr('x2', (l) => l.target.x).attr('y2', (l) => l.target.y);
      node.attr('transform', (n) => `translate(${n.x},${n.y})`);
    };
    draw();
    sim.on('tick', draw);

    // Fit the settled layout into the view.
    const xs = d3.extent(nodes, (n) => n.x);
    const ys = d3.extent(nodes, (n) => n.y);
    const gw = xs[1] - xs[0] + 60;
    const gh = ys[1] - ys[0] + 60;
    const k = Math.min(2, Math.max(0.2, Math.min(W / gw, H / gh)));
    const cx = (xs[0] + xs[1]) / 2;
    const cy = (ys[0] + ys[1]) / 2;
    const zoom = d3.zoom().scaleExtent([0.15, 6]).on('zoom', (ev) => { world.attr('transform', ev.transform); zoomState = ev.transform; });
    svg.call(zoom);
    svg.call(zoom.transform, d3.zoomIdentity.scale(k).translate(-cx, -cy));
    svg.on('click', () => { state.selectedNode = null; applyGraphMarks(); });

    node.call(d3.drag()
      .on('start', (ev, n) => { if (!ev.active) sim.alphaTarget(0.25).restart(); n.fx = n.x; n.fy = n.y; tip.hide(); })
      .on('drag', (ev, n) => { n.fx = ev.x; n.fy = ev.y; })
      .on('end', (ev, n) => { if (!ev.active) sim.alphaTarget(0); n.fx = null; n.fy = null; }));

    node.on('mouseenter', (ev, n) => {
      tip.show(ev, n.name, [
        (n.type || 'unknown') + ' · ' + fmtInt(n.degree) + ' relations · ' + fmtInt(n.facts) + ' facts',
        relScope(n.scope, state.overview && state.overview.scope) || n.scope,
        'last seen ' + ago(n.last_seen),
      ]);
      const nb = neighbours(n.id);
      node.classed('nbr', (m) => nb.has(m.id));
    }).on('mousemove', (ev) => tip.move(ev))
      .on('mouseleave', () => { tip.hide(); node.classed('nbr', false); })
      .on('click', (ev, n) => { ev.stopPropagation(); selectNode(n.id); });

    link.on('mouseenter', (ev, l) => tip.show(ev, l.predicate, [l.source.name + ' → ' + l.target.name, 'confidence ' + (l.confidence || 0).toFixed(2)]))
      .on('mousemove', (ev) => tip.move(ev))
      .on('mouseleave', () => tip.hide());

    box.replaceChildren(svg.node());
    applyGraphMarks();
  }

  function neighbours(id) {
    const s = new Set();
    for (const e of (state.graph && state.graph.edges) || []) {
      if (e.src === id) s.add(e.dst);
      if (e.dst === id) s.add(e.src);
    }
    return s;
  }

  function applyGraphMarks() {
    const svg = d3.select('#graph svg');
    if (svg.empty()) return;
    const hits = state.hits && state.hits.entities;
    svg.classed('graph-dim', !!(hits && hits.size));
    svg.selectAll('.node')
      .classed('hit', (n) => !!(hits && hits.has(n.id)))
      .classed('selected', (n) => n.id === state.selectedNode);
  }

  async function loadGraph() {
    try {
      state.graph = await api('graph?limit=150');
      state.graphErr = null;
    } catch (e) {
      state.graphErr = e;
      state.graph = null;
    }
    renderGraph();
    updateBanner();
  }

  // ---- side panel --------------------------------------------------------

  function side(...children) {
    $('side-body').replaceChildren(...children.filter((c) => c != null && c !== false));
    $('side-body').scrollTop = 0;
  }

  function renderHome() {
    state.side = 'home';
    const ov = state.overview;
    const t = (ov && ov.totals) || null;
    side(
      el('h3', { text: 'Your brain' }),
      el('div', { class: 'sub', text: ov ? ov.scope : (state.status && state.status.scope) || '' }),
      t ? el('dl', { class: 'kv' },
        el('dt', { text: 'Sessions' }), el('dd', { text: fmtInt(t.sessions) }),
        el('dt', { text: 'Turns' }), el('dd', { text: fmtInt(t.turns) }),
        el('dt', { text: 'Entities' }), el('dd', { text: fmtInt(t.entities) }),
        el('dt', { text: 'Facts' }), el('dd', { text: fmtInt(t.facts) }),
        el('dt', { text: 'Relations' }), el('dd', { text: fmtInt(t.relations) }),
        el('dt', { text: 'Pending extraction' }), el('dd', { text: fmtInt(t.pending_extraction) + ' turns' }),
        el('dt', { text: 'Updated' }), el('dd', { text: ago(ov.generated_at) })) : null,
      el('h4', { text: 'How to use this page' }),
      el('ul', { class: 'list' },
        el('li', { class: 'item', text: 'Click a tile in the brain map to see that repo or harness’s recent sessions, then a session to read its last turns.' }),
        el('li', { class: 'item', text: 'Click a node in the knowledge graph to see its facts. Drag nodes; scroll to zoom.' }),
        el('li', { class: 'item', text: 'Search runs brainsrv recall over your memories and highlights the matching entities and scopes.' })),
    );
  }

  async function selectScope(d) {
    state.selectedScope = d.scope;
    state.side = 'scope';
    applyMapMarks();
    const list = el('ul', { class: 'list' }, el('li', { class: 'muted', text: 'Loading sessions…' }));
    const turnsBox = el('div');
    side(
      el('div', { class: 'row' }, el('button', { class: 'link', type: 'button', text: '← overview', onclick: () => { state.selectedScope = null; applyMapMarks(); renderHome(); } })),
      el('h3', { text: d.label || lastLabel(d.scope) }),
      el('div', { class: 'sub', text: d.scope }),
      el('dl', { class: 'kv' },
        el('dt', { text: 'Turns' }), el('dd', { text: fmtInt(d.turns) }),
        el('dt', { text: 'Sessions' }), el('dd', { text: fmtInt(d.sessions) }),
        el('dt', { text: 'Entities' }), el('dd', { text: fmtInt(d.entities) + ' (' + fmtInt(d.facts) + ' facts, ' + fmtInt(d.relations) + ' relations)' }),
        el('dt', { text: 'Last activity' }), el('dd', { text: ago(d.last_activity) })),
      el('h4', { text: 'Recent sessions' }),
      list,
      turnsBox,
    );
    let sessions;
    try {
      const res = await api('sessions?limit=30&scope=' + encodeURIComponent(d.scope));
      sessions = Array.isArray(res) ? res : (res && (res.sessions || res.results)) || [];
    } catch (e) {
      if (state.selectedScope !== d.scope) return;
      list.replaceChildren(el('li', { class: 'error', text: explain(e) }));
      return;
    }
    if (state.selectedScope !== d.scope) return;
    if (!sessions.length) {
      list.replaceChildren(el('li', { class: 'muted', text: 'No sessions under this scope.' }));
      return;
    }
    list.replaceChildren(...sessions.map((s) => {
      const li = el('li', { class: 'item click', tabindex: 0 },
        el('div', { class: 'row' },
          el('span', { text: s.scope && s.scope !== d.scope ? relScope(s.scope, d.scope) : 'session ' + String(s.id || '').slice(0, 8) }),
          el('span', { class: 'meta', text: ago(s.last_activity || s.started_at) })),
        el('div', { class: 'meta', text: [s.channel, s.closed_at ? 'closed' : null, s.turns != null ? fmtInt(s.turns) + ' turns' : null, 'started ' + new Date(s.started_at).toLocaleString()].filter(Boolean).join(' · ') }));
      const open = () => {
        list.querySelectorAll('.item').forEach((x) => x.classList.remove('active'));
        li.classList.add('active');
        showTurns(s, turnsBox);
      };
      li.addEventListener('click', open);
      li.addEventListener('keydown', (ev) => { if (ev.key === 'Enter') open(); });
      return li;
    }));
  }

  async function showTurns(s, box) {
    box.replaceChildren(el('h4', { text: 'Last turns' }), el('div', { class: 'muted', text: 'Loading…' }));
    let res;
    try {
      res = await api('sessions/' + encodeURIComponent(s.id) + '/turns');
    } catch (e) {
      box.replaceChildren(el('h4', { text: 'Last turns' }), el('div', { class: 'error', text: explain(e) }));
      return;
    }
    const turns = ((res && res.turns) || []).slice(-20).reverse();
    if (!turns.length) {
      box.replaceChildren(el('h4', { text: 'Last turns' }), el('div', { class: 'muted', text: 'This session has no turns.' }));
      return;
    }
    box.replaceChildren(el('h4', { text: 'Last turns (newest first)' }), el('ul', { class: 'list' }, ...turns.map((t) => {
      const full = String(t.text || '');
      const text = el('div', { class: 'text', text: clip(full, 500) });
      const more = full.length > 500
        ? el('button', { class: 'link more', type: 'button', text: 'show all', onclick: (ev) => { text.textContent = full; ev.target.remove(); } })
        : null;
      return el('li', { class: 'item' },
        el('div', { class: 'row' },
          el('span', null, el('span', { class: 'badge ' + (t.role || ''), text: t.role || 'turn' }), t.kind ? ' ' : null, t.kind ? el('span', { class: 'badge', text: t.kind }) : null),
          el('span', { class: 'meta', text: '#' + t.seq + ' · ' + ago(t.occurred_at || t.created_at) })),
        text, more);
    })));
  }

  function fmtValue(v) {
    if (v == null) return '–';
    if (typeof v === 'string') return v;
    return JSON.stringify(v);
  }

  async function selectNode(id) {
    state.selectedNode = id;
    state.side = 'entity';
    applyGraphMarks();
    const n = state.graph && (state.graph.nodes || []).find((x) => x.id === id);
    side(el('h3', { text: n ? n.name : 'Entity' }), el('div', { class: 'muted', text: 'Loading facts…' }));
    let e;
    try {
      e = await api('entities/' + encodeURIComponent(id));
    } catch (err) {
      if (state.selectedNode !== id) return;
      side(el('h3', { text: n ? n.name : 'Entity' }), el('div', { class: 'error', text: explain(err) }));
      return;
    }
    if (state.selectedNode !== id) return;
    const attrs = Object.entries(e.attributes || {}).sort((a, b) => a[0].localeCompare(b[0]));
    const edges = ((state.graph && state.graph.edges) || []).filter((x) => x.src === id || x.dst === id);
    const nodeName = (nid) => { const m = (state.graph.nodes || []).find((x) => x.id === nid); return m ? m.name : nid; };
    side(
      el('div', { class: 'row' }, el('button', { class: 'link', type: 'button', text: '← overview', onclick: () => { state.selectedNode = null; applyGraphMarks(); renderHome(); } })),
      el('h3', { text: e.name || (n && n.name) || id }),
      el('div', { class: 'sub' }, el('span', { class: 'badge', text: e.type || 'unknown' }), ' ', e.state ? el('span', { class: 'badge', text: e.state }) : null),
      el('dl', { class: 'kv' },
        el('dt', { text: 'Scope' }), el('dd', { class: 'mono', text: e.scope || (n && n.scope) || '–' }),
        n ? el('dt', { text: 'Last seen' }) : null, n ? el('dd', { text: ago(n.last_seen) }) : null),
      el('h4', { text: 'Facts (' + attrs.length + ')' }),
      attrs.length
        ? el('table', { class: 'facts' }, el('tbody', null, ...attrs.map(([k, a]) => el('tr', null,
          el('td', { text: k }),
          el('td', { text: fmtValue(a && a.value) }),
          el('td', { class: 'conf', text: a && a.confidence != null ? Number(a.confidence).toFixed(2) : '', title: a && a.valid_from ? 'valid from ' + a.valid_from : null })))))
        : el('div', { class: 'muted', text: 'No active facts.' }),
      el('h4', { text: 'Relations in view (' + edges.length + ')' }),
      edges.length
        ? el('ul', { class: 'list' }, ...edges.map((x) => {
          const other = x.src === id ? x.dst : x.src;
          const label = x.src === id ? x.predicate + ' → ' : '← ' + x.predicate + ' ';
          return el('li', { class: 'item' }, el('span', { class: 'meta', text: label }),
            el('button', { class: 'link', type: 'button', text: nodeName(other), onclick: () => selectNode(other) }));
        }))
        : el('div', { class: 'muted', text: 'None among the entities shown.' }),
    );
  }

  // ---- search ------------------------------------------------------------

  async function search(q) {
    state.side = 'search';
    side(el('h3', { text: 'Search' }), el('div', { class: 'muted', text: 'Searching for “' + q + '”…' }));
    let res;
    try {
      res = await api('recall', { method: 'POST', body: { query: q, k: 20 } });
    } catch (e) {
      side(el('h3', { text: 'Search' }), el('div', { class: 'error', text: explain(e) }));
      return;
    }
    const results = (res && res.results) || [];
    const nodesById = new Map(((state.graph && state.graph.nodes) || []).map((n) => [n.id, n]));
    const ents = new Set();
    const scopes = [];
    for (const r of results) {
      if (r.entity_id) ents.add(r.entity_id);
      const sc = r.scope || (r.entity_id && nodesById.has(r.entity_id) ? nodesById.get(r.entity_id).scope : null);
      if (sc) scopes.push(sc);
    }
    state.hits = { entities: ents, scopes: scopes };
    $('clear-search').hidden = false;
    applyGraphMarks();
    applyMapMarks();
    const inGraph = [...ents].filter((id) => nodesById.has(id)).length;
    side(
      el('div', { class: 'row' }, el('button', { class: 'link', type: 'button', text: '← overview', onclick: clearSearch })),
      el('h3', { text: '“' + q + '”' }),
      el('div', { class: 'sub', text: results.length + ' result' + (results.length === 1 ? '' : 's') + ' · ' + inGraph + ' highlighted in the graph' }),
      el('h4', { text: 'Results' }),
      results.length ? el('ul', { class: 'list' }, ...results.map((r) => {
        const li = el('li', { class: 'item' + (r.entity_id ? ' click' : '') },
          el('div', { class: 'row' },
            el('span', null, el('span', { class: 'badge', text: r.table || 'memory' }), r.category ? ' ' : null, r.category ? el('span', { class: 'badge', text: r.category }) : null),
            el('span', { class: 'meta', text: (r.confidence != null ? Number(r.confidence).toFixed(2) + ' · ' : '') + ago(r.known_at) })),
          r.entity_name ? el('div', { class: 'meta', text: r.entity_name + (r.entity_type ? ' (' + r.entity_type + ')' : '') }) : null,
          el('div', { class: 'text', text: clip(r.text, 400) }));
        if (r.entity_id) li.addEventListener('click', () => selectNode(r.entity_id));
        return li;
      })) : el('div', { class: 'muted', text: 'Nothing matched.' }),
    );
  }

  function clearSearch() {
    state.hits = null;
    $('clear-search').hidden = true;
    $('q').value = '';
    applyGraphMarks();
    applyMapMarks();
    renderHome();
  }

  // ---- wiring ------------------------------------------------------------

  async function refreshAll() {
    const b = $('refresh');
    b.disabled = true;
    await Promise.all([loadStatus(), loadOverview(), loadGraph()]);
    b.disabled = false;
  }

  function init() {
    if (typeof d3 === 'undefined') {
      showBanner('The page could not load its chart library.', true);
      return;
    }
    renderStrip();
    renderHome();
    emptyState($('map'), 'Loading…');
    emptyState($('graph'), 'Loading…');
    if (!KEY) {
      const e = { code: 'bad_key' };
      state.overviewErr = e;
      showBanner(explain(e), true);
      emptyState($('map'), 'No key', explain(e));
      emptyState($('graph'), 'No key', explain(e));
      return;
    }
    $('search').addEventListener('submit', (ev) => {
      ev.preventDefault();
      const q = $('q').value.trim();
      if (q) search(q);
    });
    $('clear-search').addEventListener('click', clearSearch);
    $('refresh').addEventListener('click', refreshAll);
    // Redraw when a panel changes size (the SVGs are drawn at pixel size).
    const sizes = new Map();
    const rerender = debounce(() => {
      const changed = (id) => {
        const b = $(id);
        const key = b.clientWidth + 'x' + b.clientHeight;
        if (sizes.get(id) === key) return false;
        sizes.set(id, key);
        return true;
      };
      if (changed('map') && state.overview) renderMap();
      if (changed('graph') && state.graph) renderGraph();
    }, 150);
    if (typeof ResizeObserver !== 'undefined') {
      const ro = new ResizeObserver(rerender);
      ro.observe($('map'));
      ro.observe($('graph'));
    } else {
      window.addEventListener('resize', rerender);
    }
    refreshAll();
    setInterval(loadStatus, 10000);
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
