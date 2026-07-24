// doc.js — the architecture reader (orchestration-view §4.3, §4.4).
//
// internal/arch hands the frontend a TOKEN TREE, never an HTML string, and this
// module is the reason that shape was chosen: every token below becomes a DOM
// node through createElement/textContent, so `<img src=x onerror=alert(1)>` in
// an agent-authored document arrives as a text token and paints as the twenty
// characters it is. There is no innerHTML anywhere in this file, and adding one
// would reopen the single injection site the token tree was designed to remove.
//
// Wire shape, easy to get wrong: arch.Block / arch.Inline / arch.OutlineEntry
// carry NO json tags, so the keys are the Go field names — Kind, Text, Children,
// Items, Head, Rows — not the lowercase spellings every other DTO in this app
// uses. Read internal/arch/md.go, not the convention.

const BLOCK = {
  heading: renderHeading,
  paragraph: renderParagraph,
  code: renderCode,
  list: renderList,
  quote: renderQuote,
  table: renderTable,
  rule: renderRule,
};

function el(tag, cls) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  return n;
}

// resolvePath joins a document-relative href onto the document's directory and
// folds `.` / `..` lexically. It is a DISPLAY convenience only: the resulting
// path goes to OpenInEditor, which stats the file and refuses anything that is
// not a regular file, and every path the reader can reach came out of a
// document that already passed §4.2's containment check server-side. This is
// not a second admission check and must never be mistaken for one.
export function resolvePath(dir, href) {
  if (!href) return "";
  if (href.startsWith("/")) return href;
  const parts = (dir || "").split("/").concat(href.split("/"));
  const out = [];
  for (const p of parts) {
    if (p === "" || p === ".") continue;
    if (p === "..") { out.pop(); continue; }
    out.push(p);
  }
  return "/" + out.join("/");
}

// renderBlocks paints a token tree into a fresh element.
//
// ctx = { dir, openEditor(absPath), openURL(url) }. The two openers are the
// EXISTING, already-tested gates (§4.4): nothing here re-derives which schemes
// are safe — internal/arch already classified every link and blanked the href
// of anything inert, so an inert token has nothing left to leak.
export function renderBlocks(blocks, ctx) {
  const host = el("div", "doc-body");
  for (const b of blocks || []) host.appendChild(renderBlock(b, ctx));
  return host;
}

function renderBlock(b, ctx) {
  const fn = BLOCK[b.Kind];
  if (!fn) {
    // A kind this build does not know still reaches the page as something.
    // §4.4: there is no failure mode where the pane is empty because of a
    // construct the renderer did not recognise.
    const p = el("p", "doc-p");
    p.textContent = b.Text || "";
    return p;
  }
  return fn(b, ctx);
}

function renderHeading(b, ctx) {
  // h1 in a rendered document would compete with the app's own chrome, so the
  // levels are shifted down one and clamped. The Slug comes from Go so an
  // outline row and its heading agree without the frontend inventing an id.
  const lvl = Math.min(6, Math.max(2, (b.Level || 1) + 1));
  const h = el("h" + lvl, "doc-h doc-h" + Math.min(3, b.Level || 1));
  if (b.Slug) h.id = "doc-" + b.Slug;
  inlineInto(h, b.Inline, ctx);
  return h;
}

function renderParagraph(b, ctx) {
  const p = el("p", "doc-p");
  inlineInto(p, b.Inline, ctx);
  return p;
}

