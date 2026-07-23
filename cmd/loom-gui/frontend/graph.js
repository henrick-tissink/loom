// graph.js — the delegation graph (orchestration-view §5, §7, §8).
//
// Hand-rolled SVG, zero dependencies. The three properties this file exists to
// hold, each of which a graph library would have cost us (§15):
//
//  1. NODE IDENTITY. Every node card is one <g> held in a Map keyed by node id.
//     A status tick mutates attributes on those exact nodes and never touches
//     the layers' children — that is what makes §7.3's "no re-layout, no
//     innerHTML on the graph host" checkable rather than aspirational, and it is
//     the same lesson main.js:1418 already paid for with a half-typed rename.
//  2. LAYOUT DETERMINISM. Same topology in, byte-identical coordinates out.
//     Ties break on node id, never on map iteration order, so an unrelated
//     status tick cannot shuffle the picture under the user's eye.
//  3. TERMINATION UNDER A CYCLE. §9 is emphatic that layout survives one.
//     Ranking is bounded relaxation, the chain walk carries an on-path guard,
//     and neither can hang on a manifest that names a cycle.
//
// Layout is NOT here. §5.6 puts it in Go (internal/arch/layout.go) and has the
// frontend paint returned coordinates, and that is what this file does: a node
// arrives with x/y/rank already on it and the stage size arrives beside them.
// The reason is §12 — "the same manifest yields byte-identical coordinates
// across 100 runs" is a binding test, this repo has no JS test runner and may
// not add a dependency to get one, so a layout living here could only ever
// CLAIM determinism in a comment.
//
// The local fallback below is the one exception and is not a second layout of
// the same thing: §9's >N-node band view is a CLIENT-SIDE aggregation over
// local UI state (the collapse threshold and the expanded flag), so its
// synthetic band nodes never pass through the server and carry no coordinates.
// Real topology always uses the server's.

const SVGNS = "http://www.w3.org/2000/svg";

// Card geometry. Wide-and-short because §5.6 chose left-to-right: a node card
// carries a title, a repo chip and a badge, and a top-down layout spends the
// stage's horizontal budget on nothing.
//
// These are DEFAULTS. The authority is internal/arch's NodeW/NodeH, which the
// payload carries in `layout` — the painter needs the card size both to draw
// the rectangle and to aim an edge at a node's midpoint, and two copies of a
// number that must agree is how a graph ends up with arrows landing beside
// their nodes. They are only used when no server geometry is present, i.e. the
// band fallback.
let NODE_W = 224;
let NODE_H = 58;
const GAP_X = 88;
const GAP_Y = 28;

// States that mean "this node is finished as far as the graph is concerned".
// `verified` counts because 3a's merge gate is a human running git (delegate's
// §10 is deferred): a verified task is no longer blocking its consumers'
// artifacts, it is waiting on a person, and the blocked-on-you strip is where
// that is said.
const DONE_STATES = new Set(["merged", "verified"]);
const LIVE_STATES = new Set(["spawning", "running", "checking"]);

export function isDone(state) { return DONE_STATES.has(state); }

// ---- layout ---------------------------------------------------------------

// rankOf ranks the BAND nodes only — the real graph's ranks come from the
// server. Longest-path depth over the edge set, computed by BOUNDED
// relaxation rather than a topological sort.
//
// A topological sort has to answer "is there a cycle" before it can answer
// "what rank" — and §9 requires a rank even when the answer is yes. Relaxation
// capped at n passes returns a usable rank for a DAG (it converges in at most
// depth passes) and a bounded, deterministic one for an SCC, with no special
// case and no possibility of a hang. Edges the server flagged as part of a
// cycle are skipped first, which usually makes the remainder acyclic anyway.
function rankOf(nodes, edges) {
  const rank = new Map(nodes.map((n) => [n.id, 0]));
  const live = edges.filter((e) => !e.cycle && rank.has(e.from) && rank.has(e.to));
  // In a DAG the longest path is at most n-1 edges, so the clamp is a no-op on
  // every well-formed manifest. It binds only inside an unflagged cycle, where
  // relaxation would otherwise keep pushing ranks up within a pass and lay the
  // SCC out across a hundred columns of empty stage.
  const cap = Math.max(0, nodes.length - 1);
  for (let pass = 0; pass < nodes.length; pass++) {
    let changed = false;
    for (const e of live) {
      const want = Math.min(cap, rank.get(e.from) + 1);
      if (rank.get(e.to) < want) { rank.set(e.to, want); changed = true; }
    }
    if (!changed) break;
  }
  return rank;
}

