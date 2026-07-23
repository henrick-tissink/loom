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
// that note rather than from a `Lang === "mermaid"` test here, so the day the
// subset parser lands there is one place that changes and no second opinion
// about what "unsupported" means.
function renderCode(b, ctx) {
  const fig = el("div", "doc-code" + (b.Unclosed ? " doc-code-warn" : ""));
  if (b.Note || b.Info) {
    const head = el("div", "doc-codehead");
    const lang = el("span", "doc-lang");
    lang.textContent = b.Info || b.Lang || "";
    head.appendChild(lang);
    if (b.Note) {
      const note = el("span", "doc-chip" + (b.Unclosed ? " doc-chip-bad" : ""));
      note.textContent = b.Note;
      head.appendChild(note);
    }
    if (ctx.docPath && ctx.openEditor) {
      const open = el("button", "tact");
      open.textContent = "open in editor";
      open.addEventListener("click", () => ctx.openEditor(ctx.docPath));
      head.appendChild(open);
    }
    fig.appendChild(head);
  }
  const pre = el("pre", "doc-pre");
  const code = el("code");
  code.textContent = b.Text || "";
  pre.appendChild(code);
  fig.appendChild(pre);
  return fig;
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