// renderCode is also stage 4d's permanent graceful fallback. A mermaid fence
// arrives with Note = "mermaid — shown as source"; the chip is rendered from
// that note rather than from a `Lang === "mermaid"` test here, so there is one
// place that decides what "unsupported" means and no second opinion about it.
//
// Block.Text is the verbatim source in EVERY case, drawn or not, which is why a
// drawn diagram still carries a collapsed source view below it: the source is
// the permanent fallback (view spec §6), and a reader who suspects the picture
// is wrong must be able to check without leaving the pane.
function renderCode(b, ctx) {
  const fig = el("div", "doc-code" + (b.Unclosed ? " doc-code-warn" : ""));
  const drawn = b.Diagram && b.Diagram.Status === "ok";
  if (b.Note || b.Info) {
    const head = el("div", "doc-codehead");
    const lang = el("span", "doc-lang");
    lang.textContent = b.Info || b.Lang || "";
    head.appendChild(lang);
    if (b.Note) {
      // The chip text is composed in Go (arch.Diagram.Note) and its exact
      // wording is covered by that package's tests. It is never rebuilt here:
      // two spellings of "shown as source" is how the two come to disagree.
      //
      // The chip's WEIGHT is this file's decision and §6 binds it: a diagram
      // type outside the subset is a legible outcome, not a failure, and must
      // look deliberate — a quiet chip on an ordinary code block. A MALFORMED
      // diagram inside the subset is a real authoring mistake and gets the red
      // treatment, with the line number Go already put in the note. Rendering
      // both the same way teaches the reader to ignore the one that matters.
      const bad = b.Unclosed || (b.Diagram && b.Diagram.Status === "error");
      const note = el("span", "doc-chip" + (bad ? " doc-chip-bad" : ""));
      note.textContent = b.Note;
      head.appendChild(note);
      if (bad) fig.classList.add("doc-code-warn");
    }
    if (ctx.docPath && ctx.openEditor) {
      const open = el("button", "tact");
      open.textContent = "open in editor";
      open.addEventListener("click", () => ctx.openEditor(ctx.docPath));
      head.appendChild(open);
    }
    fig.appendChild(head);
  }
  if (drawn) {
    fig.appendChild(paintDiagram(b.Diagram));
    // Collapsed, not hidden. Open would double every diagram's height; absent
    // would remove the fallback the whole design rests on.
    const det = el("details", "doc-dgsrc");
    const sum = el("summary");
    sum.textContent = "source";
    det.appendChild(sum);
    det.appendChild(sourcePre(b));
    fig.appendChild(det);
    return fig;
  }
  fig.appendChild(sourcePre(b));
  return fig;
}

function sourcePre(b) {
  const pre = el("pre", "doc-pre");
  const code = el("code");
  code.textContent = b.Text || "";
  pre.appendChild(code);
  return pre;
}

// ---- stage 4d: the mermaid subset, painted ---------------------------------
//
// The parse, the vocabulary and the LAYOUT all happen in Go (internal/arch).
// This function paints coordinates it is handed and decides nothing: no
// ranking, no ordering, and in particular no orientation. Placements arrive
// ALREADY oriented for Diagram.Direction — including TD/TB's rank-down-the-page
// transform and RL's mirror — so re-reading Direction here to rotate anything
// would apply it twice, and the second application is invisible on the one
// direction (LR) most fences use.
//
// No mermaid.js, by decision (view spec §6): the picture is structured data on
// the wire, and the same Go layout engine draws the delegation graph. What this
// file owns is DOM.

const SVGNS = "http://www.w3.org/2000/svg";

// Card geometry, and the coupling is deliberate but NOT free: these are
// internal/arch's exported NodeW/NodeH, which is what Placements were computed
// against. They are constants there and are not on the wire, so a change in Go
// that is not mirrored here mis-places every label inside its box. Named after
// their source so a grep from either side finds the other.
const DG_NODE_W = 224;
const DG_NODE_H = 58;

// The painter's vocabulary. These are LOOM's names, not mermaid's — internal/arch
// normalises `[]`, `()`, `{}` and `([])` into these four — and an unrecognised
// value must be VISIBLE rather than silently drawn as a rectangle. A diagram
// that quietly loses a decision node is worse than one that says it could not
// draw it, because only the second is reported.
const DG_SHAPES = new Set(["rect", "round", "diamond", "stadium"]);
const DG_STYLES = new Set(["arrow", "open", "dotted", "thick"]);

function paintDiagram(d) {
  const host = el("div", "doc-dg");
  const w = Math.max(1, d.Width || DG_NODE_W);
  const h = Math.max(1, d.Height || DG_NODE_H);
  const svg = svgEl("svg", "doc-dgsvg");
  svg.setAttribute("role", "img");
  // Width/Height bound the stage; the viewBox is what makes the picture scale
  // to the reading pane instead of clipping. A little padding, because a card
  // whose stroke sits exactly on the viewBox edge loses half of it.
  const pad = 8;
  svg.setAttribute("viewBox", `${-pad} ${-pad} ${w + pad * 2} ${h + pad * 2}`);
  svg.setAttribute("preserveAspectRatio", "xMidYMin meet");
  svg.appendChild(dgMarkers());

  const pos = new Map();
  for (const p of d.Placements || []) pos.set(p.ID, p);

  // Bands first, so a subgraph is behind its members rather than over them.
  const bandLayer = svgEl("g", "doc-dgbands");
  const edgeLayer = svgEl("g", "doc-dgedges");
  const nodeLayer = svgEl("g", "doc-dgnodes");
  svg.appendChild(bandLayer);
  svg.appendChild(edgeLayer);
  svg.appendChild(nodeLayer);

  for (const sg of d.Subgraphs || []) {
    const band = paintBand(sg, pos);
    if (band) bandLayer.appendChild(band);
  }
  for (const e of d.Edges || []) {
    const from = pos.get(e.From), to = pos.get(e.To);
    // A node with no placement cannot be routed to. Skipped rather than drawn
    // from the origin, which would paint a line to the corner of the stage and
    // read as a real edge.
    if (!from || !to || e.From === e.To) continue;
    edgeLayer.appendChild(paintDgEdge(e, from, to));
  }
  for (const n of d.Nodes || []) {
    const p = pos.get(n.ID);
    if (!p) continue;
    nodeLayer.appendChild(paintDgNode(n, p));
  }
  host.appendChild(svg);
  return host;
}