// serverGeometry reads §5.6's coordinates off the payload. It computes nothing:
// if every node carries a position, that IS the layout, and the stage size
// comes from `layout` beside them.
//
// Returns null when the nodes carry no coordinates, which is the band view (and
// only the band view) — see the header.
function serverGeometry(nodes, dto) {
  if (!nodes.length || !dto || !dto.width || !dto.height) return null;
  const pos = new Map();
  for (const n of nodes) {
    if (typeof n.x !== "number" || typeof n.y !== "number") return null;
    pos.set(n.id, { x: n.x, y: n.y, rank: n.rank || 0 });
  }
  if (dto.nodeW > 0) NODE_W = dto.nodeW;
  if (dto.nodeH > 0) NODE_H = dto.nodeH;
  // `ranks` is only read by the chain highlight, which walks ids; the columns
  // are rebuilt here rather than shipped because they are derivable and a
  // second representation of the same fact is a second thing to keep in sync.
  const ranks = [];
  for (const n of nodes) {
    const r = n.rank || 0;
    while (ranks.length <= r) ranks.push([]);
    ranks[r].push(n.id);
  }
  return { pos, ranks, width: dto.width, height: dto.height };
}

// bandLayout is the fallback described in the header: a layered layout for the
// SYNTHETIC band nodes §9's >N-node view creates on the client. It is
// deliberately the same shape as internal/arch's so the two pictures read
// alike, and it is never applied to real topology.
function bandLayout(nodes, edges) {
  // Strides are read at CALL time, not at module load: NODE_W/NODE_H are the
  // server's once a real graph has been painted, and a stride frozen at import
  // would silently disagree with the card it is spacing.
  const STRIDE_X = NODE_W + GAP_X;
  const STRIDE_Y = NODE_H + GAP_Y;
  const rank = rankOf(nodes, edges);
  const maxRank = nodes.reduce((m, n) => Math.max(m, rank.get(n.id) || 0), 0);
  const columns = [];
  for (let r = 0; r <= maxRank; r++) columns.push([]);
  // Input order seeds every column, so a graph with no edges keeps the
  // manifest's own order instead of an alphabetical one nobody asked for.
  for (const n of nodes) columns[rank.get(n.id) || 0].push(n.id);

  const preds = new Map(nodes.map((n) => [n.id, []]));
  const succs = new Map(nodes.map((n) => [n.id, []]));
  for (const e of edges) {
    if (!preds.has(e.to) || !succs.has(e.from)) continue;
    preds.get(e.to).push(e.from);
    succs.get(e.from).push(e.to);
  }

  // Two barycenter sweeps (§5.6). More sweeps buy nothing measurable at the
  // node counts §9 caps this view at, and each one is another chance for the
  // ordering to oscillate between two equally-good arrangements — which the
  // user would see as the graph twitching on a manifest rewrite.
  const index = new Map();
  const reindex = () => {
    for (const col of columns) col.forEach((id, i) => index.set(id, i));
  };
  reindex();
  const sweep = (cols, neighbours) => {
    for (const col of cols) {
      const bary = new Map();
      for (const id of col) {
        const ns = neighbours.get(id).filter((x) => index.has(x));
        bary.set(id, ns.length ? ns.reduce((a, x) => a + index.get(x), 0) / ns.length : index.get(id));
      }
      col.sort((a, b) => (bary.get(a) - bary.get(b)) || (a < b ? -1 : a > b ? 1 : 0));
      col.forEach((id, i) => index.set(id, i));
    }
  };
  sweep(columns.slice(1), preds);
  sweep(columns.slice(0, -1).reverse(), succs);
  reindex();

  const tallest = columns.reduce((m, c) => Math.max(m, c.length), 1);
  const height = tallest * STRIDE_Y - GAP_Y;
  const pos = new Map();
  columns.forEach((col, r) => {
    const top = (height - (col.length * STRIDE_Y - GAP_Y)) / 2;
    col.forEach((id, i) => pos.set(id, { x: r * STRIDE_X, y: top + i * STRIDE_Y, rank: r }));
  });
  return {
    pos,
    ranks: columns,
    width: Math.max(NODE_W, (maxRank + 1) * STRIDE_X - GAP_X),
    height: Math.max(NODE_H, height),
  };
}