// A band's bounding box is the union of its members' placements. It is computed
// here and not in Go on purpose: it is a paint decision (how much air a title
// needs above a card) and nothing about the topology depends on it.
function paintBand(sg, pos) {
  const boxes = (sg.Nodes || []).map((id) => pos.get(id)).filter(Boolean);
  if (!boxes.length) return null;
  const x0 = Math.min(...boxes.map((p) => p.X)) - 14;
  const y0 = Math.min(...boxes.map((p) => p.Y)) - 30;
  const x1 = Math.max(...boxes.map((p) => p.X + DG_NODE_W)) + 14;
  const y1 = Math.max(...boxes.map((p) => p.Y + DG_NODE_H)) + 14;
  const g = svgEl("g", "doc-dgband");
  const r = svgEl("rect", "doc-dgbandbox");
  r.setAttribute("x", x0);
  r.setAttribute("y", y0);
  r.setAttribute("width", x1 - x0);
  r.setAttribute("height", y1 - y0);
  r.setAttribute("rx", 10);
  g.appendChild(r);
  const t = svgEl("text", "doc-dgbandtitle");
  t.setAttribute("x", x0 + 12);
  t.setAttribute("y", y0 + 19);
  t.textContent = dgClip(sg.Title || sg.ID || "", 40);
  g.appendChild(t);
  return g;
}

function paintDgNode(n, p) {
  const shape = DG_SHAPES.has(n.Shape) ? n.Shape : "";
  const g = svgEl("g", "doc-dgnode" + (shape ? "" : " doc-dgunknown"));
  g.setAttribute("transform", `translate(${p.X},${p.Y})`);
  g.appendChild(dgShape(shape));

  const label = svgEl("text", "doc-dgtitle");
  label.setAttribute("x", DG_NODE_W / 2);
  label.setAttribute("y", DG_NODE_H / 2 + 5);
  label.setAttribute("text-anchor", "middle");
  // SVG text does not wrap, so the label is clipped to a budget that keeps it
  // inside the card. The full text is on the <title>, which is also what a
  // screen reader announces.
  label.textContent = dgClip(n.Label || n.ID || "", 26);
  g.appendChild(label);

  const tip = svgEl("title");
  tip.textContent = shape ? (n.Label || n.ID || "")
    : `${n.Label || n.ID || ""} — unknown shape "${n.Shape}", drawn as a rectangle`;
  g.appendChild(tip);

  if (!shape) {
    // VISIBLE, not silent. A value this build does not know still paints, and
    // says so on the card, because the alternative is a diagram that has
    // quietly changed meaning.
    const mark = svgEl("text", "doc-dgmark");
    mark.setAttribute("x", DG_NODE_W - 8);
    mark.setAttribute("y", 14);
    mark.setAttribute("text-anchor", "end");
    mark.textContent = "shape? " + dgClip(n.Shape || "—", 12);
    g.appendChild(mark);
  }
  return g;
}

function dgShape(shape) {
  if (shape === "diamond") {
    const poly = svgEl("polygon", "doc-dgcard");
    const w = DG_NODE_W, h = DG_NODE_H;
    poly.setAttribute("points", `${w / 2},0 ${w},${h / 2} ${w / 2},${h} 0,${h / 2}`);
    return poly;
  }
  const r = svgEl("rect", "doc-dgcard");
  r.setAttribute("width", DG_NODE_W);
  r.setAttribute("height", DG_NODE_H);
  // stadium is a full pill; round is a soft corner; rect (and every unknown
  // shape) is the plain box.
  r.setAttribute("rx", shape === "stadium" ? DG_NODE_H / 2 : (shape === "round" ? 14 : 3));
  return r;
}

// A STRAIGHT line between the two boxes' borders, deliberately, where the
// delegation graph uses an elbow. The elbow assumes rank runs left to right;
// these placements may be ranked down the page (TD/TB) or mirrored (RL), and an
// elbow drawn on that assumption routes edges through the cards it is meant to
// connect. A straight segment is correct in every orientation, which is the
// property that matters when the orientation is not this file's to know.
function paintDgEdge(e, a, b) {
  const style = DG_STYLES.has(e.Style) ? e.Style : "";
  const g = svgEl("g", "doc-dgedgeg");
  const ac = { x: a.X + DG_NODE_W / 2, y: a.Y + DG_NODE_H / 2 };
  const bc = { x: b.X + DG_NODE_W / 2, y: b.Y + DG_NODE_H / 2 };
  const from = dgBorderPoint(ac, bc);
  const to = dgBorderPoint(bc, ac);

  const line = svgEl("line", "doc-dgedge doc-dge-" + (style || "unknown") + (e.Cycle ? " doc-dgedge-cycle" : ""));
  line.setAttribute("x1", from.x);
  line.setAttribute("y1", from.y);
  line.setAttribute("x2", to.x);
  line.setAttribute("y2", to.y);
  // `open` is the one style that means "no arrowhead". Everything else gets
  // one, including an unknown style: an edge whose direction is dropped reads
  // as a different graph.
  if (style !== "open") line.setAttribute("marker-end", "url(#doc-dg-arrow)");
  g.appendChild(line);

  const parts = [];
  if (e.Label) parts.push(e.Label);
  if (!style) parts.push(`style? ${e.Style || "—"}`);
  if (parts.length) {
    const t = svgEl("text", "doc-dgelabel" + (style ? "" : " doc-dgmark"));
    t.setAttribute("x", (from.x + to.x) / 2);
    t.setAttribute("y", (from.y + to.y) / 2 - 5);
    t.setAttribute("text-anchor", "middle");
    t.textContent = dgClip(parts.join("  "), 34);
    g.appendChild(t);
  }
  return g;
}

// dgBorderPoint walks from one card's centre toward another and returns where
// it leaves the box. Rectangular clipping for every shape, diamond included:
// the small overshoot on a diamond's corners is invisible next to an arrowhead
// and costs none of the per-shape geometry that would have to be kept in step
// with dgShape.
function dgBorderPoint(from, to) {
  const dx = to.x - from.x, dy = to.y - from.y;
  if (dx === 0 && dy === 0) return { x: from.x, y: from.y };
  const hw = DG_NODE_W / 2, hh = DG_NODE_H / 2;
  const sx = dx === 0 ? Infinity : hw / Math.abs(dx);
  const sy = dy === 0 ? Infinity : hh / Math.abs(dy);
  const s = Math.min(sx, sy);
  return { x: from.x + dx * s, y: from.y + dy * s };
}

function dgMarkers() {
  const defs = svgEl("defs");
  const m = svgEl("marker");
  m.setAttribute("id", "doc-dg-arrow");
  m.setAttribute("viewBox", "0 0 10 10");
  m.setAttribute("refX", "9");
  m.setAttribute("refY", "5");
  m.setAttribute("markerWidth", "7");
  m.setAttribute("markerHeight", "7");
  m.setAttribute("orient", "auto-start-reverse");
  const p = svgEl("path", "doc-dghead");
  p.setAttribute("d", "M0,1 L9,5 L0,9 z");
  m.appendChild(p);
  defs.appendChild(m);
  return defs;
}

function svgEl(tag, cls) {
  const n = document.createElementNS(SVGNS, tag);
  if (cls) n.setAttribute("class", cls);
  return n;
}

function dgClip(s, n) {
  s = String(s || "");
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}

function renderList(b, ctx) {
  const list = el(b.Ordered ? "ol" : "ul", "doc-list");
  for (const it of b.Items || []) {
    const li = el("li");
    inlineInto(li, it.Inline, ctx);
    for (const child of it.Blocks || []) li.appendChild(renderBlock(child, ctx));
    list.appendChild(li);
  }
  return list;
}

function renderQuote(b, ctx) {
  const q = el("blockquote", "doc-quote");
  for (const child of b.Blocks || []) q.appendChild(renderBlock(child, ctx));
  return q;
}