// ---- badge / state vocabulary ---------------------------------------------

// badgeFor is §2.1 in one function, and it is the single most load-bearing
// mapping in this file: a node's COMPLETION badge may only come from a recorded
// check result, never from a session's self-report.
//
// Hence "awaiting check": a child whose lifecycle still says running but whose
// pane has gone quiet — done, idle, or asking the human something — has made no
// verifiable statement about being finished, and rendering it green (or even
// "done") would promote exactly the self-report the whole arc is written
// against. The session status decides only that the child has STOPPED, never
// that it succeeded.
export function badgeFor(st) {
  const state = (st && st.state) || "pending";
  const check = (st && st.checkStatus) || "";
  const sess = (st && st.sessionStatus) || "";
  switch (state) {
    case "merged": return { text: "merged", cls: "dg-b-done" };
    case "verified": return { text: "verified", cls: "dg-b-ok" };
    case "failed": return { text: "check failed", cls: "dg-b-bad" };
    case "abandoned": return { text: "abandoned", cls: "dg-b-off" };
    case "blocked": return { text: "blocked", cls: "dg-b-warn" };
    case "checking": return { text: "checking", cls: "dg-b-live" };
    case "ready": return { text: "ready", cls: "dg-b-ready" };
    case "approved": return { text: "approved", cls: "dg-b-ready" };
    case "spawning": return { text: "spawning", cls: "dg-b-live" };
    case "running":
      if (!check && sess && sess !== "running") return { text: "awaiting check", cls: "dg-b-wait" };
      return { text: "running", cls: "dg-b-live" };
    default:
      return { text: st && st.ready ? "ready" : "planned", cls: st && st.ready ? "dg-b-ready" : "dg-b-plan" };
  }
}

// ---- the painted graph -----------------------------------------------------

export function createGraph(host, opts = {}) {
  const svg = svgEl("svg", "dg-svg");
  svg.setAttribute("role", "img");
  svg.appendChild(markers());
  const view = svgEl("g", "dg-view");
  const edgeLayer = svgEl("g", "dg-edges");
  const nodeLayer = svgEl("g", "dg-nodes");
  view.appendChild(edgeLayer);
  view.appendChild(nodeLayer);
  svg.appendChild(view);
  host.replaceChildren(svg);

  let nodes = [];
  let byNode = new Map();    // id → node, so a status tick is O(n) and not O(n²)
  let edges = [];
  let geom = { pos: new Map(), width: 0, height: 0 };
  // The server's stage geometry for the current topology (§5.6), or null for
  // the band fallback.
  let layoutDTO = null;
  const cards = new Map();   // id → { g, card, title, sub, badge, dot, warn, bneck }
  let edgeRefs = [];         // { path, from, to, edge }
  let tx = 0, ty = 0, k = 1;
  let chainOn = false;
  let lastStatuses = new Map();
  let lastBottleneck = "";

  function applyTransform() {
    view.setAttribute("transform", `translate(${tx.toFixed(2)},${ty.toFixed(2)}) scale(${k.toFixed(4)})`);
  }

  // Pan is a drag on the BACKGROUND only: a drag that started on a card would
  // fight the click that opens the inspector, and §5.6 forbids node dragging
  // outright (a hand-positioned graph is a promise re-layout cannot keep).
  let drag = null;
  svg.addEventListener("pointerdown", (e) => {
    if (e.target.closest(".dg-node")) return;
    drag = { x: e.clientX, y: e.clientY, tx, ty };
    svg.classList.add("dragging");
    svg.setPointerCapture(e.pointerId);
  });
  svg.addEventListener("pointermove", (e) => {
    if (!drag) return;
    tx = drag.tx + (e.clientX - drag.x);
    ty = drag.ty + (e.clientY - drag.y);
    applyTransform();
  });
  const endDrag = () => { drag = null; svg.classList.remove("dragging"); };
  svg.addEventListener("pointerup", endDrag);
  svg.addEventListener("pointercancel", endDrag);

  // ⌘-scroll zooms about the cursor; a bare scroll is left to the page, which
  // is the overview's own scroll. Trackpad users scroll past this graph
  // constantly and a bare-wheel zoom would trap the page under their fingers.
  svg.addEventListener("wheel", (e) => {
    if (!e.metaKey && !e.ctrlKey) return;
    e.preventDefault();
    const r = svg.getBoundingClientRect();
    const px = e.clientX - r.left, py = e.clientY - r.top;
    const next = Math.min(2.2, Math.max(0.25, k * (e.deltaY < 0 ? 1.1 : 1 / 1.1)));
    tx = px - (px - tx) * (next / k);
    ty = py - (py - ty) * (next / k);
    k = next;
    applyTransform();
  }, { passive: false });

  function fit() {
    const r = svg.getBoundingClientRect();
    const w = r.width || host.clientWidth || 900;
    const h = r.height || host.clientHeight || 360;
    if (!geom.width || !geom.height) return;
    k = Math.min(1, (w - 32) / geom.width, (h - 32) / geom.height);
    if (!isFinite(k) || k <= 0) k = 1;
    tx = (w - geom.width * k) / 2;
    ty = (h - geom.height * k) / 2;
    applyTransform();
  }

  function setTopology(nextNodes, nextEdges, nextLayout) {
    nodes = nextNodes || [];
    layoutDTO = nextLayout || null;
    byNode = new Map(nodes.map((n) => [n.id, n]));
    // A self-edge is dropped rather than drawn: it carries no ordering
    // information, and elbow() has no honest route for one.
    edges = (nextEdges || []).filter((e) => e.from !== e.to && byNode.has(e.from) && byNode.has(e.to));
    // Server coordinates when the payload has them (§5.6), the band fallback
    // otherwise. Never both for the same node set.
    geom = serverGeometry(nodes, layoutDTO) || bandLayout(nodes, edges);
    cards.clear();
    edgeRefs = [];
    edgeLayer.replaceChildren();
    nodeLayer.replaceChildren();
    for (const e of edges) edgeRefs.push(paintEdge(e));
    for (const n of nodes) cards.set(n.id, paintNode(n));
    applyTransform();
    // Re-apply whatever the last tick said, so a rev change does not blank the
    // badges for the 1.5s until the next poll.
    if (lastStatuses.size) patch(lastStatuses, { bottleneck: lastBottleneck });
  }

  function paintEdge(e) {
    const path = svgEl("path", "dg-edge" + (e.cycle ? " dg-edge-cycle" : ""));
    const a = geom.pos.get(e.from), b = geom.pos.get(e.to);
    if (a && b) path.setAttribute("d", elbow(a, b));
    path.setAttribute("marker-end", e.cycle ? "url(#dg-arrow-bad)" : "url(#dg-arrow)");
    if (e.artifact) {
      const t = svgEl("title");
      t.textContent = "artifact: " + e.artifact;
      path.appendChild(t);
    }
    edgeLayer.appendChild(path);
    return { path, from: e.from, to: e.to, edge: e };
  }

  function paintNode(n) {
    const p = geom.pos.get(n.id) || { x: 0, y: 0 };
    const g = svgEl("g", "dg-node" + (n.hidden ? " dg-hidden" : "") + (n.kind === "ghost" ? " dg-ghost" : "") +
      (n.kind === "band" ? " dg-band" : ""));
    g.setAttribute("transform", `translate(${p.x},${p.y})`);
    g.setAttribute("tabindex", "0");

    const card = svgEl("rect", "dg-card");
    card.setAttribute("width", NODE_W);
    card.setAttribute("height", NODE_H);
    card.setAttribute("rx", 9);
    g.appendChild(card);

    // Two lines, and the split is deliberate: the title owns line one, and line
    // two carries the three marks that must never be confused with each other —
    // the session dot (decorative), the repo (identity) and the lifecycle badge
    // (authoritative). Text lengths are clipped to fixed budgets because SVG
    // text does not wrap and an overlong title would run straight through the
    // badge, which is the one glyph on this card that must always be readable.
    const title = svgEl("text", "dg-title");
    title.setAttribute("x", 12);
    title.setAttribute("y", 21);
    title.textContent = clip(n.hidden ? "hidden" : (n.title || n.id), 22);
    g.appendChild(title);

    const dot = svgEl("circle", "dg-dot");
    dot.setAttribute("cx", 17);
    dot.setAttribute("cy", 37);
    dot.setAttribute("r", 4);
    dot.style.display = "none";
    g.appendChild(dot);

    const sub = svgEl("text", "dg-sub");
    sub.setAttribute("x", 28);
    sub.setAttribute("y", 41);
    sub.textContent = n.hidden ? "" : clip(n.repo || (n.kind === "ghost" ? "unresolved dependency" : ""), 15);
    g.appendChild(sub);

    const badge = svgEl("text", "dg-badge");
    badge.setAttribute("x", NODE_W - 12);
    badge.setAttribute("y", 41);
    badge.setAttribute("text-anchor", "end");
    g.appendChild(badge);

    // §2.3 / §2.5 / §9: not isolated · no authorization scope · no check
    // declared. A chip, not a tooltip — these are defects worth SEEING, and the
    // count is on the card so a glance across the graph finds them.
    const warn = svgEl("text", "dg-warn");
    warn.setAttribute("x", NODE_W - 12);
    warn.setAttribute("y", 21);
    warn.setAttribute("text-anchor", "end");
    if ((n.warnings || []).length) {
      warn.textContent = "⚠ " + n.warnings.length;
      const t = svgEl("title");
      t.textContent = n.warnings.join(" · ");
      warn.appendChild(t);
    }
    g.appendChild(warn);

    const bneck = svgEl("text", "dg-bneck");
    bneck.setAttribute("x", 12);
    bneck.setAttribute("y", NODE_H + 14);
    g.appendChild(bneck);

    if (!n.hidden && opts.onNode) {
      g.addEventListener("click", () => opts.onNode(n.id));
      g.addEventListener("keydown", (e) => { if (e.key === "Enter") opts.onNode(n.id); });
    }
    nodeLayer.appendChild(g);
    return { g, card, title, sub, badge, dot, warn, bneck };
  }

  // patch is §7.3's status tick: attributes only, on the SAME DOM nodes, with
  // no layout and no innerHTML. Everything that moves lives here; everything
  // that forces a re-layout lives in setTopology, and the split is the reason a
  // check result cannot destroy an open inspector or a panned viewport.
  function patch(byId, extra = {}) {
    lastStatuses = byId instanceof Map ? byId : new Map(Object.entries(byId || {}));
    lastBottleneck = extra.bottleneck || "";
    for (const [id, ref] of cards) {
      const st = lastStatuses.get(id);
      const node = byNode.get(id);
      const b = badgeFor(st);
      ref.badge.textContent = node && node.kind === "ghost" ? "unknown" : b.text;
      ref.badge.setAttribute("class", "dg-badge " + (node && node.kind === "ghost" ? "dg-b-off" : b.cls));
      const sess = st && st.sessionStatus;
      if (sess && opts.statusColor) {
        ref.dot.style.display = "";
        ref.dot.setAttribute("fill", opts.statusColor(sess));
        let t = ref.dot.querySelector("title");
        if (!t) { t = svgEl("title"); ref.dot.appendChild(t); }
        t.textContent = "session: " + sess;
      } else {
        // §9: a node with no session yet is drawn planned and HOLLOW. An absent
        // dot is the honest render; a grey one would claim a session exists.
        ref.dot.style.display = "none";
      }
      ref.g.classList.toggle("dg-blocked", !!(st && (st.blocked || st.seedFailed || st.pendingSeed)));
      ref.g.classList.toggle("dg-ready", !!(st && st.ready && !isDone(st && st.state)));
      ref.g.classList.toggle("dg-done", isDone(st && st.state));
      ref.g.classList.toggle("dg-bottleneck", id === lastBottleneck);
      ref.bneck.textContent = id === lastBottleneck ? "bottleneck" : "";
    }
    paintWaitEdges();
    if (chainOn) paintChain(true);
  }

  // §5.1's wait-edge: an edge lights ONLY when it is costing time right now.
  //
  // "Right now" is three conditions, and dropping any one of them turns the
  // animation into decoration: the producer is not finished (so the dependency
  // is genuinely unmet), the consumer is not ready and not itself finished (so
  // it is actually parked on this), and the consumer's own session is not
  // running (an edge into a node that is merrily working is not a wait). One
  // animation, one meaning — §8's motion budget spends its first of two moments
  // here.
  function paintWaitEdges() {
    for (const ref of edgeRefs) {
      const from = lastStatuses.get(ref.from);
      const to = lastStatuses.get(ref.to);
      const producerDone = isDone(from && from.state);
      const consumerDone = isDone(to && to.state);
      const parked = !!to && (to.state === "blocked" || (!to.ready && !LIVE_STATES.has(to.state)));
      const working = !!to && to.sessionStatus === "running";
      const lit = !producerDone && !consumerDone && parked && !working;
      ref.path.classList.toggle("lit", lit);
    }
  }

  // The longest REMAINING chain, in nodes. Highlighted on hover of the run
  // strip's figure and never on its own — §8: nothing pans, zooms or focuses
  // itself in response to a status change.
  //
  // The number rendered in the strip is the server's (arch.go's longestChain);
  // this walk only recovers WHICH path it is, from the same rule over the same
  // filtered node set. If the two ever disagree the number stays authoritative
  // and only the highlight is wrong, which is the failure mode worth having.
  function chainPath() {
    const kids = new Map(nodes.map((n) => [n.id, []]));
    for (const e of edges) if (kids.has(e.from)) kids.get(e.from).push(e.to);
    const memo = new Map();
    const onPath = new Set();
    const walk = (id) => {
      const st = lastStatuses.get(id);
      if (isDone(st && st.state)) return [];
      if (memo.has(id)) return memo.get(id);
      if (onPath.has(id)) return []; // a cycle contributes nothing; it never recurses twice
      onPath.add(id);
      let best = [];
      for (const kid of kids.get(id) || []) {
        const p = walk(kid);
        if (p.length > best.length) best = p;
      }
      onPath.delete(id);
      const path = [id, ...best];
      memo.set(id, path);
      return path;
    };
    let longest = [];
    for (const n of nodes) {
      const p = walk(n.id);
      if (p.length > longest.length) longest = p;
    }
    return new Set(longest);
  }

  function paintChain(on) {
    chainOn = on;
    const set = on ? chainPath() : new Set();
    for (const [id, ref] of cards) ref.g.classList.toggle("dg-chain", set.has(id));
    for (const ref of edgeRefs) ref.path.classList.toggle("chain", set.has(ref.from) && set.has(ref.to));
  }

  return {
    setTopology,
    patch,
    fit,
    highlightChain: paintChain,
    node: (id) => cards.get(id) || null,
    size: () => geom,
  };
}