// Tables scroll inside their own box. A wide table in an architecture document
// is routine, and letting one widen the overview is how the whole page starts
// scrolling sideways.
function renderTable(b, ctx) {
  const wrap = el("div", "doc-tablewrap");
  const t = el("table", "doc-table");
  if ((b.Head || []).length) {
    const thead = el("thead");
    const tr = el("tr");
    for (const c of b.Head) {
      const th = el("th");
      inlineInto(th, c.Inline, ctx);
      tr.appendChild(th);
    }
    thead.appendChild(tr);
    t.appendChild(thead);
  }
  const tbody = el("tbody");
  for (const row of b.Rows || []) {
    const tr = el("tr");
    for (const c of row) {
      const td = el("td");
      inlineInto(td, c.Inline, ctx);
      tr.appendChild(td);
    }
    tbody.appendChild(tr);
  }
  t.appendChild(tbody);
  wrap.appendChild(t);
  return wrap;
}

function renderRule() { return el("hr", "doc-rule"); }

function inlineInto(parent, nodes, ctx) {
  for (const n of nodes || []) {
    switch (n.Kind) {
      case "code": {
        const c = el("code", "doc-icode");
        c.textContent = n.Text || "";
        parent.appendChild(c);
        break;
      }
      case "emph": {
        const e = el("em");
        inlineInto(e, n.Children, ctx);
        parent.appendChild(e);
        break;
      }
      case "strong": {
        const s = el("strong");
        inlineInto(s, n.Children, ctx);
        parent.appendChild(s);
        break;
      }
      case "link":
        parent.appendChild(linkNode(n, ctx));
        break;
      case "image": {
        // Loom does not fetch an agent-authored image URL on a render path.
        // The alt text renders and the destination is offered through the same
        // gate as any other link, which is the whole of what §4.4 allows.
        const wrap = el("span", "doc-img");
        wrap.appendChild(document.createTextNode(n.Text || "image"));
        if (n.Target !== "inert" && n.Href) wrap.appendChild(linkNode({ ...n, Text: "open" }, ctx));
        parent.appendChild(wrap);
        break;
      }
      default:
        parent.appendChild(document.createTextNode(n.Text || ""));
    }
  }
}

function linkNode(n, ctx) {
  const label = (n.Children && n.Children.length) ? null : (n.Text || n.Href || "");
  if (n.Target === "inert" || !n.Href) {
    // An inert link keeps its LABEL and loses its destination — internal/arch
    // already blanked the href, so there is nothing here to accidentally
    // re-attach. Rendered as text with a quiet marker so a reader can see that
    // Loom declined to route it rather than that the document was malformed.
    const s = el("span", "doc-inert");
    if (label === null) inlineInto(s, n.Children, ctx); else s.textContent = label;
    s.title = "link target not routable";
    return s;
  }
  const a = el("a", "doc-link");
  if (label === null) inlineInto(a, n.Children, ctx); else a.textContent = label;
  a.title = n.Href;
  a.addEventListener("click", (e) => {
    e.preventDefault();
    if (n.Target === "browser") { if (ctx.openURL) ctx.openURL(n.Href); return; }
    if (ctx.openEditor) ctx.openEditor(resolvePath(ctx.dir, n.Href));
  });
  return a;
}

// renderOutline is §4.3's section outline, built from the h2/h3 entries Go
// already extracted. Clicking a row scrolls the heading into view inside the
// reader; it does not move the page, and it never focuses anything on its own —
// §8's motion budget has no room for a surface that steals the eye.
export function renderOutline(doc, scrollHost) {
  const nav = el("nav", "doc-outline");
  const entries = doc.Outline || [];
  if (!entries.length) {
    const e = el("div", "po-empty");
    e.textContent = "no sections";
    nav.appendChild(e);
    return nav;
  }
  for (const o of entries) {
    const row = el("button", "doc-orow doc-o" + (o.Level || 2));
    row.textContent = o.Text || "";
    row.addEventListener("click", () => {
      const target = scrollHost.querySelector("#doc-" + cssEscape(o.Slug));
      if (target) target.scrollIntoView({ block: "start", behavior: "auto" });
    });
    nav.appendChild(row);
  }
  return nav;
}

function cssEscape(s) {
  return (window.CSS && CSS.escape) ? CSS.escape(String(s)) : String(s).replace(/[^\w-]/g, "");
}