// ---- svg plumbing ----------------------------------------------------------

function svgEl(tag, cls) {
  const n = document.createElementNS(SVGNS, tag);
  if (cls) n.setAttribute("class", cls);
  return n;
}

function markers() {
  const defs = svgEl("defs");
  for (const [id, cls] of [["dg-arrow", "dg-head"], ["dg-arrow-bad", "dg-head dg-head-bad"]]) {
    const m = svgEl("marker");
    m.setAttribute("id", id);
    m.setAttribute("viewBox", "0 0 10 10");
    m.setAttribute("refX", "9");
    m.setAttribute("refY", "5");
    m.setAttribute("markerWidth", "7");
    m.setAttribute("markerHeight", "7");
    m.setAttribute("orient", "auto-start-reverse");
    const p = svgEl("path", cls);
    p.setAttribute("d", "M0,1 L9,5 L0,9 z");
    m.appendChild(p);
    defs.appendChild(m);
  }
  return defs;
}

// elbow routes producer → consumer with a short straight run into the target,
// so many-into-one reads as a bundle rather than a fan (§5.6). A backward edge
// — which in 3a only ever comes from a cycle — is routed over the top of the
// graph rather than back through the cards.
function elbow(a, b) {
  const sx = a.x + NODE_W, sy = a.y + NODE_H / 2;
  const ex = b.x, ey = b.y + NODE_H / 2;
  if (ex <= sx) {
    const band = -22;
    return `M${sx},${sy} L${sx + 18},${sy} L${sx + 18},${band} L${ex - 18},${band} L${ex - 18},${ey} L${ex},${ey}`;
  }
  const mid = sx + Math.max(18, (ex - sx) / 2);
  if (Math.abs(ey - sy) < 1) return `M${sx},${sy} L${ex},${ey}`;
  const r = Math.min(10, Math.abs(ey - sy) / 2, (ex - sx) / 2);
  const dir = ey > sy ? 1 : -1;
  return `M${sx},${sy} L${mid - r},${sy} Q${mid},${sy} ${mid},${sy + dir * r}` +
    ` L${mid},${ey - dir * r} Q${mid},${ey} ${mid + r},${ey} L${ex},${ey}`;
}

function clip(s, n) {
  s = String(s || "");
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}
