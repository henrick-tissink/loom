import "./tokens.css";
import "@xterm/xterm/css/xterm.css";
import { statusColor, statusWord, xtermThemeFor } from "./theme.js";
import { createGraph, badgeFor } from "./graph.js";
import { renderBlocks, renderOutline } from "./doc.js";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { Unicode11Addon } from "@xterm/addon-unicode11";

const threadsEl = document.getElementById("threads");
const projectsEl = document.getElementById("projects");
const threadsHeadEl = document.getElementById("threads-head");
const attnEl = document.getElementById("attn");
const chipEl = document.getElementById("hidechip");
let activeName = null;

// Three-pane nav state (2026-07-24). selProject is a project root, or the
// ATTENTION sentinel for the cross-project needs-you queue. activeName (above)
// remains the attached session, so a thread stays highlighted while its project
// stays selected. Both the selected project and the collapse choice persist in
// localStorage — pure per-machine view state, no backend round-trip.
const ATTENTION = "__attention__";
let selProject = ATTENTION;
let manualCollapsed = false;
let navInit = false;
const PP_KEY = "loom.projects.collapsed";
const SEL_KEY = "loom.projects.selected";
try { manualCollapsed = localStorage.getItem(PP_KEY) === "1"; } catch { /* storage off */ }
try { const v = localStorage.getItem(SEL_KEY); if (v) selProject = v; } catch { /* storage off */ }
let latestSessions = [];
let latestRecent = [];
// Project rows (ListProjectDetails) — UNFILTERED by §6 on purpose: this is the
// list the hide/solo toggles live on, and a project that vanished from its own
// settings screen the moment it was hidden could never be unhidden.
let latestProjects = [];
// What the stage is showing: a session terminal, or a project overview. Held
// so the 1.5s poll can refresh the overview's session lists without stomping
// on a rename the user is mid-way through typing.
let stageView = { kind: "none" };
let termThemeMode = "light"; // "light" | "dark", from preferences

// bound resolves a Wails-bound method by name, or null. Two callers need this:
// the bindings injected after module-eval (GetPrefs already did this dance),
// and the handful of project affordances whose Go side lands after this
// frontend — an unbound method degrades to a typed path instead of a crash,
// per the house rule that failures degrade rather than take the window down.
function bound(name) {
  const app = window.go && window.go.main && window.go.main.App;
  return app && typeof app[name] === "function" ? app[name].bind(app) : null;
}

// Reflect the terminal theme on the DOM (the #terminal pane background) and,
// live, on any open terminal.
function applyTermTheme(mode) {
  termThemeMode = mode === "dark" ? "dark" : "light";
  document.documentElement.setAttribute("data-term-theme", termThemeMode);
  if (term) term.options.theme = xtermThemeFor(termThemeMode);
}

// Load the persisted terminal theme before the first session is opened.
// Guarded: the Wails binding may not be injected yet at module-eval time.
const _getPrefs = window.go && window.go.main && window.go.main.App && window.go.main.App.GetPrefs;
if (_getPrefs) {
  _getPrefs().then((p) => applyTermTheme(p && p.terminalTheme)).catch(() => {});
}

function renderAttention(sessions) {
  const n = sessions.filter((s) => s.status === "needs_you").length;
  if (n > 0) {
    attnEl.innerHTML = `<span class="attn-dot"></span>${n} ${n === 1 ? "needs" : "need"} you`;
  } else {
    attnEl.textContent = "";
  }
}

// The titlebar reserves 92px on the left for the macOS traffic-light buttons.
// Those buttons vanish in fullscreen, so drop the inset when fullscreen.
const titlebarEl = document.getElementById("titlebar");
async function syncFullscreen() {
  try {
    const fs = await window.runtime.WindowIsFullscreen();
    titlebarEl.classList.toggle("fullscreen", !!fs);
  } catch { /* runtime not ready */ }
}
window.addEventListener("resize", syncFullscreen);
syncFullscreen();

// Wrapped rather than passed directly: openLauncher's first argument is a
// preselected target path, and a raw listener would hand it a MouseEvent.
document.getElementById("new-session").addEventListener("click", () => openLauncher());
document.getElementById("search-btn").addEventListener("click", openSearch);
document.getElementById("prefs-btn").addEventListener("click", openPrefs);
document.getElementById("wf-btn").addEventListener("click", openWorkflows);
document.addEventListener("keydown", (e) => {
  // ⌘K only (not Ctrl+K) — Ctrl+K is the terminal's readline "kill line".
  if (e.metaKey && (e.key === "k" || e.key === "K")) {
    e.preventDefault();
    openPalette();
    return;
  }
  if (e.key === "Escape") {
    const m = document.querySelector(".modal-backdrop");
    if (m) { m.remove(); return; }
  }
  if (e.key === "/" && !isTyping() && !document.querySelector(".modal-backdrop")) {
    e.preventDefault();
    openSearch();
  }
});

function isTyping() {
  const ae = document.activeElement;
  return !!ae && (ae.tagName === "INPUT" || ae.tagName === "TEXTAREA");
}

// ---- icons ----
// One coherent glyph per status, each distinct — error was a copy of needs_you
// (both an exclamation) and unknown a copy of idle (both a dash), so a failed
// session and one that needs you, or a booting and an idle one, read alike.
const STATUS_ICON = {
  needs_you: '<path d="M12 8v5M12 16.5v.01"/>',   // ! — your turn, come back
  running: '<path d="M9 6l8 6-8 6z"/>',            // ▶ — working
  idle: '<path d="M6 12h12"/>',                    // — — waiting on you
  done: '<path d="M5 13l4 4L19 7"/>',              // ✓ — finished clean
  error: '<path d="M8 8l8 8M16 8l-8 8"/>',         // ✕ — finished with an error
  unknown: '<path d="M12 12h.01"/>',               // · — booting, no state yet
};
const FOLDER_ICON = '<path d="M3 7h6l2 2h10v10H3z"/>';

function icon(inner, w) {
  return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="${w || 2.6}" stroke-linecap="round" stroke-linejoin="round">${inner}</svg>`;
}
function esc(s) {
  return String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

// Context-window gauge: the last turn's approximate token footprint against a
// 200k window. Warns as it fills so you know when to /compact. Empty when the
// count is unknown (0).
const CTX_WINDOW = 200000;
function ctxGaugeHtml(tokens) {
  if (!tokens || tokens <= 0) return "";
  const pct = Math.min(100, Math.round((tokens / CTX_WINDOW) * 100));
  const k = tokens >= 1000 ? Math.round(tokens / 1000) + "k" : String(tokens);
  const cls = pct >= 90 ? " ctx-danger" : pct >= 75 ? " ctx-warn" : "";
  return `<span class="ctxbar${cls}" title="~${k} / 200k context tokens (${pct}%)"><i style="width:${pct}%"></i></span>`;
}

// ---- rail (status-grouped, attention first) ----
const GROUPS = [
  { key: "Needs you", match: (s) => s === "needs_you" },
  { key: "Running", match: (s) => s === "running" },
  { key: "Quiet", match: (s) => s !== "needs_you" && s !== "running" },
];

// The tmux session name is a "loom-<uuid>" id; show the AI title when present,
// otherwise a short id, so the rail never displays a raw uuid.
function displayName(s) {
  if (s.title && s.title.trim()) return s.title.trim();
  return "session " + s.name.replace(/^loom-/, "").slice(0, 8);
}

function actionBtn(pathInner, title, onClick, extraClass) {
  const b = document.createElement("button");
  b.className = "tact" + (extraClass ? " " + extraClass : "");
  b.title = title;
  b.innerHTML = icon(pathInner, 2.3);
  b.addEventListener("click", (e) => { e.stopPropagation(); onClick(); });
  return b;
}

// Sessions with a pending kill-confirm, keyed by name so the armed state
// survives the 1.5s poll rail rebuild (which discards the button node).
const armedKills = new Set();

// Finished sessions with an in-flight summarize, keyed by name so the
// "Summarizing…" state survives poll rebuilds (same idea as armedKills).
const summarizing = new Set();

// One-line preview of a multi-section summary (collapse whitespace).
function sumPreview(text) { return String(text).replace(/\s+/g, " ").trim(); }

// killButton needs a two-step confirm so a stray click can't nuke an agent.
function killButton(name) {
  const b = document.createElement("button");
  b.className = "tact tact-kill";
  b.title = "Kill session";
  const glyph = () => { b.innerHTML = icon('<path d="M6 6l12 12M18 6L6 18"/>', 2.3); };
  if (armedKills.has(name)) { b.classList.add("armed"); b.textContent = "Kill?"; }
  else { glyph(); }
  b.addEventListener("click", (e) => {
    e.stopPropagation();
    if (armedKills.has(name)) {
      armedKills.delete(name);
      window.go.main.App.KillSession(name).catch((err) => console.error("kill", err));
      poll();
    } else {
      armedKills.add(name);
      b.classList.add("armed"); b.textContent = "Kill?";
      setTimeout(() => {
        armedKills.delete(name);
        if (b.isConnected) { b.classList.remove("armed"); glyph(); }
      }, 2500);
    }
  });
  return b;
}

function appendGroup(label) {
  const gh = document.createElement("li");
  gh.className = "group";
  gh.textContent = label;
  threadsEl.appendChild(gh);
}

// projLabel overrides the sub-label (the repo basename) — the Attention queue
// mixes projects, so it shows each thread's PROJECT there instead.
function appendLiveRow(s, projLabel) {
  const li = document.createElement("li");
  li.className = "thread" + (s.name === activeName ? " active" : "");
  const color = statusColor(s.status);
  li.style.setProperty("--tc", color);
  li.innerHTML =
    `<span class="tglyph i-${esc(s.status)}">${icon(STATUS_ICON[s.status] || STATUS_ICON.unknown, 3)}</span>` +
    `<span class="tinfo"><span class="tname">${esc(displayName(s))}</span><span class="tproj">${esc(projLabel != null ? projLabel : s.project)}</span>${ctxGaugeHtml(s.ctxTokens)}</span>` +
    `<span class="tright"><span class="tstatus" style="color:${color}">${esc(statusWord(s.status))}</span><span class="tactions"></span></span>`;
  const acts = li.querySelector(".tactions");
  acts.appendChild(actionBtn('<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>', "Quick reply", () => openReply(s.name)));
  acts.appendChild(killButton(s.name));
  li.addEventListener("click", () => selectSession(s.name));
  threadsEl.appendChild(li);
}

function appendFinishedRow(s) {
  const li = document.createElement("li");
  li.className = "thread finished";
  const color = statusColor(s.status);
  li.style.setProperty("--tc", color);
  const busy = summarizing.has(s.name);
  let sumLine = "";
  if (busy) sumLine = `<span class="tsum tsum-busy">Summarizing…</span>`;
  else if (s.summary) sumLine = `<span class="tsum" title="${esc(s.summary)}">${esc(sumPreview(s.summary))}</span>`;
  li.innerHTML =
    `<span class="tglyph i-${esc(s.status)}">${icon(STATUS_ICON[s.status] || STATUS_ICON.unknown, 3)}</span>` +
    `<span class="tinfo"><span class="tname">${esc(displayName(s))}</span><span class="tproj">${esc(s.project)}</span>${sumLine}</span>` +
    `<span class="tright"><span class="tstatus" style="color:${color}">${esc(s.status)}</span><span class="tactions"></span></span>`;
  const acts = li.querySelector(".tactions");
  acts.appendChild(actionBtn('<path d="M12 3l1.6 5L19 9.6 13.6 11 12 16l-1.6-5L5 9.6 10.4 8z"/>', s.summary ? "Regenerate summary" : "Summarize", async () => {
    if (summarizing.has(s.name)) return;
    summarizing.add(s.name);
    renderRail(latestSessions, latestRecent); // show "Summarizing…" at once
    try { await window.go.main.App.SummarizeSession(s.name); }
    catch (e) { console.error("summarize", e); }
    summarizing.delete(s.name);
    poll();
  }));
  acts.appendChild(actionBtn('<path d="M3 12a9 9 0 1 0 3-6.7"/><path d="M3 3v5h5"/>', "Resume", async () => {
    try { const nn = await window.go.main.App.ResumeSession(s.name); if (nn) selectSession(nn); poll(); }
    catch (e) { console.error("resume", e); }
  }));
  acts.appendChild(actionBtn('<path d="M6 6l12 12M18 6L6 18"/>', "Dismiss from history", async () => {
    try { await window.go.main.App.DismissSession(s.name); poll(); } catch (e) { console.error("dismiss", e); }
  }));
  threadsEl.appendChild(li);
}

// ---- rail sections (§8) ----
// Sections are per PROJECT; today's status ordering is preserved INSIDE each
// one. Ordering across sections is one total order (§7): needs-you projects by
// name → Ungrouped if it holds a needs-you session → remaining projects by
// name → Ungrouped.
//
// Deliberately NOT strict project-then-status nesting: that buries an urgent
// session inside a collapsed group, and attention-first is what loom is for.
// The same reasoning is why a collapsed header still carries the needs-you dot.
const UNGROUPED_NAME = "Ungrouped";

// §6.1's predicate, in the frontend's own terms. The DTO lists arriving from
// the Go side are already filtered, but the PROJECT list is not — it is what
// the hide/solo toggles live on — so every surface that renders project names
// from it (the rail's index, the palette) must re-apply the rule or it becomes
// the leak the feature exists to prevent. A soloed project whose root is
// missing degrades to "nothing hidden", matching the Go resolver exactly.
function projectVisible(p) {
  const solo = latestProjects.find((x) => x.solo && !x.missing);
  if (solo) return p.root === solo.root;
  return !p.hidden;
}

// Roots whose rail section is collapsed. §8 puts this in loom.db beside the
// other project flags rather than a third store; until that binding exists the
// set is mirrored to localStorage so the state at least survives a reload, and
// the server's value wins the moment ListProjectDetails carries one.
const collapsedRoots = new Set();
const COLLAPSE_KEY = "loom.rail.collapsed";
try {
  for (const r of JSON.parse(localStorage.getItem(COLLAPSE_KEY) || "[]")) collapsedRoots.add(r);
} catch { /* corrupt or unavailable storage — start expanded */ }

// syncCollapseFromServer lets the Go side be the authority as soon as it
// carries the flag: a DTO without `collapsed` leaves the local mirror alone,
// so this is safe both before and after that binding lands.
function syncCollapseFromServer(projects) {
  if (!projects.some((p) => typeof p.collapsed === "boolean")) return;
  collapsedRoots.clear();
  for (const p of projects) if (p.collapsed) collapsedRoots.add(p.root);
}

function setCollapsed(root, on) {
  if (on) collapsedRoots.add(root); else collapsedRoots.delete(root);
  const persist = bound("SetProjectCollapsed");
  if (persist) persist(root, on).catch((e) => console.error("collapse", e));
  try { localStorage.setItem(COLLAPSE_KEY, JSON.stringify([...collapsedRoots])); }
  catch { /* storage full or disabled — collapse is then session-only */ }
}

// railSections buckets the (already §6-filtered) DTOs by project root and puts
// the buckets in §7's total order. Ungrouped is keyed by root "" — the store's
// reserved row — so no surface needs a second branch for "no project".
function railSections(sessions, recent) {
  const byRoot = new Map();
  const bucket = (root, name) => {
    let b = byRoot.get(root);
    if (!b) {
      b = { root, name: name || (root === "" ? UNGROUPED_NAME : root.split("/").pop()), live: [], finished: [] };
      byRoot.set(root, b);
    }
    return b;
  };
  for (const s of sessions) bucket(s.projectRoot || "", s.projectName).live.push(s);
  for (const s of recent || []) bucket(s.projectRoot || "", s.projectName).finished.push(s);

  const urgent = (b) => b.live.some((s) => s.status === "needs_you");
  const byName = (a, b) => a.name.localeCompare(b.name);
  const named = [...byRoot.values()].filter((b) => b.root !== "");
  const ung = byRoot.get("");
  return [
    ...named.filter(urgent).sort(byName),
    ...(ung && urgent(ung) ? [ung] : []),
    ...named.filter((b) => !urgent(b)).sort(byName),
    ...(ung && !urgent(ung) ? [ung] : []),
  ];
}

function appendSectionHead(sec) {
  const li = document.createElement("li");
  const isCollapsed = collapsedRoots.has(sec.root);
  const urgent = sec.live.filter((s) => s.status === "needs_you").length;
  li.className = "psec" + (isCollapsed ? " collapsed" : "") + (urgent ? " urgent" : "");
  li.innerHTML =
    `<button class="psec-caret" title="${isCollapsed ? "Expand" : "Collapse"}">${isCollapsed ? "▸" : "▾"}</button>` +
    `<span class="psec-name">${esc(sec.name)}</span>` +
    // The dot survives collapse on purpose: collapsing a section must never be
    // the reason an urgent session goes unnoticed.
    (urgent ? `<span class="psec-dot" title="${urgent} need you"></span>` : "") +
    `<span class="psec-count">${sec.live.length || ""}</span>`;
  li.querySelector(".psec-caret").addEventListener("click", (e) => {
    e.stopPropagation();
    setCollapsed(sec.root, !collapsedRoots.has(sec.root));
    renderRail(latestSessions, latestRecent);
  });
  // Clicking the header itself is §8's "replace the stage with the overview".
  li.addEventListener("click", () => openProject(sec.root));
  threadsEl.appendChild(li);
}

// The rail's tail: every visible project with no sessions right now, as a
// quiet index. Without it a freshly created (or simply idle) project would
// have no route to its overview — and the overview is where hide/solo,
// re-point and membership live.
function appendProjectIndex(shownRoots) {
  const rest = latestProjects
    .filter((p) => !p.ungrouped && !shownRoots.has(p.root) && projectVisible(p))
    .sort((a, b) => a.name.localeCompare(b.name));
  if (!rest.length) return;
  const key = "\0index"; // not a real root, so it can never collide
  const isCollapsed = collapsedRoots.has(key);
  const head = document.createElement("li");
  head.className = "psec psec-index" + (isCollapsed ? " collapsed" : "");
  head.innerHTML =
    `<button class="psec-caret">${isCollapsed ? "▸" : "▾"}</button>` +
    `<span class="psec-name">Projects</span><span class="psec-count">${rest.length}</span>`;
  head.addEventListener("click", () => {
    setCollapsed(key, !collapsedRoots.has(key));
    renderRail(latestSessions, latestRecent);
  });
  threadsEl.appendChild(head);
  if (isCollapsed) return;
  for (const p of rest) {
    const li = document.createElement("li");
    li.className = "prow" + (p.missing ? " missing" : "");
    li.innerHTML = `<span class="prow-name">${esc(p.name)}</span>` +
      (p.missing ? `<span class="prow-tag">missing</span>` : "");
    li.addEventListener("click", () => openProject(p.root));
    threadsEl.appendChild(li);
  }
}

// renderRail is kept as the single nav-refresh entry point (its six call sites
// are unchanged); it now paints the two nav panes. Args are ignored — every
// pane reads the poll's globals.
function renderRail() {
  renderProjectsPane();
  renderThreadsPane();
}

// ---- projects pane (2026-07-24 three-pane nav) ----

function effectiveCollapsed() { return window.innerWidth < 980 ? true : manualCollapsed; }

function applyCollapse() {
  const on = effectiveCollapsed();
  const body = document.getElementById("body");
  if (body) body.classList.toggle("pp-collapsed", on);
  const btn = document.getElementById("pp-collapse");
  if (btn) { btn.textContent = on ? "›" : "‹"; btn.title = on ? "Expand projects" : "Collapse projects"; }
  renderProjectsPane();
}

// Per-project needs-you / running counts from the (already §6-filtered) sessions.
function projectCounts() {
  const m = new Map();
  for (const s of latestSessions) {
    const root = s.projectRoot || "";
    let c = m.get(root); if (!c) { c = { need: 0, run: 0 }; m.set(root, c); }
    if (s.status === "needs_you") c.need++;
    else if (s.status === "running") c.run++;
  }
  return m;
}

// §3 order: needs-you (by count) → running → the rest alphabetical → Ungrouped
// (only when it holds sessions). Hidden/solo respected via projectVisible.
// A STABLE alphabetical order (Ungrouped last). Projects deliberately do NOT
// reorder by needs-you count: a live agent's status flips constantly (a turn
// ends → needs-you, you reply → running), and reordering on every flip made the
// whole list jump up and down while you read it. The needs-you count still
// shows as an in-place badge, and the pinned Attention entry is the one place
// attention floats to the top — so a project row never has to move.
function orderedProjects() {
  const counts = projectCounts();
  const withC = latestProjects
    .filter((p) => !p.ungrouped && projectVisible(p))
    .map((p) => ({ root: p.root, name: p.name, missing: p.missing, need: 0, run: 0, ...(counts.get(p.root) || {}) }))
    .sort((a, b) => a.name.localeCompare(b.name));
  const ungC = counts.get("");
  const ung = ungC ? [{ root: "", name: "Ungrouped", ung: true, need: ungC.need, run: ungC.run }] : [];
  return [...withC, ...ung];
}

function totalNeed() { return latestSessions.filter((s) => s.status === "needs_you").length; }

function initials(name) {
  const w = String(name).split(/[ /_.-]/).filter(Boolean);
  return ((w[0] ? w[0][0] : "?") + (w[1] ? w[1][0] : "")).toUpperCase();
}

function renderProjectsPane() {
  if (!projectsEl) return;
  const collapsed = effectiveCollapsed();
  const need = totalNeed();
  const projects = orderedProjects();
  projectsEl.replaceChildren();

  if (collapsed) {
    const a = document.createElement("li");
    a.className = "pj-tok" + (selProject === ATTENTION ? " on" : "");
    a.title = "Attention" + (need ? ` — ${need} need you` : "");
    a.innerHTML = `<span class="attn-dot"></span>` + (need ? `<span class="pj-badge">${need}</span>` : "");
    a.addEventListener("click", openAttention);
    projectsEl.appendChild(a);
    for (const p of projects) {
      const li = document.createElement("li");
      li.className = "pj-tok" + (selProject === p.root ? " on" : "") + (p.ung ? " ung" : "");
      li.title = p.name;
      li.innerHTML = (p.ung ? "·" : `<span>${esc(initials(p.name))}</span>`) +
        (p.need ? `<span class="pj-badge">${p.need}</span>` : (p.run ? `<span class="pj-tdot"></span>` : ""));
      li.addEventListener("click", () => selectProject(p.root));
      projectsEl.appendChild(li);
    }
    return;
  }

  const a = document.createElement("li");
  a.className = "pj-attn" + (selProject === ATTENTION ? " on" : "");
  a.innerHTML = `<span class="attn-dot"></span><span class="pj-name">Attention</span>` + (need ? `<span class="pj-count">${need}</span>` : "");
  a.addEventListener("click", openAttention);
  projectsEl.appendChild(a);
  const div = document.createElement("li"); div.className = "pj-div"; projectsEl.appendChild(div);
  for (const p of projects) {
    const li = document.createElement("li");
    li.className = "pj-row" + (selProject === p.root ? " on" : "") + (p.missing ? " missing" : "") + (p.ung ? " ung" : "");
    li.innerHTML = `<span class="pj-name">${esc(p.name)}</span><span class="pj-meta">` +
      (p.run && !p.need ? `<span class="pj-run"></span>` : "") +
      (p.need ? `<span class="pj-count">${p.need}</span>` : "") + `</span>`;
    li.addEventListener("click", () => selectProject(p.root));
    projectsEl.appendChild(li);
  }
}

// ---- threads pane (2026-07-24 three-pane nav) ----

function renderThreadsPane() {
  if (!threadsHeadEl || !threadsEl) return;
  threadsEl.replaceChildren();

  if (selProject === ATTENTION) {
    const queue = latestSessions.filter((s) => s.status === "needs_you");
    threadsHeadEl.innerHTML =
      `<div class="th-head"><span class="th-title"><span class="attn-dot"></span><span>Needs you</span></span></div>` +
      `<div class="th-sub"><span>${queue.length} across all projects</span></div>`;
    for (const s of queue) appendLiveRow(s, s.projectName || "");
    if (!queue.length) threadsEl.innerHTML = `<div class="th-empty">Nothing needs you right now.<br><span>Walk away — loom pulls you back.</span></div>`;
    return;
  }

  const p = projectByRoot(selProject);
  const name = p ? p.name : (selProject === "" ? "Ungrouped" : selProject.split("/").pop());
  const live = latestSessions.filter((s) => (s.projectRoot || "") === selProject);
  const fin = latestRecent.filter((s) => (s.projectRoot || "") === selProject);
  const repoCount = p && p.repos ? p.repos.length : 0;
  threadsHeadEl.innerHTML =
    `<div class="th-head"><span class="th-title"><span>${esc(name)}</span></span><button class="th-new" id="th-new">+ New</button></div>` +
    (p && !p.ungrouped && selProject !== "" ? `<div class="th-sub"><span>${repoCount} repo${repoCount === 1 ? "" : "s"}</span><a class="th-spacer" id="th-overview">overview</a></div>` : "");
  const nb = document.getElementById("th-new"); if (nb) nb.addEventListener("click", () => openLauncher());
  const ov = document.getElementById("th-overview"); if (ov) ov.addEventListener("click", () => openProject(selProject));

  // A flat, STABLE list — live sessions newest-launched-first (latestSessions
  // is created_at desc), each showing its status in place (glyph + colour +
  // word). No status sub-grouping: a status flip must not move a row. The only
  // membership change is the one-way live→finished transition (rare), which
  // drops a row into the FINISHED section below.
  for (const s of live) appendLiveRow(s);
  if (fin.length) { appendGroup("Finished"); for (const s of fin) appendFinishedRow(s); }
  if (!live.length && !fin.length) threadsEl.innerHTML = `<div class="th-empty">No threads in ${esc(name)} yet.<br><span>Press + New to start one.</span></div>`;
}

function selectProject(root) {
  try { localStorage.setItem(SEL_KEY, root); } catch { /* storage off */ }
  openProject(root); // sets the stage to the overview, activeName=null, re-renders nav
}

function openAttention() {
  selProject = ATTENTION;
  try { localStorage.setItem(SEL_KEY, ATTENTION); } catch { /* storage off */ }
  teardownTerminal();
  activeName = null;
  stageView = { kind: "attention" };
  renderAttentionStage();
  renderRail();
}

function renderAttentionStage() {
  const stage = document.getElementById("stage");
  if (!stage) return;
  stage.replaceChildren();
  const d = document.createElement("div");
  d.id = "stage-empty";
  d.textContent = latestSessions.some((s) => s.status === "needs_you")
    ? "Pick a thread on the left to attend to it." : "Nothing needs you right now.";
  stage.appendChild(d);
}

// On the first poll (once the DTOs exist) sync the stage to the restored
// selection, and apply the collapse state. Runs once.
function initNav() {
  applyCollapse();
  if (selProject !== ATTENTION && projectByRoot(selProject)) openProject(selProject);
  else { selProject = ATTENTION; stageView = { kind: "attention" }; renderAttentionStage(); renderRail(); }
}

function renderStageHeader(name) {
  const el = document.getElementById("stage-header");
  if (!el) return;
  const s = latestSessions.find((x) => x.name === name);
  const status = s ? s.status : "unknown";
  const project = s ? s.project : "";
  const label = s ? displayName(s) : name;
  const color = statusColor(status);
  el.className = "stage-head";
  el.innerHTML =
    `<span class="sh-name">${esc(label)}</span>` +
    (project ? `<span class="sh-proj">${icon(FOLDER_ICON, 2)}${esc(project)}</span>` : "") +
    `<span class="sh-pill"><i style="background:${color}"></i><span style="color:${color}">${esc(statusWord(status))}</span></span>` +
    `<button class="sh-btn" id="sh-diff" title="Review uncommitted changes">${icon('<circle cx="6" cy="6" r="2.4"/><circle cx="6" cy="18" r="2.4"/><circle cx="18" cy="9" r="2.4"/><path d="M6 8.4v7.2M18 11.4v.6a4 4 0 0 1-4 4H8.4"/>', 2)}Diff</button>`;
  const diffBtn = el.querySelector("#sh-diff");
  if (diffBtn) diffBtn.addEventListener("click", () => openDiff(name));
}

// ---- hide/solo chip (§6.4) ----
// Permanent, in the titlebar, and NEVER an identity-bearing needs-you count:
// "3 need you" over a two-session demo re-leaks exactly what hiding concealed.
// Restore is armed-confirm (the killButton idiom) so one stray click mid
// screen-share cannot undo it.
let chipArmed = false;
let chipTimer = null;

function renderHideChip() {
  if (!chipEl) return;
  if (chipArmed) return; // don't overwrite the armed label under the user
  const solo = latestProjects.find((p) => p.solo);
  const hidden = latestProjects.filter((p) => p.hidden).length;
  let text = "", cls = "hidechip";
  if (solo && !solo.missing) {
    text = "solo: " + solo.name;
    cls += " solo";
  } else if (solo && solo.missing) {
    // The resolver degrades a missing solo root to "nothing hidden" rather
    // than to "everything hidden". Say so: silently believing a demo is
    // protected when it is not is the failure mode this whole feature exists
    // to avoid.
    text = "solo: " + solo.name + " — unavailable";
    cls += " warn";
  } else if (hidden > 0) {
    text = hidden + " hidden";
  }
  chipEl.className = cls;
  chipEl.textContent = text;
  chipEl.title = text ? "Click twice to restore everything" : "";
}

function disarmChip() {
  chipArmed = false;
  clearTimeout(chipTimer);
  renderHideChip();
}

if (chipEl) {
  chipEl.addEventListener("click", async () => {
    if (!chipEl.textContent) return;
    if (!chipArmed) {
      chipArmed = true;
      chipEl.classList.add("armed");
      chipEl.textContent = "Restore?";
      chipTimer = setTimeout(disarmChip, 2500);
      return;
    }
    disarmChip();
    const solo = bound("SetProjectSolo"), hide = bound("SetProjectHidden");
    for (const p of latestProjects) {
      if (p.solo && solo) await solo(p.root, false).catch((e) => console.error("solo", e));
      if (p.hidden && hide) await hide(p.root, false).catch((e) => console.error("hide", e));
    }
    poll();
  });
}

// ---- small prompt modal (rename / re-point / add repo) ----
// Directory input is a typed path plus a "Choose…" button, and the button only
// appears when the picker binding is present — the Go side owns the native
// dialog, so an unbound picker degrades to typing rather than to a dead modal.
function promptModal({ title, label, value = "", placeholder = "", dir = false, okText = "Save" }) {
  if (document.querySelector(".modal-backdrop")) return Promise.resolve(null);
  return new Promise((resolve) => {
    const picker = dir ? bound("OpenDirectoryDialog") : null;
    const backdrop = document.createElement("div");
    backdrop.className = "modal-backdrop";
    backdrop.innerHTML = `
      <div class="modal" role="dialog" aria-label="${esc(title)}">
        <h2>${esc(title)}</h2>
        <div class="field">
          <label for="pm-input">${esc(label)}</label>
          <div class="pm-row">
            <input id="pm-input" class="search-input" type="text" value="${esc(value)}" placeholder="${esc(placeholder)}" autocomplete="off" spellcheck="false" />
            ${picker ? `<button class="btn-ghost" id="pm-pick">Choose…</button>` : ""}
          </div>
        </div>
        <div class="modal-error" id="pm-error"></div>
        <div class="modal-actions">
          <button class="btn-ghost" id="pm-cancel">Cancel</button>
          <button class="btn-launch" id="pm-ok">${esc(okText)}</button>
        </div>
      </div>`;
    document.body.appendChild(backdrop);
    const input = backdrop.querySelector("#pm-input");
    input.focus();
    input.select();
    // The global Escape handler removes any open backdrop. Without resolving
    // here, that path would leave the caller awaiting a promise forever — so
    // the modal owns its own Escape and settles before the node goes.
    const onEsc = (e) => { if (e.key === "Escape") done(null); };
    document.addEventListener("keydown", onEsc, true);
    const done = (v) => {
      document.removeEventListener("keydown", onEsc, true);
      backdrop.remove();
      resolve(v);
    };
    backdrop.addEventListener("click", (e) => { if (e.target === backdrop) done(null); });
    backdrop.querySelector("#pm-cancel").addEventListener("click", () => done(null));
    if (picker) backdrop.querySelector("#pm-pick").addEventListener("click", async () => {
      try { const p = await picker(title); if (p) input.value = p; }
      catch (e) { backdrop.querySelector("#pm-error").textContent = String(e); }
    });
    const ok = () => { const v = input.value.trim(); if (v) done(v); };
    backdrop.querySelector("#pm-ok").addEventListener("click", ok);
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") { e.preventDefault(); ok(); }
      else if (e.key === "Escape") { e.preventDefault(); done(null); }
    });
  });
}

// ---- project overview (§8) ----
// Clicking a rail section header replaces the stage with this. The overview
// above #po-orch / #po-arch is unchanged from slice 1: the orchestration layer
// renders INTO those two seams and nothing above them reads either node, which
// is what keeps slice 1's overview behaviour meaning what it says.
function projectByRoot(root) {
  return latestProjects.find((p) => p.root === root) || null;
}

function openProject(root) {
  teardownTerminal();
  activeName = null;
  selProject = root; // keep the projects pane in sync (openProject has many callers)
  stageView = { kind: "project", root };
  resetOrchestration(root);
  renderProject();
  renderRail(); // drop the active-session highlight, reflect the selected project
}

async function projectAction(fn) {
  try { await fn(); } catch (e) { console.error("project", e); flashProjectError(String(e)); return; }
  await poll();
  if (stageView.kind === "project") renderProject();
}

function flashProjectError(msg) {
  const el = document.getElementById("po-error");
  if (el) el.textContent = msg;
}

function projectSessionsHtml(root) {
  const live = latestSessions.filter((s) => (s.projectRoot || "") === root);
  const fin = latestRecent.filter((s) => (s.projectRoot || "") === root);
  const row = (s, kind) =>
    `<li class="po-sess" data-name="${esc(s.name)}" data-kind="${kind}">
       <span class="po-dot" style="background:${statusColor(s.status)}"></span>
       <span class="po-sname">${esc(displayName(s))}</span>
       <span class="po-sstatus">${esc(kind === "live" ? statusWord(s.status) : s.status)}</span>
     </li>`;
  const block = (title, rows) =>
    `<div class="po-sub">${title}</div>` +
    (rows.length ? `<ul class="po-list">${rows.join("")}</ul>` : `<div class="po-empty">none</div>`);
  return block("Live", live.map((s) => row(s, "live"))) +
    block("Finished", fin.map((s) => row(s, "finished")));
}

function wireProjectSessions(host) {
  host.querySelectorAll(".po-sess").forEach((li) => {
    const name = li.getAttribute("data-name");
    if (li.getAttribute("data-kind") === "live") {
      li.addEventListener("click", () => selectSession(name));
    } else {
      li.addEventListener("click", async () => {
        try { const nn = await window.go.main.App.ResumeSession(name); if (nn) selectSession(nn); poll(); }
        catch (e) { console.error("resume", e); }
      });
    }
  });
}

function refreshProjectSessions() {
  const host = document.getElementById("po-sessions");
  if (!host) return;
  host.innerHTML = projectSessionsHtml(stageView.root);
  wireProjectSessions(host);
}

function renderProject() {
  const stage = document.getElementById("stage");
  const p = projectByRoot(stageView.root);
  stage.replaceChildren();
  if (!p) {
    stage.innerHTML = `<div id="stage-empty">This project is no longer in loom.db.</div>`;
    return;
  }
  const repoRow = (r) => `
    <li class="po-repo${r.missing ? " missing" : ""}" data-path="${esc(r.path)}">
      <span class="po-rlabel">${esc(r.label)}</span>
      <span class="po-rpath">${esc(r.path)}</span>
      ${r.missing ? `<span class="po-tag">missing</span>` : ""}
      <span class="po-racts">
        <button class="tact po-launch"${r.missing ? " disabled" : ""} title="${r.missing ? "Directory is gone — re-point the project first" : "New session here"}">launch</button>
        <button class="tact po-remove" title="Move this repo to a project of its own">remove</button>
      </span>
    </li>`;

  stage.innerHTML = `
    <div id="stage-header" class="stage-head po-head">
      <span class="sh-name">${esc(p.name)}</span>
      ${p.hidden ? `<span class="po-tag">hidden</span>` : ""}
      ${p.solo ? `<span class="po-tag po-tag-solo">solo</span>` : ""}
      ${p.missing ? `<span class="po-tag">missing</span>` : ""}
      <span class="sh-proj">${icon(FOLDER_ICON, 2)}${esc(p.root || "no directory")}</span>
      <span class="po-acts">
        <button class="sh-btn" id="po-hide">${p.hidden ? "Show" : "Hide"}</button>
        <button class="sh-btn" id="po-solo">${p.solo ? "Leave solo" : "Solo"}</button>
        <button class="sh-btn" id="po-rename">Rename</button>
        <button class="sh-btn" id="po-repoint">Re-point</button>
      </span>
    </div>
    <div class="po">
      <div class="modal-error" id="po-error"></div>
      <div id="po-warnings"></div>
      <section class="po-block">
        <div class="po-bhead"><h3>Repos</h3><button class="sh-btn" id="po-addrepo">Add repo</button></div>
        ${(p.repos || []).length ? `<ul class="po-list">${p.repos.map(repoRow).join("")}</ul>` : `<div class="po-empty">No repos — the project root is the launch target.</div>`}
      </section>
      <section class="po-block">
        <div class="po-bhead"><h3>Sessions</h3></div>
        <div id="po-sessions"></div>
      </section>
      <section class="po-block" id="po-orch" hidden></section>
      <!-- The orchestration seam. Slice 1 left it empty and its comment said
           "slice 2"; slice ordering moved after slice 1 shipped (2 = orchestrator
           + brief, 4 = rendering), and a stale seam comment is how the next
           reader mis-attributes the whole view. Slice 4 renders INTO this node
           and adds nothing above it, so the overview's existing behaviour keeps
           meaning what it says. -->
      <div class="po-arch" id="po-arch" data-slot="orchestration"></div>
    </div>`;

  refreshProjectSessions();
  renderOrchestratorBlock();
  mountOrchestration();

  const ungrouped = p.ungrouped;
  const on = (id, fn) => { const el = document.getElementById(id); if (el) el.addEventListener("click", fn); };
  // Ungrouped is a real row in the model, but it owns no directory: renaming,
  // re-pointing or adding repos to it is meaningless, and soloing it is
  // explicitly suppressed by §6.1.
  for (const id of ["po-solo", "po-rename", "po-repoint", "po-addrepo"]) {
    const el = document.getElementById(id);
    if (el && ungrouped) el.disabled = true;
  }
  on("po-hide", () => projectAction(() => window.go.main.App.SetProjectHidden(p.root, !p.hidden)));
  on("po-solo", () => projectAction(() => window.go.main.App.SetProjectSolo(p.root, !p.solo)));
  on("po-rename", async () => {
    const name = await promptModal({ title: "Rename project", label: "Name", value: p.name });
    if (name) projectAction(() => window.go.main.App.RenameProject(p.root, name));
  });
  on("po-repoint", async () => {
    const dir = await promptModal({
      title: "Re-point project", label: "New root directory", value: p.root, dir: true, okText: "Re-point",
    });
    if (dir) projectAction(() => window.go.main.App.RepointProject(p.root, dir));
  });
  on("po-addrepo", async () => {
    const dir = await promptModal({ title: "Add repo", label: "Repo directory", dir: true, okText: "Add" });
    if (dir) projectAction(() => window.go.main.App.AddProjectRepo(p.root, dir));
  });
  stage.querySelectorAll(".po-repo").forEach((li) => {
    const path = li.getAttribute("data-path");
    const launch = li.querySelector(".po-launch");
    if (launch && !launch.disabled) launch.addEventListener("click", () => openLauncher(path));
    li.querySelector(".po-remove").addEventListener("click", () =>
      projectAction(() => window.go.main.App.RemoveProjectRepo(path)));
  });
  renderProjectWarnings();
}

// Warnings are the only place a repo skipped by reconciliation (a label
// collision, §2) becomes visible — discovery is never fatal, so without this
// the repo is simply absent from the launcher with no explanation anywhere.
async function renderProjectWarnings() {
  const host = document.getElementById("po-warnings");
  const list = bound("ProjectWarnings");
  if (!host || !list) return;
  let ws = [];
  try { ws = await list(); } catch { return; }
  if (!ws.length || !host.isConnected) return;
  host.innerHTML = `<div class="po-warns">${ws.map((w) => `<div class="po-warn">${esc(w)}</div>`).join("")}</div>`;
}

// ---- the orchestrator block (orchestrator spec §10's #po-orch) ----
//
// Written here, in slice 4's commit, because slice 2 shipped three bound methods
// — ProjectOrchestrator, SpawnOrchestrator, ReassembleBrief — and no surface
// that reaches any of them. It is deliberately the thin version §10 describes
// and NOTHING more: it renders no note content, because rich rendering is the
// architecture reader's job below and a shabby second copy here is work that
// would have to be deleted.
//
// It sits ABOVE #po-arch and never writes into it (§10, binding).
async function renderOrchestratorBlock() {
  const host = document.getElementById("po-orch");
  const read = bound("ProjectOrchestrator");
  if (!host || !read) return;
  const root = stageView.root;
  let dto = null;
  try { dto = await read(root); } catch (e) { console.error("orchestrator", e); return; }
  if (!host.isConnected || stageView.kind !== "project" || stageView.root !== root) return;
  // The hidden marker renders as nothing here: the seam below already carries
  // §3.1's one constant line, and a second, differently-worded one would be a
  // second chance to leak a bit.
  if (!dto || dto.hidden) { host.hidden = true; return; }

  const o = dto.orchestrator;
  const state = !o ? "none"
    : o.claiming ? "claiming — the spawn is in flight"
    : o.live ? "live since " + elapsedSince(o.spawnedAt) + " ago"
    : "ended " + elapsedSince(o.endedAt) + " ago";
  const brief = o && o.assembledAt
    ? `brief.md · ${Math.round((o.briefBytes || 0) / 1024)} KB · assembled ${elapsedSince(o.assembledAt)} ago`
    : "no brief assembled yet";
  host.hidden = false;
  host.innerHTML = `
    <div class="po-bhead">
      <h3>Orchestrator</h3>
      <span class="po-orch-state">${esc(state)}</span>
      <span class="po-acts">
        ${o && o.live && o.sessionName ? `<button class="sh-btn" id="po-attach-orch">Attach</button>` : `<button class="sh-btn" id="po-spawn-orch">Spawn</button>`}
        <button class="sh-btn" id="po-reassemble">Reassemble brief</button>
        <button class="sh-btn" id="po-notes">Move notes…</button>
      </span>
    </div>
    <div class="po-orch-facts">
      <span>${esc(brief)}</span>
      ${o && o.spawnCount ? `<span>${o.spawnCount} spawn${o.spawnCount === 1 ? "" : "s"}</span>` : ""}
      ${o && o.notesDir ? `<code>${esc(o.notesDir)}</code>` : `<span>notes not materialized yet</span>`}
    </div>
    ${(o && (o.truncatedSections || []).length)
      // A silently truncated brief is a brief whose missing half nobody knows
      // about, which is why slice 2 carries the section names at all.
      ? `<div class="po-warn">brief sections truncated: ${esc((o.truncatedSections || []).join(", "))}</div>` : ""}`;

  const on = (id, fn) => { const el = document.getElementById(id); if (el) el.addEventListener("click", fn); };
  on("po-attach-orch", () => selectSession(o.sessionName));
  on("po-spawn-orch", async () => {
    const intent = await promptModal({
      title: "Spawn orchestrator", label: "What is this orchestrator for?",
      placeholder: "e.g. re-architect the ledger across bankenstein and atlas", okText: "Spawn",
    });
    if (intent === null) return;
    projectAction(async () => {
      const name = await window.go.main.App.SpawnOrchestrator(root, intent || "");
      if (name) selectSession(name);
    });
  });
  on("po-reassemble", () => projectAction(() => window.go.main.App.ReassembleBrief(root)));
  on("po-notes", async () => {
    const dir = await promptModal({ title: "Move notes", label: "Notes directory", value: (o && o.notesDir) || "", dir: true, okText: "Move" });
    if (dir) projectAction(() => window.go.main.App.SetProjectNotesDir(root, dir));
  });
}

// ---- orchestration view (slice 4: 4a documents · 4b graph · 4c analysis) ----
//
// The seam slice 1 left at #po-arch expands into four stacked blocks, and §3
// binds the ORDER because the order is the argument: run strip, blocked-on-you,
// delegation graph, architecture & decisions. Blocked-on-you sits above the
// picture because the single highest-value fact on this page is "you are the
// thing holding this up", and a graph you have to read to discover that is a
// diagram rather than an instrument.
//
// Two rules from §7 shape everything below and are easy to erode:
//
//   - There are TWO update paths. A status tick patches DOM nodes in place; a
//     rev change re-lays-out. Collapsing them into one re-render would destroy
//     pan, zoom and an open inspector every 1.5s — the same failure main.js's
//     poll already avoids for a half-typed rename.
//   - Nothing here is ever a blank panel. §9's degradation matrix has a row for
//     every way this can fail and each one renders named, visible text.
//
//   - Layout is the SERVER's (§5.6). internal/arch/layout.go computes rank,
//     order and coordinates; GraphNodeDTO carries x/y/rank and the payload
//     carries the stage size, and graph.js paints them. It lives there and not
//     here because §12 binds "byte-identical coordinates across 100 runs" and
//     this frontend has no test runner and may not add a dependency to get one
//     — a determinism claim in a comment is not a determinism test. The one
//     layout that remains in graph.js is §9's band fallback, whose nodes are
//     synthesized on the client from local UI state and therefore never pass
//     through the server at all.

const orch = {
  root: null,
  runID: 0,          // §9's switcher selection — LOCAL UI state, never persisted
  rev: 0,            // last-seen topology fingerprint (§7.2)
  runs: [], run: null,
  nodes: [], edges: [], layout: null, statuses: [], strip: null, blocked: [],
  warnings: [], error: "",
  hidden: false, loaded: false, inFlight: false, unbound: false,
  docs: null, docsErr: "", docSel: null, docsLoading: false,
  docsRev: 0,        // last-seen document-set fingerprint (§7.4)
  graph: null, graphRun: 0, expanded: false, changed: false,
  stripSig: "", blockedSig: "", chipSig: "", msgSig: "",
  inspectorUpdate: null,

  // ---- §§9-12, and every one of these is fetched from a READ-ONLY binding ----
  //
  // OrchestrationSnapshot is the per-tick payload and it now carries the run's
  // red KIND and §5.1's edge kinds itself. What it still does not carry is the
  // wait-for cycle and the park DECLARATIONS, and the sources below fill those
  // in WITHOUT putting a write on the poll — TickDelegationRun runs checks,
  // integrations and seed deliveries, so a view that called it every 1.5s would
  // be executing agent-authored argv because a tab is open.
  //
  //   redKind/baselineFaults — from RunHeadDTO, in the snapshot. They used to
  //     cost a second ProjectDelegation call the instant a run went red; the
  //     values are a fold over the run row Go had already read, so the round
  //     trip bought nothing but a way for the discriminator to be missing.
  //     §10.2 vs §12.1 is the whole point: `deadlocked` means two completely
  //     different things and blaming a task for a broken baseline is the
  //     failure §10.2 exists to prevent.
  //   deadlock — RunDeadlock, a READ-ONLY binding that runs delegate's detector
  //     over already-persisted state. Fetched only while the run is red.
  //   parks — TaskPark per blocked child. Server-parsed, never re-parsed here:
  //     a JS copy of delegate.ParseBlock is a second definition of what a valid
  //     declaration is, and the second one is where the validation gets dropped.
  //   tick — the last TickReport a HUMAN asked for, kept so §11.3's amendment
  //     log survives the re-render that follows the press.
  redKind: "", baselineFaults: [],
  deadlock: null, redSig: "", redFetch: false,
  parks: new Map(), parkSig: "", parkOver: 0, parkFetch: false,
  tick: null, tickAt: 0,
  // baselineAlert is a §10.2 run-level fault observed THIS session, from an
  // integration or a merge. Held beside redKind because the two arrive by
  // different routes and the durable one can lag a press by a poll.
  baselineAlert: null,
  // The last RunIntegration read, for the strip's staging row. A read and not a
  // poll: the call re-fingerprints artifacts and §7.5 caps what a tick may do.
  integration: null,
  escSig: "", parkedSig: "", amendSig: "", topoSig: "",
};

// resetRunLocal drops everything keyed to ONE run. Called on a run switch as
// well as on a project change: a park declaration, a red kind and a tick report
// all name task ids that do not exist in the other run's graph, and carrying
// them across would render another run's deadlock over this one's picture.
function resetRunLocal() {
  orch.redKind = ""; orch.baselineFaults = []; orch.redSig = ""; orch.deadlock = null;
  orch.parks = new Map(); orch.parkSig = ""; orch.parkOver = 0;
  orch.tick = null; orch.tickAt = 0; orch.baselineAlert = null; orch.integration = null;
  orch.escSig = orch.parkedSig = orch.amendSig = orch.topoSig = "";
}

function resetOrchestration(root) {
  if (orch.root === root) return;
  orch.root = root;
  orch.runID = 0; orch.rev = 0;
  orch.runs = []; orch.run = null;
  orch.nodes = []; orch.edges = []; orch.layout = null; orch.statuses = []; orch.strip = null; orch.blocked = [];
  orch.warnings = []; orch.error = ""; orch.hidden = false; orch.loaded = false;
  orch.docs = null; orch.docsErr = ""; orch.docSel = null; orch.docsRev = 0;
  orch.graph = null; orch.graphRun = 0; orch.expanded = false; orch.changed = false;
  orch.stripSig = orch.blockedSig = orch.chipSig = orch.msgSig = "";
  orch.inspectorUpdate = null;
  resetRunLocal();
}

function statusMap() {
  return new Map((orch.statuses || []).map((s) => [s.id, s]));
}
function nodeById(id) {
  return (orch.nodes || []).find((n) => n.id === id) || null;
}

// mountOrchestration builds the seam's skeleton once per renderProject(). The
// per-tick path never touches these containers, only their contents — and the
// graph host specifically is written exactly twice: on mount, and on a rev
// change.
function mountOrchestration() {
  const seam = document.getElementById("po-arch");
  if (!seam) return;
  seam.innerHTML = `
    <div id="dg-hidden" class="dg-hiddenline"></div>
    <!-- §12.1's escalation, and it is FIRST because a silent all-blocked run
         looks exactly like healthy progress. Not a chip and not a toast: the
         whole hazard is that the state is quiet, so the render is a panel that
         names every task and artifact in the wait-for cycle. -->
    <div id="dg-escalate"></div>
    <div id="dg-strip"></div>
    <div id="dg-blocked"></div>
    <div id="dg-parked"></div>
    <section class="po-block" id="dg-graphblock" hidden>
      <div class="po-bhead"><h3>Delegation graph</h3><span class="dg-chips" id="dg-chips"></span></div>
      <div id="dg-msgs"></div>
      <div class="dg-host" id="dg-host"></div>
      <div class="dg-legend">drag to pan · ⌘-scroll to zoom · click a node for its brief, scope and check</div>
    </section>
    <section class="po-block" id="dg-runblock" hidden>
      <div class="po-bhead"><h3>Integration &amp; the plan</h3>
        <span class="dg-chips">
          <button class="tact" id="dg-runint">Integration &amp; limits</button>
          <button class="tact" id="dg-runtick">Advance run</button>
        </span>
      </div>
      <div id="dg-amend"></div>
      <div id="dg-runbody"></div>
    </section>
    <section class="po-block" id="po-docs"></section>`;
  orch.graph = null;
  // Every rendered signature, because the host elements were just replaced: a
  // surviving signature would compare equal against a container that no longer
  // holds the markup and leave the block permanently blank.
  orch.stripSig = orch.blockedSig = orch.chipSig = orch.msgSig = "";
  orch.escSig = orch.parkedSig = orch.amendSig = orch.topoSig = "";
  renderOrchestration();
  renderDocuments();
  // Both are fire-and-forget on the render path, so both swallow their own
  // failure: a rejected promise here must cost the seam, never the window.
  wireRunActions();
  refreshOrchestration().catch((e) => console.error("orchestration", e));
  refreshDocuments().catch((e) => console.error("documents", e));
}

// The two RUN-level presses, and they are presses rather than poll output on
// purpose.
//
// "Integration & limits" is a read, but an expensive one — it re-fingerprints
// every interface artifact a consumer needs — and §7.5's cost ceiling forbids
// paying that on the 1.5s tick. "Advance run" is a WRITE: a tick runs checks,
// integrations, seed deliveries and the watchdogs, and a surface that ran it on
// render would be executing agent-authored argv because someone opened a tab.
//
// Both are deliberately visible as buttons for the same reason: they are the
// two things a human does to an otherwise idle run, and a run that only moves
// when something invisible happens is a run nobody can unstick.
function wireRunActions() {
  const body = () => document.getElementById("dg-runbody");
  const runID = () => (orch.run ? orch.run.id : 0);
  const intBtn = document.getElementById("dg-runint");
  if (intBtn) intBtn.addEventListener("click", async () => {
    const call = bound("RunIntegration");
    const host = body();
    if (!call || !runID() || !host) return;
    intBtn.disabled = true;
    try {
      const d = await call(runID());
      host.innerHTML = runIntegrationHtml(d);
      // Kept for the strip's staging row. The verdicts are what §5.2's approval
      // rests on and they belong beside the run's own figures, not only inside
      // the panel someone pressed a button to open; the full panel — the
      // captured output and §10.5's limits — stays behind the press because
      // re-fingerprinting every interface artifact is what §7.5 caps.
      if (d && !d.hidden) { orch.integration = d; orch.stripSig = ""; renderOrchestration(); }
    }
    catch (e) { host.innerHTML = `<div class="po-warn">${esc(String(e))}</div>`; }
    intBtn.disabled = false;
  });
  const tickBtn = document.getElementById("dg-runtick");
  if (tickBtn) tickBtn.addEventListener("click", async () => {
    const call = bound("TickDelegationRun");
    const host = body();
    if (!call || !runID() || !host) return;
    tickBtn.disabled = true;
    tickBtn.textContent = "Advancing…";
    try {
      const rep = await call(runID());
      // KEPT, not just painted. The report is the only source of §12.1's
      // wait-for cycle and of §11.3's amendment log, and both are rendered by
      // the seam's own blocks — which re-render on every poll and would lose
      // them the moment this handler's markup was overwritten.
      if (rep && !rep.hidden) { orch.tick = rep; orch.tickAt = Math.floor(Date.now() / 1000); }
      host.innerHTML = tickHtml(rep);
    } catch (e) { host.innerHTML = `<div class="po-warn">${esc(String(e))}</div>`; }
    tickBtn.disabled = false;
    tickBtn.textContent = "Advance run";
    await refreshOrchestration();
    renderOrchestration();
  });
}

// ---- §§10-12: the three read-only sources the snapshot does not carry -------

// A run whose status is one of these is a run that has STOPPED, and §10.2 and
// §12.1 are the two completely different reasons why. The fetch below is gated
// on this set rather than run unconditionally: ProjectDelegation reads every
// task row of every run in the project, and paying that on a 1.5s tick for a
// run that is progressing normally is exactly the cost §7.5 caps.
//
// One entry, and it is exhaustive against the Go side rather than defensive:
// `delegation_runs.status` is only ever written `planning` (store/delegation.go),
// `running` (run.go) or `deadlocked` (run.go, integrate.go). A speculative
// "failed"/"abandoned" here would be dead weight in the cheap direction and
// actively wrong in the loud one — it would title an abandoned run "deadlocked
// — the tasks are waiting on each other" and demand a re-plan for a run a human
// deliberately stopped. A status a newer Loom adds falls through and escalates
// nothing, which is the failure worth having: the run strip still renders it
// verbatim beside the run name.
const RED_RUN_STATUSES = new Set(["deadlocked"]);

// The two readings of `deadlocked`, spelled exactly as orchestration.go exports
// them. They are compared, never composed: an unknown value from a newer Loom
// must fall through to the deadlock rendering rather than match nothing.
const RED_BASELINE = "baseline-fault";

// refreshRunDeadlock reads §12.1's wait-for cycle on the POLL.
//
// WHICH red a red run is comes free with the snapshot now (applySnapshot reads
// RunHeadDTO.redKind), so this call is only ever about the cycle itself — the
// thing a human needs in order to re-plan, and the thing that used to be
// reachable only through TickDelegationRun, i.e. only by advancing the run in
// order to draw it.
//
// It runs only while the run is red, and at most once per (run, status,
// updatedAt): the detector is pure over persisted rows, so re-asking without a
// row having moved cannot produce a different answer.
async function refreshRunDeadlock() {
  const run = orch.run;
  if (!run || !RED_RUN_STATUSES.has(run.status)) {
    if (orch.deadlock) {
      // The run recovered — or the switcher moved. Clearing is not cosmetic: a
      // stale cycle would keep the loudest panel on the page on screen for a
      // run that is now green.
      orch.deadlock = null; orch.redSig = "";
      renderOrchestration();
    }
    return;
  }
  const call = bound("RunDeadlock");
  if (!call) return;
  const sig = `${run.id}:${run.status}:${run.updatedAt || 0}`;
  if (sig === orch.redSig || orch.redFetch) return;
  orch.redFetch = true;
  try {
    const d = await call(run.id);
    if (!d || d.hidden || !orch.run || orch.run.id !== run.id) return;
    // A null deadlock with no error means the detector says the run is moving,
    // and the panel falls back to the derived picture below. An error is NOT
    // treated as "no deadlock": a run we could not diagnose must not render as
    // a healthy one.
    orch.deadlock = d.deadlock || null;
    orch.redSig = d.error ? "" : sig;
    renderOrchestration();
  } catch (e) {
    console.error("run deadlock", e);
  } finally {
    orch.redFetch = false;
  }
}

// §11's declarations are read one per parked child, and never more than this
// many. The cap is stated on screen rather than silently applied — a park list
// that quietly stops at twelve is a child parked forever with nobody told, in
// the one place §11.2 says that must not happen.
const PARK_CAP = 12;

function parkedIds() {
  return (orch.statuses || [])
    // The STATE and the declaration disagree in both directions and each
    // disagreement is a finding (ParkDTO.Parked's comment): `blocked` with no
    // file is a cleared block, a file on a running task is a park the detector
    // has not acted on yet. Both are worth fetching.
    .filter((s) => s.state === "blocked" || s.blocked)
    .map((s) => s.id)
    .sort();
}

// refreshParks reads each parked child's own declaration through TaskPark.
//
// Server-parsed, deliberately. The raw block.json is on the wire in other DTOs
// and parsing it here would be quicker — and would be a second implementation of
// delegate.ParseBlock, which decides what a valid declaration IS. The malformed
// case is the one that matters most (§11.2: a swallowed block is a child parked
// forever) and it is the case a hand-rolled JSON.parse gets wrong most quietly.
async function refreshParks() {
  const ids = parkedIds();
  const sig = ids.join(",");
  if (sig === orch.parkSig || orch.parkFetch) return;
  const runID = orch.run ? orch.run.id : 0;
  const call = bound("TaskPark");
  // An unbound TaskPark no longer needs saying on the chip: the park EDGES come
  // off the snapshot now, so what is lost without this call is the declarations
  // block, which renders its own absence.
  if (!call || !runID) return;
  if (!ids.length) {
    orch.parks = new Map(); orch.parkSig = sig; orch.parkOver = 0;
    renderOrchestration();
    return;
  }
  orch.parkFetch = true;
  const wanted = ids.slice(0, PARK_CAP);
  const out = new Map();
  try {
    for (const id of wanted) {
      try {
        const p = await call(runID, id);
        // A hidden payload is dropped rather than rendered as an absent park:
        // §3.1's marker carries nothing, and there is nothing to show.
        if (p && !p.hidden && (p.hasBlock || p.parked)) out.set(id, p);
      } catch (e) { console.error("task park", id, e); }
    }
  } finally {
    orch.parkFetch = false;
  }
  if (!orch.run || orch.run.id !== runID) return; // the switcher moved under us
  orch.parks = out;
  orch.parkSig = sig;
  orch.parkOver = Math.max(0, ids.length - wanted.length);
  renderOrchestration();
}

// §11.3's two decisions. Both are CAS-guarded server-side, so a second press —
// or the other Loom instance — is REFUSED and says so; nothing here tries to
// predict which way that goes.
function wireAmendmentButtons(host, runID) {
  for (const b of host.querySelectorAll("[data-amend]")) {
    b.addEventListener("click", async () => {
      const seq = parseInt(b.getAttribute("data-amend"), 10);
      const call = bound(b.getAttribute("data-act") === "reject"
        ? "RejectDelegationAmendment" : "ApproveDelegationAmendment");
      if (!call || !seq) return;
      b.disabled = true;
      try {
        const res = await call(runID, seq);
        const note = res && (res.note || res.error || "");
        const line = document.createElement("div");
        line.className = (res && (res.approved || res.rejected)) ? "dgi-outcome" : "po-warn";
        line.textContent = note || (res && res.approved ? "approved" : "declined");
        b.parentElement.appendChild(line);
        // Both decisions return the run RE-TICKED, so the new edge's
        // consequences are on screen in the same gesture. It replaces the held
        // report rather than only repainting: the amendment block re-renders on
        // the next poll from orch.tick, and a stale report there would put the
        // offer straight back with an Approve button that now refuses.
        if (res && res.tick && !res.tick.hidden) {
          orch.tick = res.tick;
          orch.tickAt = Math.floor(Date.now() / 1000);
          // Deferred one frame so the outcome line above is read before the
          // list it sits in is rebuilt.
          setTimeout(() => { orch.amendSig = ""; renderOrchestration(); }, 900);
        }
      } catch (e) {
        const line = document.createElement("div");
        line.className = "po-warn";
        line.textContent = String(e);
        b.parentElement.appendChild(line);
      }
      b.disabled = false;
      await refreshOrchestration();
    });
  }
}

// runIntegrationHtml renders §10.5, LIMITS INCLUDED and not as a footnote.
//
// The limits come from Go and are always non-empty, deliberately: no VCS
// operation can surface a cross-repo interface break, and a screen that showed
// green per-repo baselines and said nothing else IS the misreading this section
// exists to prevent.
function runIntegrationHtml(d) {
  if (!d) return "";
  if (d.hidden) return `<div class="po-warn">hidden</div>`;
  if (d.error) return `<div class="po-warn">${esc(d.error)}</div>`;
  const bits = [];
  for (const r of d.repos || []) {
    bits.push(`<div class="dgi-checkline ${r.red ? "bad" : "ok"}">${esc(r.repo)} · ` +
      esc(r.status || "no verdict recorded") + `</div>`);
    bits.push(fact("branch", r.branch));
    bits.push(fact("worktree", r.worktree));
    if (!r.hasCheck) {
      // A REAL degradation, named as one: the task's own check is then the
      // only evidence behind §5.2's precondition.
      bits.push(`<div class="po-warn">no integration.per_repo check for this repo — ` +
        `the task's own check is the only evidence there is</div>`);
    } else {
      bits.push(fact("gate", (r.checkArgv || []).join(" ")));
    }
    if (r.out) bits.push(`<pre class="dgi-pre dgi-out">${esc(r.out)}</pre>`);
  }
  if ((d.cross || []).length) {
    bits.push(`<div class="dgi-sec">Cross-repo checks</div>`);
    for (const c of d.cross) {
      bits.push(fact(c.id, (c.argv || []).join(" ")));
      if ((c.needsRepos || []).length) bits.push(fact("waits for", c.needsRepos.join(", ")));
    }
  }
  if ((d.drifts || []).length) {
    bits.push(`<div class="dgi-sec">Stale contracts — an interface moved after a consumer was built against it</div>`);
    for (const dr of d.drifts) {
      bits.push(`<div class="po-warn">${esc(dr.taskId)} needs ${esc(dr.artifact)} ` +
        `(from ${esc(dr.producer)}): ${esc(dr.wasCommit)} → ${esc(dr.nowCommit)}</div>`);
    }
  }
  bits.push(`<div class="dgi-sec">What this does and does not prove</div>`);
  for (const l of d.limits || []) bits.push(`<div class="dgi-outcome">${esc(l)}</div>`);
  return bits.join("");
}

// tickHtml renders one scheduler pass: what moved and what the watchdogs saw.
//
// §11.3's amendments and §12.1's deadlock are deliberately NOT here. They
// outlive the press — an offer is a standing question and a deadlock is a
// permanent state — and both are rendered by the seam's own blocks off the held
// report, so that the poll's next re-render cannot wipe them off the screen.
function tickHtml(rep) {
  if (!rep) return "";
  if (rep.hidden) return `<div class="po-warn">hidden</div>`;
  const bits = [];
  if (rep.error) bits.push(`<div class="po-warn">${esc(rep.error)}</div>`);
  const moved = [
    ["checked", rep.checked], ["integrated", rep.integrated],
    ["resumed", rep.resumed], ["ready", rep.ready], ["blocked", rep.blocked],
  ].filter(([, v]) => (v || []).length);
  if (moved.length) {
    bits.push(`<div class="dgi-sec">This pass</div>`);
    for (const [label, v] of moved) bits.push(fact(label, v.join(", ")));
  }
  for (const f of rep.findings || []) {
    // NOTHING here killed anything: §12.2's actions are flag, offer-retry,
    // resolve and stop-spawns, and a `stalled` child is a label on a child that
    // is still running.
    bits.push(`<div class="po-warn">${esc(f.taskId)}: ${esc(f.kind)} — ${esc(f.detail || f.action || "")}</div>`);
  }
  for (const s of rep.suppressed || []) {
    // §12.2's `run-budget` and §10.2's stopped spawning arrive here. An action
    // Loom declined to take is not a no-op the human can be left to infer.
    bits.push(`<div class="dgi-outcome">${esc(s.taskId)}: ${esc(s.action)} not offered — ${esc(s.reason)}</div>`);
  }
  for (const e of rep.errs || []) {
    bits.push(`<div class="po-warn">${esc(e.taskId)} failed at ${esc(e.stage)}: ${esc(e.error)}</div>`);
  }
  if (rep.verdict) bits.push(`<div class="dgi-outcome">${esc(rep.verdict)}</div>`);
  if (!bits.length) bits.push(`<div class="po-empty">nothing moved this pass</div>`);
  return bits.join("");
}

// ---- §11.3: the amendment log, rendered where it can be answered ------------
//
// An amendment is the ONLY path from "a child found a dependency the plan did
// not have" to an edge, and Loom never applies one itself. So the three lists
// stay separate: a decided amendment is not a standing offer, and a declined one
// that disappeared would be re-decided by the next human to read the run.
function renderAmendments(host) {
  if (!host) return;
  const rep = orch.tick;
  const runID = orch.run ? orch.run.id : 0;
  if (!rep || !runID || rep.hidden) {
    // Said out loud rather than left blank. The log is only readable through a
    // tick, the tick is a write, and a human who does not know that reads an
    // empty amendment block as "no child has asked for anything".
    const html = runID
      ? `<div class="dgi-sec">Amendments</div><div class="po-empty">not read yet — ` +
        `the amendment log travels with a scheduler pass. Press “Advance run”.</div>`
      : "";
    if (html !== orch.amendSig) { orch.amendSig = html; host.innerHTML = html; }
    return;
  }
  const html =
    amendmentListHtml("Offers — a child hit a dependency the plan did not have", rep.offers, true) +
    amendmentListHtml("Granted", rep.granted, false) +
    amendmentListHtml("Declined", rep.declined, false) +
    (((rep.offers || []).length + (rep.granted || []).length + (rep.declined || []).length)
      ? `<div class="dg-asof">as of the pass ${esc(elapsedSince(orch.tickAt))} ago</div>`
      : `<div class="dgi-sec">Amendments</div><div class="po-empty">no child has raised one</div>`);
  if (html === orch.amendSig) return;
  orch.amendSig = html;
  host.innerHTML = html;
  wireAmendmentButtons(host, runID);
}

function amendmentListHtml(title, list, decidable) {
  if (!(list || []).length) return "";
  const bits = [`<div class="dgi-sec">${esc(title)}</div>`];
  for (const a of list) {
    // A SCOPE amendment is not one offer among several and is not rendered as
    // one. Approving it rewrites the child's brief and re-seeds it — it widens
    // an authorization a human wrote deliberately, and §11.3 is explicit that it
    // is never auto-granted. The other two kinds add an edge or ask a question;
    // this one hands out permission, so it carries its own frame, its own ink
    // and the sentence saying what the press does.
    const scope = a.kind === "scope";
    const what = a.replan
      // §11.3's second branch: nobody produces the artifact, so there is no
      // edge to add and nothing to approve. It is a conversation with the
      // plan's author, and approving it would be approving a phantom.
      ? `re-plan: <b>${esc(a.task)}</b> needs <code>${esc(a.artifact)}</code>, which no task produces`
      : scope
        ? `widen <b>${esc(a.task)}</b>'s authorization to also write ` +
          `<code>${esc((a.paths || []).join("  "))}</code>`
        : `<b>${esc(a.task)}</b> needs <code>${esc(a.artifact)}</code>` +
          (a.producer ? ` from <b>${esc(a.producer)}</b>` : "");
    bits.push(
      `<div class="dgi-amend${scope ? " dgi-amend-scope" : ""}${a.replan ? " dgi-amend-replan" : ""}">` +
      (scope ? `<div class="dg-scopemark">authorization change</div>` : "") +
      `<div>${what}</div>` +
      (scope
        ? `<div class="po-warn">approving REWRITES this child's brief and re-seeds it. ` +
          `It is the one amendment that hands out permission, and Loom never grants it on its own.</div>`
        : "") +
      (a.replan
        ? `<div class="dgi-outcome">nothing to approve: add the artifact to a producing task, ` +
          `then re-run the pass. Loom does not invent tasks.</div>`
        : "") +
      (a.reason ? `<div class="dgi-outcome">${esc(a.reason)}</div>` : "") +
      (decidable && !a.replan
        ? `<div class="modal-actions">` +
          `<button class="tact${scope ? " dg-scopebtn" : ""}" data-amend="${a.seq}" data-act="approve">` +
          `${scope ? "Widen the authorization" : "Approve"}</button>` +
          `<button class="tact" data-amend="${a.seq}" data-act="reject">Decline</button></div>`
        : "") +
      (a.approved ? `<div class="dgi-outcome">granted ${esc(elapsedSince(a.approvedAt))} ago</div>` : "") +
      (a.rejected ? `<div class="dgi-outcome">declined ${esc(elapsedSince(a.rejectedAt))} ago — ` +
        `the row stays in the log so the same offer is not re-proposed</div>` : "") +
      `</div>`);
  }
  return bits.join("");
}

// refreshOrchestration is the §7 poll call. One in flight at a time: the 1.5s
// tick is faster than a slow snapshot on a large run, and two overlapping calls
// would race each other's rev into the client.
async function refreshOrchestration() {
  if (stageView.kind !== "project" || !orch.root) return;
  const snapshot = bound("OrchestrationSnapshot");
  if (!snapshot) {
    // §5.4: an unbound method degrades blocks 1-3 to ABSENT and leaves block 4
    // working. Stage 4a depends on nothing from slice 3, and this is the line
    // that keeps that true.
    orch.unbound = true; orch.loaded = true;
    renderOrchestration();
    return;
  }
  if (orch.inFlight) return;
  orch.inFlight = true;
  const root = orch.root;
  let snap = null, err = "";
  try { snap = await snapshot(root, orch.runID, orch.rev); }
  catch (e) { err = String(e); }
  finally { orch.inFlight = false; }
  if (stageView.kind !== "project" || orch.root !== root) return; // the user moved on
  if (!snap) {
    orch.loaded = true;
    orch.error = err || "orchestration snapshot returned nothing";
    renderOrchestration();
    return;
  }
  applySnapshot(snap);
  // Both are no-ops until their own trigger fires — a red run status, or a
  // change in the set of parked children — so the common case is two map
  // comparisons and no call at all. Fire-and-forget for the same reason the
  // document refresh is: a failure here costs the seam a block, never the poll.
  refreshRunDeadlock().catch((e) => console.error("run deadlock", e));
  refreshParks().catch((e) => console.error("parks", e));
}

function applySnapshot(snap) {
  orch.loaded = true;
  orch.unbound = false;
  if (snap.hidden) {
    // §3.1.3: ONE constant line, whose wording does not vary with whether a
    // run, a manifest or a document exists. Everything cached is dropped so a
    // stale graph cannot outlive the hide by even one frame.
    orch.hidden = true;
    orch.runs = []; orch.run = null; orch.nodes = []; orch.edges = []; orch.layout = null;
    orch.statuses = []; orch.strip = null; orch.blocked = [];
    orch.warnings = []; orch.error = ""; orch.rev = 0; orch.graph = null;
    orch.docs = null; orch.docSel = null;
    renderOrchestration();
    return;
  }
  orch.hidden = false;
  orch.runs = snap.runs || [];
  orch.run = snap.run || null;
  // §10.2 vs §12.1, off the payload. Assigned unconditionally — including the
  // empty case — so a run that RECOVERS drops its baseline fault on the same
  // frame the status changes, rather than one poll later when a separate call
  // gets round to noticing.
  orch.redKind = (orch.run && orch.run.redKind) || "";
  orch.baselineFaults = (orch.run && orch.run.baselineFaults) || [];
  orch.statuses = snap.statuses || [];
  orch.strip = snap.strip || null;
  orch.blocked = snap.blocked || [];
  orch.warnings = snap.warnings || [];
  orch.error = snap.error || "";

  const wasRev = orch.rev;
  const prevIds = orch.nodes.map((n) => n.id).join(",");
  let relayout = false;
  if (!snap.unchanged) {
    orch.nodes = snap.nodes || [];
    orch.edges = snap.edges || [];
    // The coordinates and the stage size travel with the topology half and are
    // omitted from an `unchanged` reply for the same reason the nodes are: the
    // client already holds them, and re-applying them is the re-layout §7.3
    // forbids.
    orch.layout = snap.layout || null;
    relayout = true;
  } else if (!orch.nodes.length && snap.rev) {
    // The server withheld the layout half because our rev matched, but we hold
    // no nodes — the only way that happens is a client that dropped its cache
    // (a re-render, a run switch racing a tick). Zero the rev so the NEXT poll
    // is forced to send the full payload, rather than sitting on a permanently
    // empty graph that every subsequent tick agrees is up to date.
    orch.rev = 0;
    renderOrchestration();
    return;
  }
  orch.rev = snap.rev || 0;
  if (relayout && wasRev && prevIds !== orch.nodes.map((n) => n.id).join(",")) {
    // §7.3: the picture is allowed to change under a live orchestrator, but it
    // must SAY SO rather than silently reshuffling under the eye.
    orch.changed = true;
    // A manifest rewrite is also when documents[] moves, and it is the only
    // cheap signal this view gets that a document set may have changed.
    refreshDocuments().catch((e) => console.error("documents", e));
  }
  renderOrchestration(relayout);
}

// renderOrchestration is the whole seam. `relayout` is the rev-change path;
// without it only the moving parts are touched, and each part compares its own
// rendered signature first so an unchanged strip is not rebuilt under a hovering
// cursor.
function renderOrchestration(relayout) {
  const seam = document.getElementById("po-arch");
  if (!seam || !seam.isConnected) return;
  const hiddenLine = document.getElementById("dg-hidden");
  const escHost = document.getElementById("dg-escalate");
  const stripHost = document.getElementById("dg-strip");
  const blockedHost = document.getElementById("dg-blocked");
  const parkedHost = document.getElementById("dg-parked");
  const graphBlock = document.getElementById("dg-graphblock");
  const runBlock = document.getElementById("dg-runblock");
  const amendHost = document.getElementById("dg-amend");
  const docsHost = document.getElementById("po-docs");
  if (!hiddenLine || !stripHost || !blockedHost || !graphBlock || !docsHost) return;

  if (orch.hidden) {
    hiddenLine.textContent = "hidden — orchestration view suppressed; unhide to view";
    if (escHost) escHost.replaceChildren();
    stripHost.replaceChildren();
    blockedHost.replaceChildren();
    if (parkedHost) parkedHost.replaceChildren();
    graphBlock.hidden = true;
    if (runBlock) {
      runBlock.hidden = true;
      runBlock.querySelector("#dg-runbody").replaceChildren();
      if (amendHost) amendHost.replaceChildren();
    }
    docsHost.replaceChildren();
    orch.escSig = orch.parkedSig = orch.amendSig = "";
    return;
  }
  hiddenLine.textContent = "";
  // The run actions exist only when there is a run to act on. Absence of
  // orchestration is not an error (§3), so the block goes away rather than
  // rendering two buttons that would refuse.
  if (runBlock) runBlock.hidden = !orch.run;

  // Escalation FIRST, above the figures. §12.1: the run row goes red and
  // permanent, and the entire hazard is that an all-blocked run reads exactly
  // like a run that is working.
  renderEscalation(escHost);
  renderRunStrip(stripHost);
  renderBlockedStrip(blockedHost);
  renderParkedBlock(parkedHost);
  renderAmendments(amendHost);
  renderGraphBlock(graphBlock, relayout);
  // Last, and outside renderGraphBlock's several early returns: an open
  // inspector must keep up with the poll even when the graph itself is a table
  // or is hidden behind a fatal manifest error.
  if (orch.inspectorUpdate) orch.inspectorUpdate();
}

// ---- block 1: the run strip -------------------------------------------------

function elapsedSince(ts) {
  if (!ts) return "";
  const s = Math.max(0, Math.floor(Date.now() / 1000) - ts);
  if (s < 90) return s + "s";
  if (s < 5400) return Math.round(s / 60) + "m";
  if (s < 172800) return Math.round(s / 3600) + "h";
  return Math.round(s / 86400) + "d";
}

function renderRunStrip(host) {
  if (!orch.run) { host.replaceChildren(); orch.stripSig = ""; return; }
  const s = orch.strip || {};
  const r = orch.run;
  const bneckTitle = s.bottleneck ? ((nodeById(s.bottleneck) || {}).title || s.bottleneck) : "";
  const density = s.nodes ? (s.edges / s.nodes).toFixed(2) : "0";
  const figs = [
    fig(`${s.merged || 0}/${s.nodes || 0}`, "merged"),
    fig(String(s.verified || 0), "verified"),
    // Only when there is one. It is the figure the human can act on directly —
    // §5.2's gate is waiting on a press — so a permanent "0 mergeable" beside
    // the others would train the eye to skip the row that matters.
    s.mergeable ? fig(String(s.mergeable), "mergeable", "dg-f-act") : "",
    fig(String(s.ready || 0), "ready"),
    fig(String(s.running || 0), "running"),
    s.failed ? fig(String(s.failed), "failed", "dg-f-bad") : "",
    `<span class="dg-fig dg-chain-fig" id="dg-chainfig" title="Hover to trace it on the graph"><b>${s.longestChain || 0}</b><i>longest remaining chain, in nodes</i></span>`,
    // §2.7's cohesion read, labelled as the INDICATOR it is. Edge density and
    // repo-sharing correlate with inter-task cohesion; they do not measure it,
    // and a UI that called this a measurement would be the confident wrong
    // number §5.2 bans elsewhere.
    `<span class="dg-fig" title="cohesion indicator, not a measurement"><b>${s.repos || 0}</b><i>repos · ${s.sharedRepos || 0} shared · density ${density}</i></span>`,
  ].filter(Boolean).join("");
  // §2.6's run-health number. The caveat it used to carry — "live count only" —
  // is gone because the count is no longer live-only: the payload's park edges
  // come from delegate.Effective, which includes every ACCEPTED amendment, so a
  // park that was granted and worked through is still counted. What remains
  // uncounted is a park that was resumed without an amendment ever being
  // recorded, and the chip no longer claims otherwise in either direction.
  const parkN = parkEdgeCount();
  const parkedN = orch.parks.size;
  const chips = [
    `<span class="dg-chip${parkN ? " dg-chip-warn" : ""}" title="A park edge is a dependency the plan did not have: an accepted amendment, or a child parked on something the plan missed.">park edges: ${parkN}${parkedN ? ` · ${parkedN} parked now` : ""}</span>`,
    s.hiddenNodes ? `<span class="dg-chip dg-chip-mute">${s.hiddenNodes} node${s.hiddenNodes === 1 ? "" : "s"} hidden</span>` : "",
    // Said in the figures too, not only in the escalation band above: the two
    // are read at different distances, and the band is the one a user learns to
    // scroll past once they have read it.
    RED_RUN_STATUSES.has(r.status) ? `<span class="dg-chip dg-chip-warn">spawning stopped</span>` : "",
    orch.changed ? `<button class="dg-chip dg-chip-warn" id="dg-changed">graph changed — dismiss</button>` : "",
  ].filter(Boolean).join("");
  const switcher = orch.runs.length > 1
    ? `<select class="dg-runsel" id="dg-runsel">${orch.runs.map((x) =>
        `<option value="${x.id}"${x.id === r.id ? " selected" : ""}>${esc(x.name || x.slug || ("run " + x.id))} · ${esc(x.status)}</option>`).join("")}</select>`
    : "";

  const html = `
    <section class="po-block dg-strip">
      <div class="po-bhead">
        <h3>Run</h3>
        <span class="dg-runname">${esc(r.name || r.slug || ("run " + r.id))}</span>
        <span class="po-tag">${esc(r.status)}</span>
        ${r.createdAt ? `<span class="dg-elapsed">${esc(elapsedSince(r.createdAt))} old</span>` : ""}
        ${switcher}
      </div>
      <div class="dg-figs">${figs}</div>
      <div class="dg-chiprow">${bneckTitle ? `<span class="dg-chip dg-chip-accent">bottleneck: ${esc(bneckTitle)}</span>` : ""}${chips}</div>
      ${stagingRowHtml()}
    </section>`;
  if (html === orch.stripSig) return;
  orch.stripSig = html;
  host.innerHTML = html;

  const sel = document.getElementById("dg-runsel");
  if (sel) sel.addEventListener("change", () => {
    orch.runID = parseInt(sel.value, 10) || 0;
    // A different run is a different graph: drop the topology fingerprint so
    // the next snapshot is forced to send the full payload rather than
    // answering "unchanged" against the previous run's rev.
    orch.rev = 0; orch.nodes = []; orch.edges = []; orch.layout = null; orch.graph = null; orch.changed = false;
    // Everything §§10-12 holds is keyed to the run that was showing: another
    // run's parks, red kind and tick report name task ids this graph does not
    // have, and would paint that run's deadlock over this one.
    resetRunLocal();
    refreshOrchestration().catch((e) => console.error("orchestration", e));
  });
  const changed = document.getElementById("dg-changed");
  if (changed) changed.addEventListener("click", () => { orch.changed = false; renderOrchestration(); });
  const chainFig = document.getElementById("dg-chainfig");
  if (chainFig) {
    chainFig.addEventListener("mouseenter", () => { if (orch.graph) orch.graph.highlightChain(true); });
    chainFig.addEventListener("mouseleave", () => { if (orch.graph) orch.graph.highlightChain(false); });
  }
}

function fig(value, label, cls) {
  return `<span class="dg-fig${cls ? " " + cls : ""}"><b>${esc(value)}</b><i>${esc(label)}</i></span>`;
}

// stagingRowHtml is §10.1's staging area in one line per repo.
//
// Three values, not two, and the third is the load-bearing one: a recorded
// `pass`, a recorded failure, and NO VERDICT AT ALL — the worktree's position is
// recorded before any per-repo check has ever run there. Rendering that as green
// would certify on the absence of a result; as red it would blame every first
// integration. It reads "no verdict yet", which is what it is.
//
// A repo with no declared `integration.per_repo` gate is a REAL degradation and
// is marked as one on the chip: the task's own check is then the only evidence
// behind §5.2's approval, and that is a smaller claim than the gate implies.
function stagingRowHtml() {
  const d = orch.integration;
  if (!d || d.hidden || !(d.repos || []).length) return "";
  const chips = d.repos.map((r) => {
    const cls = r.red ? " dg-stage-bad" : (r.status === "pass" ? " dg-stage-ok" : " dg-stage-none");
    return `<span class="dg-stagechip${cls}" title="${esc(r.branch || "")}">` +
      `<code>${esc(r.repo)}</code> ${esc(r.status || "no verdict yet")}` +
      (r.hasCheck ? "" : ` <i>ungated</i>`) + `</span>`;
  }).join("");
  return `<div class="dg-stagerow"><span class="dg-stagelabel">staging</span>${chips}</div>`;
}

// ---- block 2: blocked on you -----------------------------------------------

const BLOCK_ACTION = {
  attach: "attach", diff: "view diff", check: "run check", merge: "merge command", seed: "attach",
};

function renderBlockedStrip(host) {
  const rows = orch.blocked || [];
  if (!rows.length) { host.replaceChildren(); orch.blockedSig = ""; return; }
  const html = `
    <section class="po-block dg-blockblock">
      <div class="po-bhead"><h3>Blocked on you</h3><span class="dg-count">${rows.length}</span></div>
      <ul class="po-list">${rows.map((b) => `
        <li class="po-sess dg-brow" data-id="${esc(b.id)}" data-action="${esc(b.action)}">
          <span class="dg-bdot"></span>
          <span class="po-sname">${esc(b.title || b.id)}</span>
          <span class="dg-reason">${esc(b.reason)}</span>
          <span class="dg-since">${esc(elapsedSince(b.since))}</span>
          <button class="tact dg-baction">${esc(BLOCK_ACTION[b.action] || "open")}</button>
        </li>`).join("")}</ul>
    </section>`;
  if (html === orch.blockedSig) return;
  orch.blockedSig = html;
  host.innerHTML = html;
  host.querySelectorAll(".dg-brow").forEach((li) => {
    const id = li.getAttribute("data-id");
    const action = li.getAttribute("data-action");
    li.addEventListener("click", () => openNodeInspector(id));
    const btn = li.querySelector(".dg-baction");
    btn.addEventListener("click", (e) => { e.stopPropagation(); runNodeAction(id, action); });
  });
}

// runNodeAction is the verb on a blocked-on-you row. §5.5: a list of problems
// with no verb is a list the user reads and then leaves.
function runNodeAction(id, action) {
  const st = statusMap().get(id) || {};
  switch (action) {
    case "attach":
    case "seed":
      if (st.sessionName) { selectSession(st.sessionName); return; }
      openNodeInspector(id);
      return;
    case "diff":
      if (st.sessionName) { openDiff(st.sessionName); return; }
      openNodeInspector(id);
      return;
    default:
      // check and merge both want the evidence beside the button, and the
      // inspector is where the evidence already is.
      openNodeInspector(id);
  }
}

// ---- §12.1 / §10.2: the escalation band ------------------------------------
//
// One band, two completely different faults, and the difference is the whole
// reason this is not a status chip:
//
//   §10.2 BASELINE fault — the repo's own integration tree is red. NO TASK IS
//     BLAMED, spawning stops, and the repair is to the repo. Blaming a child
//     for this is precisely the failure the attribution table exists to prevent,
//     and it is an easy one to render by accident because both readings share
//     the run status `deadlocked`.
//   §12.1 DEADLOCK — no task ready, none running, work outstanding. Rendered as
//     the actual wait-for cycle, naming every task and artifact in it, or as the
//     list of decisions owed.
//
// Loud, and permanent until the run moves. A deadlocked run and a run quietly
// making progress produce identical graphs, identical dots and identical
// figures; if this band is subtle then it has failed at the one job it has.

function nodeTitle(id) {
  const n = nodeById(id);
  return (n && !n.hidden && n.title) ? n.title : id;
}

// PARK_LABEL is §11.1's four kinds in the words that say what unblocks each.
// The kind is what decides the remedy — that is why block.json carries it — so
// the label carries the remedy rather than repeating the enum.
const PARK_LABEL = {
  "needs-artifact": "waiting on a peer to publish something",
  "needs-decision": "waiting on a human decision",
  "needs-scope": "the task's boundary was drawn wrong",
  "blocked-external": "blocked on something outside the run",
};

// The kinds that no tick can clear. §11.4's resume only re-attempts a delivery
// Loom already owes; these three need a person.
const HUMAN_PARK_KINDS = new Set(["needs-decision", "needs-scope", "blocked-external"]);

function renderEscalation(host) {
  if (!host) return;
  const run = orch.run;
  // The POLL's finding first, the last human-pressed pass second. Both are
  // delegate.DetectDeadlock over the same persisted rows; the tick's is used
  // only when the read-only binding is absent (an older build) or has not
  // answered yet, because a tick's report can be minutes old and the poll's is
  // at most one interval behind.
  const dl = orch.deadlock || (orch.tick && orch.tick.deadlock) || null;
  const red = !!run && RED_RUN_STATUSES.has(run.status);
  const baselineFault = orch.redKind === RED_BASELINE || !!orch.baselineAlert;
  if (!red && !dl && !orch.baselineAlert) {
    if (orch.escSig !== "") { orch.escSig = ""; host.replaceChildren(); }
    return;
  }
  const bits = [];
  if (baselineFault) bits.push(baselinePanelHtml(run));
  // The deadlock panel is suppressed for a pure baseline fault: the run is red
  // for a reason that names no task, and rendering an EMPTY wait-for cycle
  // beside it is the specific misread RedKind exists to prevent.
  if (dl || (red && !baselineFault)) bits.push(deadlockPanelHtml(dl));
  const html = bits.join("");
  if (html === orch.escSig) return;
  orch.escSig = html;
  host.innerHTML = html;

  const diag = host.querySelector("#dg-diagnose");
  if (diag) diag.addEventListener("click", async () => {
    const call = bound("TickDelegationRun");
    const runID = orch.run ? orch.run.id : 0;
    if (!call || !runID) return;
    diag.disabled = true;
    diag.textContent = "Reading the scheduler…";
    try {
      const rep = await call(runID);
      if (rep && !rep.hidden) { orch.tick = rep; orch.tickAt = Math.floor(Date.now() / 1000); }
      // Both signatures, because a pass can answer both questions at once.
      orch.escSig = orch.amendSig = "";
    } catch (e) { console.error("diagnose", e); }
    diag.disabled = false;
    diag.textContent = "Diagnose (runs a scheduler pass)";
    await refreshOrchestration();
    renderOrchestration();
  });
  host.querySelectorAll("[data-task]").forEach((el) =>
    el.addEventListener("click", () => openNodeInspector(el.getAttribute("data-task"))));
}

function baselinePanelHtml(run) {
  const bits = [`
    <section class="dg-escalate dg-escalate-baseline">
      <div class="dg-esc-head"><span class="dg-esc-mark">baseline fault</span>
        <h3>The repo's own tree is red — no task is to blame</h3></div>
      <div class="dg-esc-body">This is §10.2's second attribution: the integration check was red
        <b>with</b> the task merged and red <b>without</b> it. Loom has stopped offering spawns.
        The repair is to the repository, not to the plan, and nothing here is evidence that any
        child did anything wrong.</div>`];
  for (const f of orch.baselineFaults || []) {
    bits.push(`<div class="dg-esc-row"><code>${esc(f.repo)}</code> ` +
      `<span class="dg-esc-verdict">${esc(f.status || "no verdict recorded")}</span>` +
      (f.head ? ` <span class="dg-esc-dim">at ${esc(f.head.slice(0, 12))}</span>` : "") +
      (f.at ? ` <span class="dg-esc-dim">${esc(elapsedSince(f.at))} ago</span>` : "") + `</div>` +
      (f.reason ? `<pre class="dgi-pre dgi-out">${esc(f.reason)}</pre>` : ""));
  }
  const a = orch.baselineAlert;
  if (a) {
    bits.push(`<div class="dg-esc-row">seen this session while integrating ` +
      `<b data-task="${esc(a.taskId)}" class="dg-esc-task">${esc(nodeTitle(a.taskId))}</b>` +
      (a.repo ? ` in <code>${esc(a.repo)}</code>` : "") + `</div>`);
    if (a.note) bits.push(`<div class="dg-esc-body">${esc(a.note)}</div>`);
  }
  if (!(orch.baselineFaults || []).length && !a) {
    bits.push(`<div class="dg-esc-body">The run row says <code>${esc(run ? run.status : "")}</code> and its
      integration column carries the fault; the captured reason has not been read back yet.</div>`);
  }
  bits.push(`</section>`);
  return bits.join("");
}

function deadlockPanelHtml(dl) {
  const shape = dl ? dl.shape : "";
  const head = shape === "external"
    ? "Every remaining task is waiting on you"
    : shape === "starved"
      ? "The run is stuck in a state Loom cannot classify"
      : "Deadlocked — the tasks are waiting on each other";
  const bits = [`
    <section class="dg-escalate dg-escalate-deadlock">
      <div class="dg-esc-head"><span class="dg-esc-mark">deadlocked</span><h3>${esc(head)}</h3></div>
      <div class="dg-esc-body">Nothing is ready, nothing is running, and work remains. A run in this
        state looks exactly like a run that is progressing — which is why it is said here rather
        than left to a status chip.</div>`];

  if (shape === "starved") {
    // Not a third kind of deadlock: a task row is in a state no bucket claims,
    // which is a fault in Loom. Telling the human to re-plan a manifest that is
    // fine would be worse than admitting the tool is confused.
    bits.push(`<div class="dg-esc-body">This one is a fault in <b>Loom</b>, not in the plan: some
      task is in a state no scheduler bucket claims. Re-planning the manifest will not fix it.</div>`);
  }

  const cycle = dl ? (dl.cycle || []) : [];
  const owed = dl ? (dl.owed || []) : [];
  if (cycle.length) {
    bits.push(`<div class="dg-esc-sec">The wait-for cycle, in full</div>`);
    for (const e of cycle) {
      bits.push(`<div class="dg-esc-edge">` +
        `<b class="dg-esc-task" data-task="${esc(e.from)}">${esc(nodeTitle(e.from))}</b>` +
        `<span class="dg-esc-arrow">is waited on by</span>` +
        `<b class="dg-esc-task" data-task="${esc(e.to)}">${esc(nodeTitle(e.to))}</b>` +
        (e.artifact ? `<span class="dg-esc-art">for <code>${esc(e.artifact)}</code></span>`
          // Named as unnamed rather than omitted: an edge with no artifact came
          // from a block that did not say what it wanted, and that is itself
          // the thing to go and read.
          : `<span class="dg-esc-dim">no artifact named</span>`) +
        `</div>`);
    }
    bits.push(`<div class="dg-esc-body">Always fatal to the plan as written: a human re-plan is the
      only exit. Loom will not break the cycle by choosing a task to abandon.</div>`);
  }
  if (owed.length) {
    bits.push(`<div class="dg-esc-sec">Decisions owed, oldest first</div>`);
    for (const o of owed) {
      bits.push(`<div class="dg-esc-edge">` +
        `<b class="dg-esc-task" data-task="${esc(o.taskId)}">${esc(nodeTitle(o.taskId))}</b>` +
        `<span class="dg-esc-kind">${esc(o.kind)}</span>` +
        `<span>${esc(o.summary || PARK_LABEL[o.kind] || "")}</span>` +
        (o.since ? `<span class="dg-esc-dim">${esc(elapsedSince(o.since))} waiting</span>` : "") +
        `</div>`);
    }
  }
  if (dl && !cycle.length && !owed.length && (dl.stuck || []).length) {
    bits.push(`<div class="dg-esc-sec">Non-terminal tasks</div>`);
    bits.push(`<div class="dg-esc-row">${dl.stuck.map((id) =>
      `<b class="dg-esc-task" data-task="${esc(id)}">${esc(nodeTitle(id))}</b>`).join(" · ")}</div>`);
  }

  if (!dl) {
    // The run row says deadlocked and no pass has been read this session. What
    // CAN be shown is shown — the server already marks back edges on the graph
    // payload, and the parked children's own declarations are on hand — and it
    // is labelled as the weaker evidence it is rather than presented as the
    // scheduler's finding.
    bits.push(derivedDeadlockHtml());
  }
  bits.push(`<div class="modal-actions"><button class="tact" id="dg-diagnose">` +
    `Diagnose (runs a scheduler pass)</button></div>`);
  bits.push(`<div class="dg-esc-dim">A pass runs checks, integrations and owed seed deliveries. ` +
    `It is a write, which is why it is a press and not something the poll does.</div>`);
  bits.push(`</section>`);
  return bits.join("");
}

// derivedDeadlockHtml is the second-best picture, and it says so.
function derivedDeadlockHtml() {
  const back = (orch.edges || []).filter((e) => e.cycle);
  const human = [...orch.parks.entries()].filter(([, p]) => HUMAN_PARK_KINDS.has(p.kind));
  const bits = [`<div class="dg-esc-sec">What this screen can see without a pass</div>`];
  if (back.length) {
    for (const e of back) {
      bits.push(`<div class="dg-esc-edge"><b class="dg-esc-task" data-task="${esc(e.from)}">${esc(nodeTitle(e.from))}</b>` +
        `<span class="dg-esc-arrow">is waited on by</span>` +
        `<b class="dg-esc-task" data-task="${esc(e.to)}">${esc(nodeTitle(e.to))}</b>` +
        (e.artifact ? `<span class="dg-esc-art">for <code>${esc(e.artifact)}</code></span>` : "") +
        `<span class="dg-esc-dim">back edge on the drawn graph</span></div>`);
    }
  }
  for (const [id, p] of human) {
    bits.push(`<div class="dg-esc-edge"><b class="dg-esc-task" data-task="${esc(id)}">${esc(nodeTitle(id))}</b>` +
      `<span class="dg-esc-kind">${esc(p.kind)}</span>` +
      `<span>${esc(p.summary || PARK_LABEL[p.kind] || "")}</span></div>`);
  }
  if (!back.length && !human.length) {
    bits.push(`<div class="dg-esc-body">Nothing on the drawn graph and no parked child explains it.
      The scheduler's own finding is one press away.</div>`);
  } else {
    bits.push(`<div class="dg-esc-dim">Read off the drawn graph and the parked children's own
      declarations — not the scheduler's wait-for computation. Diagnose for that.</div>`);
  }
  return bits.join("");
}

// ---- §11: the park layer ----------------------------------------------------
//
// A parked child is the fallback the design is measured on (§2's M3), so a park
// is never a badge on its own. Each one renders WHAT THE CHILD WROTE — its own
// words, unedited — and the one action that can end it.

function renderParkedBlock(host) {
  if (!host) return;
  const parks = [...orch.parks.entries()];
  if (!parks.length) {
    if (orch.parkedSig !== "") { orch.parkedSig = ""; host.replaceChildren(); }
    return;
  }
  const runID = orch.run ? orch.run.id : 0;
  const rows = parks.map(([id, p]) => {
    if (p.malformed) {
      // §11.2's worst outcome is a SWALLOWED block: a child parked forever with
      // nobody told. So an unparseable declaration is the loudest row here, and
      // the raw bytes are shown because they are all there is.
      return `<li class="dg-park dg-park-bad" data-id="${esc(id)}">
        <div class="dg-parkhead"><b class="dg-esc-task" data-task="${esc(id)}">${esc(nodeTitle(id))}</b>
          <span class="dg-parkkind dg-parkkind-bad">declaration will not parse</span></div>
        <div class="po-warn">${esc(p.parseError || "")} — Loom will not guess what the child meant.
          Fix or remove block.json in the task's .meta/ directory.</div>
        <pre class="dgi-pre">${esc(p.raw || "")}</pre></li>`;
    }
    const scope = p.kind === "needs-scope";
    return `<li class="dg-park${scope ? " dg-park-scope" : ""}" data-id="${esc(id)}">
      <div class="dg-parkhead">
        <b class="dg-esc-task" data-task="${esc(id)}">${esc(nodeTitle(id))}</b>
        <span class="dg-parkkind${scope ? " dg-parkkind-scope" : ""}">${esc(p.kind || "block")}</span>
        <span class="dg-esc-dim">${esc(PARK_LABEL[p.kind] || "")}</span>
        ${p.at ? `<span class="dg-since">${esc(elapsedSince(p.at))} parked</span>` : ""}
      </div>
      ${p.summary ? `<div class="dg-parksum">${esc(p.summary)}</div>` : ""}
      ${p.artifact ? fact("needs artifact", p.artifact + (p.from ? ` (from ${p.from})` : "")) : ""}
      ${(p.paths || []).length ? fact("wants to write", p.paths.join("  ")) : ""}
      ${p.resumeWhen ? fact("resume when", p.resumeWhen) : ""}
      ${p.pendingSeed
        // §11.4's durable debt, shown as the TEXT Loom owes rather than as the
        // word "pending": the workflow view already refuses to render a pending
        // seed with nothing behind it.
        ? `<div class="dgi-sec">Loom owes this child</div><pre class="dgi-pre">${esc(p.pendingSeed)}</pre>`
        : ""}
      <div class="dg-parkacts">
        <button class="tact dg-parkinspect">Open the declaration</button>
        ${p.resumable
          ? `<button class="tact dg-parkresume">Retry the delivery</button>`
          // The greyed button carries its own sentence. A disabled control with
          // no reason beside it is how a human concludes the park is permanent.
          : `<span class="dg-esc-dim">${esc(p.resumeNote || "")}</span>`}
      </div></li>`;
  }).join("");
  const over = orch.parkOver
    ? `<div class="po-warn">${orch.parkOver} more parked child${orch.parkOver === 1 ? "" : "ren"} ` +
      `not read on this pass — open them from the graph</div>`
    : "";
  const html = `
    <section class="po-block dg-parkblock">
      <div class="po-bhead"><h3>Parked children</h3><span class="dg-count">${parks.length}</span>
        <span class="dg-chips"><span class="dg-chip dg-chip-mute" title="§11 is the FALLBACK, not the mechanism: a run that accumulates parks is a run whose plan was wrong">the plan missed these</span></span>
      </div>
      ${over}
      <ul class="po-list dg-parklist">${rows}</ul>
    </section>`;
  if (html === orch.parkedSig) return;
  orch.parkedSig = html;
  host.innerHTML = html;
  host.querySelectorAll("[data-task]").forEach((el) =>
    el.addEventListener("click", () => openNodeInspector(el.getAttribute("data-task"))));
  host.querySelectorAll(".dg-park").forEach((li) => {
    const id = li.getAttribute("data-id");
    const open = li.querySelector(".dg-parkinspect");
    if (open) open.addEventListener("click", () => openNodeInspector(id));
    const res = li.querySelector(".dg-parkresume");
    if (res) res.addEventListener("click", async () => {
      const call = bound("ResumeTask");
      if (!call || !runID) return;
      res.disabled = true;
      res.textContent = "Retrying…";
      let note = "";
      try {
        const out = await call(runID, id);
        note = out && out.hidden ? "hidden"
          : out && out.resumed ? "delivered — the child is running again"
          : out && out.childGone ? "the child this seed is owed to is gone — re-spawn it or abandon the task"
          : out && out.owed ? "still owed: the child was not at a prompt that consumes typed text. Loom keeps the seed and will retry."
          : out && out.conflict ? "the producer's branch would not merge into this child's worktree — the task stays parked"
          : (out && out.error) || "nothing changed";
      } catch (e) { note = String(e); }
      const line = document.createElement("div");
      line.className = "dgi-outcome";
      line.textContent = note;
      li.querySelector(".dg-parkacts").appendChild(line);
      res.disabled = false;
      res.textContent = "Retry the delivery";
      // Force the next render to rebuild: the park's own row may have changed
      // state, and the signature guard would otherwise hold the old one.
      orch.parkSig = "";
      await refreshOrchestration();
    });
  });
}

// ---- §5.1's park EDGES ------------------------------------------------------
//
// A park edge is a dependency the plan did not have: it exists because a child
// stopped and said so, never because the manifest declared it.
//
// The snapshot now carries them: GraphEdgeDTO.kind is derived in Go from
// delegate.Effective — declared edges + ACCEPTED AMENDMENTS + live blocks — so
// the server's set includes the parks that were granted and have since been
// worked through, which no live-declaration read can see. Those are the ones
// §2.6 most wants counted: a park that was accepted and resolved is still a
// plan that was wrong.
//
// What is synthesized below is the REMAINDER: a live declaration whose task row
// has not been through Detector.Poll yet, so the block_json column the server
// reads is still empty. It is a lag of at most one tick, and it is drawn rather
// than waited for because a park edge that appears a tick late is a wait the
// human cannot see during the tick it costs them.
function producerOf(artifact) {
  if (!artifact) return "";
  const n = (orch.nodes || []).find((x) => !x.hidden && (x.artifacts || []).includes(artifact));
  return n ? n.id : "";
}

function parkEdgeList() {
  const out = [];
  const declared = new Set((orch.edges || []).map((e) => e.from + "\0" + e.to));
  for (const [id, p] of orch.parks) {
    if (!p || p.malformed || p.kind !== "needs-artifact") continue;
    if (!nodeById(id)) continue;
    const from = (p.from && nodeById(p.from)) ? p.from : producerOf(p.artifact);
    if (!from || from === id || !nodeById(from)) continue;
    // A park that names a pair of tasks the payload ALREADY carries an edge for
    // — declared or park — is not a second edge: drawing a second dashed line
    // over one that is already there would inflate the run-health number §2.6
    // reads.
    if (declared.has(from + "\0" + id)) continue;
    out.push({ from, to: id, artifact: p.artifact, kind: "park" });
  }
  return out;
}

// parkEdgeCount is the run-health figure over BOTH sources — the payload's park
// edges and the not-yet-persisted remainder above. Counting only the second was
// the bug this replaces: it saw the standing live parks and none of the accepted
// amendments, so a run that rendezvoused six times, was granted every edge and
// recovered read as a plan that held.
function parkEdgeCount() {
  return (orch.edges || []).filter((e) => e.kind === "park").length + parkEdgeList().length;
}

// ---- block 3: the graph -----------------------------------------------------

// §9's two size caps. Above 60 nodes the graph collapses to repo bands (still a
// graph, still expandable); above 200 it is replaced by a table, because a
// picture nobody can read is worse than a list they can sort.
const BAND_AT = 60;
const TABLE_AT = 200;

function renderGraphBlock(block, relayout) {
  const msgs = document.getElementById("dg-msgs");
  const chips = document.getElementById("dg-chips");
  const host = document.getElementById("dg-host");
  if (!block || !msgs || !host) return;

  if (orch.unbound || (!orch.run && !orch.error && orch.loaded)) {
    // §3: absence of orchestration is NOT an error state. Blocks 1-3 are
    // absent, not empty, and block 4 carries on alone.
    block.hidden = true;
    return;
  }
  if (!orch.loaded) { block.hidden = true; return; }
  block.hidden = false;

  const msgHtml =
    (orch.error ? `<div class="po-warn dg-fatal">${esc(orch.error)}</div>` : "") +
    (orch.warnings || []).map((w) => `<div class="po-warn">${esc(w)}</div>`).join("");
  if (msgHtml !== orch.msgSig) { orch.msgSig = msgHtml; msgs.innerHTML = msgHtml; }

  if (!orch.nodes.length) {
    // A manifest that will not parse, a schema this build cannot render, or a
    // run whose snapshot holds no tasks. The message above IS the render —
    // never a spinner, never a blank host.
    host.innerHTML = orch.error ? "" : `<div class="po-empty">This run has no tasks in its manifest snapshot.</div>`;
    chips.textContent = "";
    orch.chipSig = "";  // so the chips repaint when a graph comes back
    orch.graph = null;
    return;
  }

  // The drawn edge set is the manifest's PLUS §5.1's park edges, which are
  // synthesized from the live declarations and therefore change without the
  // topology rev moving. Hence the signature below: `relayout` alone would keep
  // a park edge off the picture until the manifest itself changed.
  const parkEdges = parkEdgeList();
  const allEdges = orch.edges.concat(parkEdges);
  const topoSig = orch.nodes.map((n) => n.id).join(",") + "|" +
    allEdges.map((e) => e.from + ">" + e.to + ":" + (e.kind || "plan")).join(",");

  const banded = orch.nodes.length > BAND_AT && !orch.expanded;
  const asTable = orch.nodes.length > TABLE_AT && !orch.expanded;
  const chipHtml = [
    `<span class="dg-chip dg-chip-mute">${orch.nodes.length} nodes · ${orch.edges.length} edges</span>`,
    asTable ? `<span class="dg-chip dg-chip-warn">over ${TABLE_AT} nodes — shown as a table</span>` : "",
    (banded && !asTable) ? `<button class="dg-chip" id="dg-expand">collapsed to repo bands — expand</button>` : "",
    (orch.expanded && orch.nodes.length > BAND_AT) ? `<button class="dg-chip" id="dg-collapse">collapse to repo bands</button>` : "",
  ].filter(Boolean).join("");
  if (chipHtml !== orch.chipSig) {
    orch.chipSig = chipHtml;
    chips.innerHTML = chipHtml;
    const ex = document.getElementById("dg-expand");
    if (ex) ex.addEventListener("click", () => { orch.expanded = true; orch.graph = null; renderOrchestration(true); });
    const col = document.getElementById("dg-collapse");
    if (col) col.addEventListener("click", () => { orch.expanded = false; orch.graph = null; renderOrchestration(true); });
  }

  if (asTable) { orch.graph = null; orch.topoSig = ""; renderNodeTable(host); return; }

  const view = banded ? bandView(orch.nodes, allEdges, statusMap()) : {
    nodes: orch.nodes, edges: allEdges, statuses: statusMap(),
  };
  if (!orch.graph) {
    // A band is not a task, so clicking one expands rather than opening an
    // inspector on a node that has no brief, no scope and no check.
    const onNode = (id) => {
      if (id.startsWith("band:")) { orch.expanded = true; orch.graph = null; renderOrchestration(true); return; }
      openNodeInspector(id);
    };
    orch.graph = createGraph(host, { onNode, statusColor });
    orch.graph.setTopology(view.nodes, view.edges, banded ? null : orch.layout);
    orch.graph.fit();
    orch.graphRun = orch.run ? orch.run.id : 0;
    orch.topoSig = topoSig;
  } else if (relayout || topoSig !== orch.topoSig) {
    // §7.3: a rev change re-lays-out and PRESERVES the viewport. Only a run
    // switch re-fits, because that genuinely is a different picture.
    // The banded view passes no server layout: its nodes are synthesized here.
    orch.graph.setTopology(view.nodes, view.edges, banded ? null : orch.layout);
    if (orch.graphRun !== (orch.run ? orch.run.id : 0)) {
      orch.graph.fit();
      orch.graphRun = orch.run ? orch.run.id : 0;
    }
  }
  orch.graph.patch(view.statuses, { bottleneck: bandedId(banded, (orch.strip || {}).bottleneck) });
}

function bandedId(banded, id) {
  if (!banded || !id) return id || "";
  const n = nodeById(id);
  return n ? bandKey(n) : "";
}

function bandKey(n) {
  return "band:" + (n.hidden ? "·hidden" : (n.repo || "unassigned"));
}

// bandView is §9's >60-node collapse: one node per repo, edges aggregated, the
// same data underneath. The band's badge is the WORST state among its members,
// deliberately — a band that reads green because nine of its ten tasks passed
// is the summary lying about the one that failed.
function bandView(nodes, edges, statuses) {
  const bands = new Map();
  const keyOf = new Map();
  for (const n of nodes) {
    const key = bandKey(n);
    keyOf.set(n.id, key);
    let b = bands.get(key);
    if (!b) {
      b = { id: key, kind: "band", title: (n.hidden ? "hidden" : (n.repo || "unassigned")), repo: "", warnings: [], members: [] };
      bands.set(key, b);
    }
    b.members.push(n.id);
  }
  const bandEdges = new Map();
  for (const e of edges) {
    const a = keyOf.get(e.from), b = keyOf.get(e.to);
    if (!a || !b || a === b) continue;
    const k = a + "\0" + b;
    if (!bandEdges.has(k)) bandEdges.set(k, { from: a, to: b, artifact: "", cycle: !!e.cycle });
    else if (e.cycle) bandEdges.get(k).cycle = true;
  }
  const RANKS = ["failed", "blocked", "checking", "running", "spawning", "ready", "approved", "pending", "verified", "merged", "abandoned"];
  const out = new Map();
  for (const [key, b] of bands) {
    b.repo = b.members.length + " task" + (b.members.length === 1 ? "" : "s");
    let worst = -1, ready = false, sess = "";
    for (const id of b.members) {
      const st = statuses.get(id);
      if (!st) continue;
      if (st.ready) ready = true;
      if (st.sessionStatus === "needs_you" || !sess) sess = st.sessionStatus || sess;
      const idx = RANKS.indexOf(st.state);
      if (idx >= 0 && (worst < 0 || idx < worst)) worst = idx;
    }
    out.set(key, { id: key, state: worst >= 0 ? RANKS[worst] : "pending", ready, sessionStatus: sess });
  }
  return { nodes: [...bands.values()], edges: [...bandEdges.values()], statuses: out };
}

// §9's >200-node fallback: the same data and the same actions, as a table.
function renderNodeTable(host) {
  const sts = statusMap();
  const rows = orch.nodes.map((n) => {
    const st = sts.get(n.id) || {};
    const b = badgeFor(st);
    return `<tr class="dg-trow" data-id="${esc(n.id)}">
      <td>${esc(n.hidden ? "hidden" : (n.title || n.id))}</td>
      <td class="dg-tmono">${esc(n.repo || "")}</td>
      <td><span class="dg-tbadge ${b.cls}">${esc(b.text)}</span></td>
      <td>${esc(st.checkStatus || "—")}</td>
      <td>${esc(st.sessionStatus || "—")}</td>
      <td>${(n.warnings || []).length ? esc("⚠ " + n.warnings.join(" · ")) : ""}</td>
    </tr>`;
  }).join("");
  host.innerHTML = `<div class="dg-tablewrap"><table class="dg-table">
    <thead><tr><th>Task</th><th>Repo</th><th>State</th><th>Check</th><th>Session</th><th>Warnings</th></tr></thead>
    <tbody>${rows}</tbody></table></div>`;
  host.querySelectorAll(".dg-trow").forEach((tr) =>
    tr.addEventListener("click", () => openNodeInspector(tr.getAttribute("data-id"))));
}

// ---- the node inspector -----------------------------------------------------

// Evidence only, and every piece of it primary (§2.2): the recorded gate
// result, the child's own worktree diff, its brief and its authorization scope
// verbatim. There is deliberately no panel anywhere in this modal for an
// orchestrator's opinion of a child's work — reflection-style review measured
// worse than nothing, and rendering it would make it load-bearing.
function openNodeInspector(id) {
  if (document.querySelector(".modal-backdrop")) return;
  const n = nodeById(id);
  if (!n || n.hidden) return;
  const runID = orch.run ? orch.run.id : 0;

  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal dg-modal" role="dialog" aria-label="Task">
      <h2>${esc(n.title || n.id)}</h2>
      <div class="dgi-head">
        <span class="dgi-badge" id="dgi-badge"></span>
        <span class="po-tag">${esc(n.kind)}</span>
        <span class="dgi-dot" id="dgi-dot"></span>
        <span class="dgi-sess" id="dgi-sess"></span>
      </div>
      ${(n.warnings || []).length ? `<div class="po-warns">${n.warnings.map((w) => `<div class="po-warn">${esc(w)}</div>`).join("")}</div>` : ""}
      <div class="dgi-facts">
        ${fact("repo", n.repo)}${fact("path", n.path)}
        ${fact("worktree", n.worktree || "— not isolated")}${fact("branch", n.branch)}
        ${fact("artifacts", (n.artifacts || []).join(", "))}
      </div>
      <div class="dgi-sec">Authorization scope</div>
      ${n.authorization
        ? `<pre class="dgi-pre">${esc(n.authorization)}</pre>`
        : `<div class="po-warn">brief declares no authorization scope</div>`}
      <div class="dgi-sec">Brief</div>
      ${n.brief ? `<pre class="dgi-pre dgi-brief">${esc(n.brief)}</pre>` : `<div class="po-empty">no brief text in the manifest</div>`}
      <div class="dgi-sec">Check</div>
      ${(n.checkCmd || []).length
        ? `<pre class="dgi-pre dgi-cmd">${esc((n.checkCmd || []).join(" "))}</pre>`
        : `<div class="po-warn">no check declared — this task has no definition of done</div>`}
      <div id="dgi-flags"></div>
      <div id="dgi-check"></div>
      <div id="dgi-park"></div>
      <div id="dgi-memory"></div>
      <div id="dgi-integration"></div>
      <div id="dgi-gate"></div>
      <div id="dgi-merge"></div>
      <div class="modal-error" id="dgi-err"></div>
      <div class="modal-actions dgi-actions" id="dgi-actions">
        <button class="btn-ghost" id="dgi-close">Close</button>
      </div>
    </div>`;
  document.body.appendChild(backdrop);
  const close = () => { orch.inspectorUpdate = null; backdrop.remove(); };
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  backdrop.querySelector("#dgi-close").addEventListener("click", close);

  const errEl = backdrop.querySelector("#dgi-err");
  const actions = backdrop.querySelector("#dgi-actions");
  const act = (label, fn, primary) => {
    const b = document.createElement("button");
    b.className = primary ? "btn-launch" : "btn-ghost";
    b.textContent = label;
    b.addEventListener("click", () => fn(b));
    actions.appendChild(b);
    return b;
  };

  // The live half of the modal is refreshed by the poll rather than frozen at
  // open time (§7.3 patches, it does not re-render) — an inspector that still
  // said "checking" five minutes after the check finished would be the surface
  // the user stops trusting first.
  const paintLive = () => {
    const st = statusMap().get(id) || {};
    const b = badgeFor(st);
    const badge = backdrop.querySelector("#dgi-badge");
    badge.textContent = b.text;
    badge.className = "dgi-badge " + b.cls;
    const dot = backdrop.querySelector("#dgi-dot");
    const sess = backdrop.querySelector("#dgi-sess");
    if (st.sessionStatus) {
      dot.style.background = statusColor(st.sessionStatus);
      dot.style.display = "";
      sess.textContent = statusWord(st.sessionStatus) + (st.sessionName ? " · " + st.sessionName : "");
    } else {
      dot.style.display = "none";
      // §9: a node whose session row is gone keeps its manifest-authored
      // lifecycle badge and says so plainly, rather than greying the card and
      // leaving the user to guess.
      sess.textContent = st.state && st.state !== "pending"
        ? "no live session — resume it from Finished"
        : "not launched yet";
    }
    // §12.2/§12.3's badges with their SENTENCES, and the sentences come from Go
    // (NodeStatusDTO.flagDetails). Two of them are binding wording — the drift
    // note says "changed since spawn" and never "the child wrote this" — so
    // nothing here composes a string for a flag; the frontend only decides
    // where a loud one goes. A flag with no sentence still renders, as itself.
    const flagHost = backdrop.querySelector("#dgi-flags");
    const details = st.flagDetails || [];
    const rows = details.map((d) =>
      `<div class="${d.loud ? "po-warn" : "dgi-flagline"}"><code>${esc(d.name)}</code> ${esc(d.note || "")}</div>`);
    // The DEBT, read from the column rather than from the `seed-pending` badge.
    // §10.3's park owes a seed the moment it writes the column, and a view keyed
    // on the flag alone would show a parked child with nothing saying Loom owes
    // it a message.
    if (st.pendingSeed) {
      rows.push(`<div class="po-warn">Loom owes this child a message and has not delivered it yet — ` +
        `it is delivered when the child next reaches its prompt</div>`);
    }
    // §12.3.1 is "`diverged` flag WITH THE FILE LIST", and this is the list. It
    // rides on the poll payload — a decode of the column the check already
    // wrote, no git — so the human reads WHICH files without opening the merge
    // gate, which is the surface the acknowledgement lives on and therefore the
    // wrong place to have to go to find out whether there is anything to
    // acknowledge.
    const dv = st.divergence || null;
    if (dv && !dv.empty) {
      if ((dv.outside || []).length) {
        rows.push(`<div class="dgi-flagline">outside the task's declared paths: ` +
          dv.outside.map((f) => `<code>${esc(f)}</code>`).join(" ") + `</div>`);
      }
      // §12.3.2 is the stronger half — it PREDICTS the merge conflict before
      // integration reaches it — so it is loud where the first is not.
      for (const [sib, files] of Object.entries(dv.siblings || {})) {
        rows.push(`<div class="po-warn">inside <b>${esc(nodeTitle(sib))}</b>'s declared paths: ` +
          files.map((f) => `<code>${esc(f)}</code>`).join(" ") + `</div>`);
      }
    }
    flagHost.innerHTML = rows.length ? `<div class="dgi-sec">Flags</div>` + rows.join("") : "";

    const host = backdrop.querySelector("#dgi-check");
    if (!st.checkStatus) {
      host.innerHTML = `<div class="po-empty">no check has run — completion is only ever a check result</div>`;
    } else {
      host.innerHTML =
        `<div class="dgi-checkline ${st.checkStatus === "pass" ? "ok" : "bad"}">` +
        `${esc(st.checkStatus)} · exit ${esc(String(st.checkExit))} · ${esc(elapsedSince(st.checkAt))} ago` +
        (st.flags && st.flags.length ? ` · ${esc(st.flags.join(" "))}` : "") + `</div>` +
        (st.checkOut ? `<pre class="dgi-pre dgi-out">${esc(st.checkOut)}</pre>` : "");
    }
  };
  orch.inspectorUpdate = () => { if (backdrop.isConnected) paintLive(); else orch.inspectorUpdate = null; };
  paintLive();

  // The child's own account of its work (§10). This is placed BELOW the check
  // block deliberately: §2.1 is that a child's self-report never promotes a
  // node to done, and putting the two side by side would invite reading them as
  // equal evidence. The check says whether it worked; this says what the child
  // thought it did, which is what the human debugging a red check needs.
  //
  // Fetched rather than taken from latestRecent's summary, which was the honest
  // substitute before SessionMemory existed: a summary is only present for a
  // session that has ENDED and been summarized, so a RUNNING child — the one a
  // human is most likely to be inspecting — showed nothing at all.
  const st0 = statusMap().get(id) || {};
  const memory = bound("SessionMemory");
  if (memory && st0.sessionName) {
    memory(st0.sessionName).then((m) => {
      const host = backdrop.querySelector("#dgi-memory");
      // The modal may have closed, or the user may have clicked through to
      // another node, while this was in flight.
      if (!host || !host.isConnected || !m || m.hidden) return;
      const rows = [];
      if (m.ask) rows.push(`<div class="dgi-fact"><span>asked</span><code>${esc(m.ask)}</code></div>`);
      if (m.outcome) rows.push(`<div class="dgi-outcome">${esc(m.outcome)}</div>`);
      if (m.summary) rows.push(`<div class="dgi-outcome">${esc(m.summary)}</div>`);
      if (m.files) rows.push(`<div class="dgi-fact"><span>files</span><code>${esc(m.files)}</code></div>`);
      // Said out loud rather than rendered as an empty block: "the child
      // reported nothing" and "the record was deleted" are different facts and
      // a human would debug them differently.
      if (m.missing) rows.push(`<div class="po-warn">the transcript file this was built from is gone</div>`);
      if (!rows.length) return;
      host.innerHTML = `<div class="dgi-sec">What the child reported — not evidence of completion</div>` + rows.join("");
    }).catch((e) => console.error("session memory", e));
  }

  if (st0.sessionName) {
    act("Attach", () => { close(); selectSession(st0.sessionName); });
    act("Diff", () => { close(); openDiff(st0.sessionName); });
  }
  // There is deliberately no "open the worktree" action: OpenInEditor resolves
  // a REGULAR FILE and refuses a directory, so the button would fail every time
  // it was pressed. The worktree path is in the facts grid above, which is what
  // §2.3 actually asks for — isolation being VISIBLE, not navigable.
  const approve = bound("ApproveTask");
  if (approve && runID && st0.state === "ready") {
    act("Approve & spawn", async (b) => {
      b.disabled = true;
      try {
        const res = await approve(runID, id);
        if (res && res.error) errEl.textContent = res.error;
        else if (res && res.hidden) errEl.textContent = "hidden";
        else if (res && res.sessionName) { close(); selectSession(res.sessionName); }
      } catch (e) { errEl.textContent = String(e); }
      b.disabled = false;
      poll();
    }, true);
  }
  const check = bound("RunTaskCheck");
  if (check && runID) {
    act("Run check", async (b) => {
      b.disabled = true;
      b.textContent = "Running…";
      try {
        const res = await check(runID, id);
        if (res && res.error) errEl.textContent = res.error;
        if (res && (res.unpublished || []).length) {
          errEl.textContent = "unpublished artifacts: " + res.unpublished.join(", ");
        }
      } catch (e) { errEl.textContent = String(e); }
      b.disabled = false;
      b.textContent = "Run check";
      await refreshOrchestration();
      paintLive();
    });
  }
  // §11.1's block declaration, rendered as the child WROTE it. Fetched rather
  // than derived from the state chip: `blocked` says a child stopped, and this
  // says what it stopped on, which is the only half a human can act on.
  const park = bound("TaskPark");
  if (park && runID) {
    park(runID, id).then((p) => {
      const host = backdrop.querySelector("#dgi-park");
      if (!host || !host.isConnected || !p || p.hidden || !p.hasBlock) return;
      const bits = [`<div class="dgi-sec">Why this child stopped — its own declaration</div>`];
      if (p.malformed) {
        // A block Loom cannot parse is NEVER dropped: the child is parked
        // either way, and a swallowed declaration is a child parked forever
        // with nobody told. The raw text is shown because it is all there is.
        bits.push(`<div class="po-warn">the declaration will not parse: ${esc(p.parseError || "")}</div>`);
        bits.push(`<pre class="dgi-pre">${esc(p.raw || "")}</pre>`);
      } else {
        bits.push(`<div class="dgi-checkline bad">${esc(p.kind || "block")}` +
          (p.author ? ` · by ${esc(p.author)}` : "") + `</div>`);
        if (p.summary) bits.push(`<div class="dgi-outcome">${esc(p.summary)}</div>`);
        if (p.artifact) bits.push(fact("needs artifact", p.artifact + (p.from ? " (from " + p.from + ")" : "")));
        if ((p.paths || []).length) bits.push(fact("wants scope", p.paths.join(", ")));
        if (p.detail) bits.push(`<pre class="dgi-pre">${esc(p.detail)}</pre>`);
        if (p.resumeWhen) bits.push(`<div class="dgi-fact"><span>resume when</span><code>${esc(p.resumeWhen)}</code></div>`);
      }
      host.innerHTML = bits.join("");
    }).catch((e) => console.error("task park", e));
  }

  // §10.2, run on demand. Loom merges the task into its repo's INTEGRATION
  // worktree — a test bed, never the thing that ships — and re-runs the
  // per-repo gate there. The result names the STAGE, because "the integration
  // is red" is ambiguous and a conflict, a bootstrap failure and a red check
  // have completely different remedies.
  const integrate = bound("IntegrateTask");
  if (integrate && runID) {
    act("Integrate", async (b) => {
      b.disabled = true;
      b.textContent = "Integrating…";
      const host = backdrop.querySelector("#dgi-integration");
      try {
        const res = await integrate(runID, id);
        host.innerHTML = integrationHtml(res);
        // A run-level fault is not a modal-local fact: it stops spawning for
        // the whole run and it names no child. It is hoisted to the seam's
        // escalation band so it survives this modal closing, which is the one
        // way a "no task is to blame" finding gets read as a task's problem.
        noteBaselineFault(res, id);
      } catch (e) { errEl.textContent = String(e); }
      b.disabled = false;
      b.textContent = "Integrate";
      await refreshOrchestration();
      paintLive();
    });
  }

  // §5.2's gate, then §10.4's merge. TWO presses, deliberately: the first
  // computes and RENDERS the divergence and the drift, and the second carries
  // the human's acknowledgement of exactly what was rendered.
  //
  // The ack lists are the gate's own arrays, passed VERBATIM. Rebuilding them
  // here from the divergence report would defeat the staleness check outright —
  // Runner.Merge compares what was acknowledged against what is there NOW, in
  // both directions, and a client that regenerates the list can only ever agree
  // with itself.
  const gate = bound("TaskMergeGate");
  const doMerge = bound("MergeTask");
  if (gate && doMerge && runID) {
    act("Merge…", async (b) => {
      b.disabled = true;
      const host = backdrop.querySelector("#dgi-gate");
      let g;
      try { g = await gate(runID, id); }
      catch (e) { errEl.textContent = String(e); b.disabled = false; return; }
      b.disabled = false;
      if (!g || g.hidden) { errEl.textContent = "hidden"; return; }
      if (g.error) { errEl.textContent = g.error; return; }
      host.innerHTML = mergeGateHtml(g);
      const press = async (btn, force) => {
        btn.disabled = true;
        const was = btn.textContent;
        btn.textContent = "Merging…";
        try {
          // Verbatim. See above.
          const res = await doMerge(runID, id, g.ackDivergence || [], g.ackDrift || [], force);
          host.innerHTML = mergeResultHtml(res) + host.innerHTML;
          // §10.4 step 2: a merge that SUCCEEDED can leave the user's own branch
          // red, which is a baseline fault with no task blamed. That is the most
          // important thing this screen can say and it must outlive the modal.
          if (res && res.baseline && res.baseline.status && res.baseline.status !== "pass") {
            orch.baselineAlert = {
              taskId: id, repo: res.baseline.repo,
              note: "the integration baseline re-derived from your own branch after this merge is " +
                res.baseline.status + " — §10.4 step 2. No task is blamed.",
            };
            orch.escSig = "";
          }
        } catch (e) { errEl.textContent = String(e); }
        btn.disabled = false;
        btn.textContent = was;
        await refreshOrchestration();
        paintLive();
      };
      const go = host.querySelector("#dgi-domerge");
      if (go) go.addEventListener("click", () => press(go, false));
      // Force is armed by a second click, the same two-step the kill button
      // uses, and it is offered ONLY when there is something to force past. It
      // is never silent: the `forced` flag is written before the merge and
      // nothing reads it as permission.
      const forceBtn = host.querySelector("#dgi-forcemerge");
      if (forceBtn) {
        let armed = false;
        forceBtn.addEventListener("click", () => {
          if (!armed) {
            armed = true;
            forceBtn.classList.add("armed");
            forceBtn.textContent = "Force past the blockers?";
            return;
          }
          armed = false;
          forceBtn.classList.remove("armed");
          forceBtn.textContent = "Force merge";
          press(forceBtn, true);
        });
      }
    }, true);
  }

  // §11.4's delivery, retried by hand. A seed is OWED until it is delivered and
  // a timeout is explicitly not a failure, so this is a retry and never a
  // "send again" — the durable column is the same one either way.
  const resume = bound("ResumeTask");
  if (resume && runID) {
    act("Resume child", async (b) => {
      b.disabled = true;
      b.textContent = "Resuming…";
      try {
        const res = await resume(runID, id);
        if (res && res.hidden) errEl.textContent = "hidden";
        else if (res && res.error) errEl.textContent = res.error;
        else if (res && res.childGone) errEl.textContent = "the child this seed is owed to is gone — re-spawn it or abandon the task";
        else if (res && res.owed) errEl.textContent = "still owed: the child was not at a prompt that consumes typed text. Loom keeps the seed and will retry.";
      } catch (e) { errEl.textContent = String(e); }
      b.disabled = false;
      b.textContent = "Resume child";
      await refreshOrchestration();
      paintLive();
    });
  }

  const merge = bound("TaskMergeCommand");
  if (merge && runID) {
    act("Merge command", async () => {
      let cmd;
      try { cmd = await merge(runID, id); } catch (e) { errEl.textContent = String(e); return; }
      if (!cmd || cmd.hidden) { errEl.textContent = "hidden"; return; }
      if (cmd.error) { errEl.textContent = cmd.error; return; }
      // Loom PRINTS the merge command and never runs it (delegation §2). There
      // is no execute path here on purpose, and the warnings are rendered
      // beside it because a red merge you can explain is fine and a red merge
      // nobody was told about is not.
      const host = backdrop.querySelector("#dgi-merge");
      host.innerHTML = `<div class="dgi-sec">Merge — you run this, Loom does not</div>` +
        `<pre class="dgi-pre dgi-cmd">${esc(cmd.display)}</pre>` +
        (cmd.warnings || []).map((w) => `<div class="po-warn">${esc(w)}</div>`).join("") +
        `<button class="tact" id="dgi-copy">copy</button>`;
      const copy = backdrop.querySelector("#dgi-copy");
      copy.addEventListener("click", async () => {
        try { await navigator.clipboard.writeText(cmd.display); copy.textContent = "copied"; }
        catch { copy.textContent = "copy failed — select it"; }
      });
    });
  }
}

function fact(label, value) {
  if (!value) return "";
  return `<div class="dgi-fact"><span>${esc(label)}</span><code>${esc(value)}</code></div>`;
}

// integrationHtml renders one §10.2 pass. The STAGE leads, because that is what
// decides the remedy: a conflict is a human decision, a bootstrap failure is
// usually the environment, a per-repo failure is the task, and a cross failure
// may be either side of the seam.
function integrationHtml(res) {
  if (!res) return "";
  if (res.hidden) return `<div class="po-warn">hidden</div>`;
  if (res.error) return `<div class="po-warn">${esc(res.error)}</div>`;
  if (res.busy) return `<div class="po-warn">another integration is already running for this repo</div>`;
  const bits = [`<div class="dgi-sec">Integration — a staging worktree, never the thing that ships</div>`];
  if (!res.ran) {
    bits.push(`<div class="po-empty">nothing ran</div>`);
  } else {
    bits.push(`<div class="dgi-checkline ${res.status === "pass" ? "ok" : "bad"}">` +
      `${esc(res.stage || "")} · ${esc(res.status || "")}` +
      (res.blame ? ` · blamed: ${esc(res.blame)}` : "") + `</div>`);
  }
  // §10.2's attribution table, and it gets a FRAME of its own rather than a
  // word on a chip. The two rows of that table have opposite remedies — park
  // the child, or go and repair your repo — and blaming a task for a broken
  // baseline is the exact failure the table was written to prevent. The bit
  // read here is runLevelFault, never a string comparison on `blame`: a
  // frontend that has to match "baseline" to decide whether to blame a child is
  // a frontend that will one day blame the wrong one.
  if (res.runLevelFault) {
    bits.push(`<div class="dg-blame dg-blame-baseline">
      <div class="dg-blame-head">baseline fault — no task is to blame</div>
      <div>${esc(res.blameNote || "red with this task merged AND red without it: the repo's own tree is broken. Spawning stops and the repair is to the repo.")}</div>
      </div>`);
  } else if (res.blame === "task") {
    bits.push(`<div class="dg-blame dg-blame-task">
      <div class="dg-blame-head">this task is to blame</div>
      <div>${esc(res.blameNote || "red with this task merged and green without it.")}</div>
      </div>`);
  } else if (res.blameNote) {
    bits.push(`<div class="dgi-outcome">${esc(res.blameNote)}</div>`);
  }
  if ((res.conflicts || []).length) {
    bits.push(fact("conflicting files", res.conflicts.join(", ")));
  }
  if (res.crossCheck) bits.push(fact("cross check", res.crossCheck));
  for (const w of res.warnings || []) bits.push(`<div class="po-warn">${esc(w)}</div>`);
  if (res.output) bits.push(`<pre class="dgi-pre dgi-out">${esc(res.output)}</pre>`);
  return bits.join("");
}

// noteBaselineFault hoists §10.2's second attribution out of whatever modal
// observed it. Nothing is cleared here: the durable read (refreshRunRed) clears
// it when the run stops being red, which is the only evidence that the repo was
// actually repaired.
function noteBaselineFault(res, taskID) {
  if (!res || !res.runLevelFault) return;
  orch.baselineAlert = { taskId: taskID, repo: res.repo || "", note: res.blameNote || "" };
  orch.escSig = "";
  renderOrchestration();
}

// mergeGateHtml is §5.2 rendered: the diff's shape, the divergence, the drift,
// the blockers, and the exact target branch. The Merge button is offered even
// when there are blockers — greyed decisions are read, hidden ones are guessed
// at — but it carries the force wording when that is what it would be.
function mergeGateHtml(g) {
  const bits = [`<div class="dgi-sec">Merge gate — you are about to write to your own branch</div>`];
  bits.push(fact("merging", g.branch));
  bits.push(fact("into", g.target));
  bits.push(fact("repo", g.repo));
  if (g.dirty) {
    bits.push(`<div class="po-warn">your work tree is dirty — commit or stash first; ` +
      `this is refused even under force: ${esc((g.dirtyFiles || []).join(", "))}</div>`);
  }
  for (const b of g.blockers || []) bits.push(`<div class="po-warn">${esc(b)}</div>`);
  for (const w of g.warnings || []) bits.push(`<div class="po-warn">${esc(w)}</div>`);
  if ((g.ackDivergence || []).length) {
    bits.push(`<div class="dgi-sec">Committed outside its declared paths — you are acknowledging these</div>`);
    bits.push(`<pre class="dgi-pre">${esc(g.ackDivergence.join("\n"))}</pre>`);
  }
  if ((g.ackDrift || []).length) {
    // The hedge is Go's (DriftDTO.summary) and is reproduced verbatim, never
    // paraphrased: the walk cannot tell the human's own edits from a child's.
    bits.push(`<div class="dgi-sec">Changed outside the worktree since spawn — a separate acknowledgement</div>`);
    bits.push(`<pre class="dgi-pre">${esc(g.ackDrift.join("\n"))}</pre>`);
    if (g.drift && g.drift.summary) bits.push(`<div class="dgi-outcome">${esc(g.drift.summary)}</div>`);
  }
  bits.push(integrationHtml(g.integration));
  const blocked = (g.blockers || []).length > 0;
  bits.push(`<div class="modal-actions">` +
    `<button class="btn-launch" id="dgi-domerge">Merge this branch</button>` +
    // Offered, not hidden, when there is something to force past — a decision
    // that is not on screen is one the human makes from the terminal instead.
    (blocked ? `<button class="btn-ghost" id="dgi-forcemerge">Force merge</button>` : "") +
    `</div>`);
  return bits.join("");
}

function mergeResultHtml(res) {
  if (!res) return "";
  if (res.hidden) return `<div class="po-warn">hidden</div>`;
  const bits = [];
  if (res.merged) {
    bits.push(`<div class="dgi-checkline ok">merged into your branch${res.forced ? " (FORCED — recorded as such)" : ""}</div>`);
  } else {
    const why = res.stale ? "the gate you read is out of date — re-read it"
      : res.dirty ? "your work tree is dirty"
      : res.moved ? "the task moved under you — re-read the run"
      : "refused";
    bits.push(`<div class="dgi-checkline bad">not merged — ${esc(why)}</div>`);
  }
  if (res.error) bits.push(`<div class="po-warn">${esc(res.error)}</div>`);
  // §10.4 step 2's own sentences. These are the two facts a human most needs
  // after a merge — "your branch is red now" and "the worktree was left behind"
  // — and they exist nowhere else: they are composed on the integration result
  // the merge returns, not on any status column.
  for (const w of res.warnings || []) bits.push(`<div class="po-warn">${esc(w)}</div>`);
  if (res.baseline && res.baseline.status && res.baseline.status !== "pass") {
    bits.push(`<div class="po-warn">the re-derived integration baseline for ` +
      `${esc(res.baseline.repo || "")} is ${esc(res.baseline.status)} — that is a BASELINE fault, no task is blamed</div>`);
  }
  return bits.join("");
}

// ---- block 4: architecture & decisions (stage 4a) ---------------------------
//
// This block depends on nothing from slices 2-3: a project with no orchestrator
// at all still gets its outlines and decision records rendered, which is stage
// 4a's whole claim.
//
// §7.4 is served by a two-call split, and the split is the point: the per-tick
// call is ProjectDocumentsRev, which only stats, and the parse-and-render call
// is ProjectDocuments, which runs only when that number moved.
//
// Calling ProjectDocuments itself at 1.5s was never an option — it returns
// fully parsed token trees for every architecture file in the project, which is
// exactly what §7.5's cost ceiling forbids.
//
// probeDocuments degrades to the old behaviour when the probe is unbound (an
// older backend against a newer frontend): documents then refresh on open, on a
// manifest rev change, and on explicit Refresh only, which is stale rather than
// broken. A rev of 0 is the server's "hidden, or nothing to show" and is never
// treated as a change, so a hidden project cannot be made to re-fetch on a tick.
async function probeDocuments() {
  if (stageView.kind !== "project" || !orch.root) return;
  const probe = bound("ProjectDocumentsRev");
  if (!probe || orch.docsLoading) return;
  const root = orch.root;
  let rev = 0;
  try { rev = await probe(root); }
  catch { return; } // a failed probe is a missed refresh, never a broken seam
  if (stageView.kind !== "project" || orch.root !== root) return;
  if (!rev || rev === orch.docsRev) return;
  await refreshDocuments();
}

async function refreshDocuments() {
  if (stageView.kind !== "project" || !orch.root) return;
  const list = bound("ProjectDocuments");
  if (!list) { orch.docs = { unbound: true }; renderDocuments(); return; }
  if (orch.docsLoading) return;
  orch.docsLoading = true;
  const root = orch.root;
  // The rev is read BEFORE the documents, not after. A file edited while the
  // parse is in flight would otherwise be folded into the number we store
  // without ever being rendered, and the probe would agree we were up to date
  // forever. Taking it first means that race costs one extra refresh on the
  // next tick, which is the direction to be wrong in.
  const probe = bound("ProjectDocumentsRev");
  let rev = 0;
  if (probe) { try { rev = await probe(root); } catch { rev = 0; } }
  let set = null, err = "";
  try { set = await list(root); }
  catch (e) { err = String(e); }
  finally { orch.docsLoading = false; }
  if (stageView.kind !== "project" || orch.root !== root) return;
  orch.docs = set;
  orch.docsErr = err;
  // A failed fetch does not claim the rev: leaving it at 0 makes the next tick
  // retry rather than sitting on an error until the user presses Refresh.
  orch.docsRev = err ? 0 : rev;
  renderDocuments();
}

function renderDocuments() {
  const host = document.getElementById("po-docs");
  if (!host || !host.isConnected) return;
  host.replaceChildren();
  if (orch.hidden) return;

  const set = orch.docs;
  if (!set) {
    host.innerHTML = `<div class="po-bhead"><h3>Architecture &amp; decisions</h3></div><div class="po-empty">Reading…</div>`;
    return;
  }
  if (set.unbound) return; // no binding: the block is absent, not broken
  if (set.hidden) return;  // the constant line above already said it

  const head = document.createElement("div");
  head.className = "po-bhead";
  head.innerHTML = `<h3>Architecture &amp; decisions</h3><button class="sh-btn" id="doc-refresh">Refresh</button>`;
  host.appendChild(head);
  head.querySelector("#doc-refresh").addEventListener("click", () => {
    orch.docs = null;
    // Zero the rev too: an explicit Refresh must re-read even when the probe
    // says nothing moved, or the one gesture the user has for "I don't believe
    // you" would be the one gesture that does nothing.
    orch.docsRev = 0;
    renderDocuments();
    refreshDocuments().catch((e) => console.error("documents", e));
  });

  if (orch.docsErr) {
    host.appendChild(warnCard("could not read documents: " + orch.docsErr));
  }
  // §4.2: a refused document is VISIBLE, naming the path and the rule it broke.
  // A silently-dropped one is indistinguishable from a missing one, and the
  // user would go and debug the wrong thing.
  for (const r of set.refusals || []) host.appendChild(warnCard(`${r.Path} — ${r.Rule}`));
  for (const w of set.warnings || []) host.appendChild(warnCard(w));

  const docs = set.documents || [];
  if (!docs.length) {
    const e = document.createElement("div");
    e.className = "po-empty";
    e.textContent = "No architecture outline or decision records found under docs/.";
    host.appendChild(e);
    return;
  }

  const arch = docs.filter((d) => d.Kind === "architecture");
  const decisions = docs.filter((d) => d.Kind === "decision");
  const contracts = docs.filter((d) => d.Kind === "contract");

  if (arch.length) {
    if (!orch.docSel || !arch.some((d) => d.Path === orch.docSel)) orch.docSel = arch[0].Path;
    const tabs = document.createElement("div");
    tabs.className = "doc-tabs";
    for (const d of arch) {
      const b = document.createElement("button");
      b.className = "doc-tab" + (d.Path === orch.docSel ? " active" : "");
      b.textContent = d.Title || d.Name;
      b.title = d.Rel || d.Path;
      b.addEventListener("click", () => { orch.docSel = d.Path; renderDocuments(); });
      tabs.appendChild(b);
    }
    host.appendChild(tabs);
    const sel = arch.find((d) => d.Path === orch.docSel);
    if (sel) host.appendChild(readerFor(sel));
  }

  if (decisions.length) host.appendChild(decisionList(decisions));
  if (contracts.length) host.appendChild(contractList(contracts));
}

function warnCard(text) {
  const d = document.createElement("div");
  d.className = "po-warn";
  d.textContent = text;
  return d;
}

function docCtx(doc) {
  const dir = String(doc.Path || "").split("/").slice(0, -1).join("/");
  return {
    dir,
    docPath: doc.Path,
    // Both openers are the app's EXISTING gates. OpenInEditor stats the target
    // and refuses anything that is not a regular file, so a link out of a
    // document cannot become a launcher; OpenURL is the http(s) gate that
    // already ships and is already tested.
    openEditor: (p) => {
      const open = bound("OpenInEditor");
      if (open) open(activeName || "", p, 0).catch((e) => console.error("open", e));
    },
    openURL: (u) => { const g = bound("OpenURL"); if (g) g(u).catch((e) => console.error("openurl", e)); },
  };
}

function readerFor(doc) {
  const wrap = document.createElement("div");
  wrap.className = "doc-reader";
  const pane = document.createElement("div");
  pane.className = "doc-pane";
  const meta = document.createElement("div");
  meta.className = "doc-meta";
  meta.innerHTML =
    `<span class="doc-path">${esc(doc.Rel || doc.Path)}</span>` +
    (doc.Declared ? `<span class="po-tag">declared</span>` : "") +
    (doc.Truncated ? `<span class="po-tag dg-chip-warn">truncated at 512 KB</span>` : "");
  pane.appendChild(meta);
  for (const w of doc.Warnings || []) pane.appendChild(warnCard(w));
  pane.appendChild(renderBlocks(doc.Blocks, docCtx(doc)));
  wrap.appendChild(pane);
  wrap.appendChild(renderOutline(doc, pane));
  return wrap;
}

// Decision cards, newest first, superseded ones dimmed. §4.3 is binding on the
// metadata: with no `status:` the chip reads "status unknown" and never
// "accepted", and with no `date:` the card shows the file mtime LABELLED as
// mtime. A decision-record UI that confidently displays a fabricated "Accepted"
// is worse than one that displays nothing.
function decisionList(docs) {
  const superseded = new Set();
  for (const d of docs) {
    const s = (d.Meta && d.Meta.Supersedes) || "";
    for (const part of s.split(/[,\s]+/)) if (part) superseded.add(part);
  }
  const key = (d) => (d.Meta && d.Meta.Date) || "";
  const sorted = [...docs].sort((a, b) => {
    const ka = key(a), kb = key(b);
    if (ka !== kb) return ka < kb ? 1 : -1;
    return (b.ModTime || 0) - (a.ModTime || 0);
  });
  const sec = document.createElement("div");
  sec.className = "doc-sec";
  const h = document.createElement("div");
  h.className = "po-sub";
  h.textContent = "Decisions";
  sec.appendChild(h);
  for (const d of sorted) {
    const m = d.Meta || {};
    const old = (m.ID && superseded.has(m.ID)) || (d.Name && superseded.has(d.Name.replace(/\.md$/, "")));
    const card = document.createElement("details");
    card.className = "doc-card" + (old ? " superseded" : "");
    const sum = document.createElement("summary");
    sum.innerHTML =
      `<span class="doc-ctitle">${esc(d.Title || d.Name)}</span>` +
      `<span class="doc-status${m.StatusKnown ? "" : " unknown"}">${esc(m.StatusKnown ? m.Status : "status unknown")}</span>` +
      `<span class="doc-date">${esc(m.DateFromMTime ? fmtDate(d.ModTime) + " (mtime)" : m.Date)}</span>` +
      (old ? `<span class="po-tag">superseded</span>` : "") +
      (m.Consequence ? `<span class="doc-conseq">${esc(m.Consequence)}</span>` : "");
    card.appendChild(sum);
    card.addEventListener("toggle", () => {
      if (!card.open || card.dataset.filled) return;
      card.dataset.filled = "1";
      for (const w of d.Warnings || []) card.appendChild(warnCard(w));
      card.appendChild(renderBlocks(d.Blocks, docCtx(d)));
    });
    sec.appendChild(card);
  }
  return sec;
}

function contractList(docs) {
  const sec = document.createElement("div");
  sec.className = "doc-sec";
  const h = document.createElement("div");
  h.className = "po-sub";
  h.textContent = "Contracts";
  sec.appendChild(h);
  for (const d of docs) {
    const card = document.createElement("details");
    card.className = "doc-card";
    const sum = document.createElement("summary");
    sum.innerHTML =
      `<span class="doc-ctitle">${esc(d.Title || d.Name)}</span>` +
      (d.ID ? `<span class="po-tag">${esc(d.ID)}</span>` : "") +
      `<span class="doc-date">${esc(d.Rel || "")}</span>`;
    card.appendChild(sum);
    card.addEventListener("toggle", () => {
      if (!card.open || card.dataset.filled) return;
      card.dataset.filled = "1";
      card.appendChild(renderBlocks(d.Blocks, docCtx(d)));
    });
    sec.appendChild(card);
  }
  return sec;
}

function fmtDate(unix) {
  if (!unix) return "";
  const d = new Date(unix * 1000);
  return d.toISOString().slice(0, 10);
}

// ---- create project (§8) ----
async function openCreateProject() {
  if (document.querySelector(".modal-backdrop")) return;
  const picker = bound("OpenDirectoryDialog");
  const suggest = bound("SuggestRepos");
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal cp-modal" role="dialog" aria-label="New project">
      <h2>New project</h2>
      <div class="field">
        <label for="cp-root">Root directory</label>
        <div class="pm-row">
          <input id="cp-root" class="search-input" type="text" placeholder="/Users/you/Sauce/Innostream" autocomplete="off" spellcheck="false" />
          ${picker ? `<button class="btn-ghost" id="cp-pick">Choose…</button>` : ""}
        </div>
      </div>
      <div class="field">
        <label for="cp-name">Name</label>
        <input id="cp-name" class="search-input" type="text" placeholder="Innostream" autocomplete="off" spellcheck="false" />
      </div>
      <div class="field">
        <label>Repos</label>
        <div id="cp-repos" class="fan-projects"><div class="po-empty">Pick a root to list its repos.</div></div>
      </div>
      <div class="field">
        <label for="cp-extra">Add a repo outside the root</label>
        <div class="pm-row">
          <input id="cp-extra" class="search-input" type="text" placeholder="/Users/you/other/repo" autocomplete="off" spellcheck="false" />
          ${picker ? `<button class="btn-ghost" id="cp-extra-pick">Choose…</button>` : ""}
          <button class="btn-ghost" id="cp-extra-add">Add</button>
        </div>
      </div>
      <div class="modal-error" id="cp-error"></div>
      <div class="modal-actions">
        <button class="btn-ghost" id="cp-cancel">Cancel</button>
        <button class="btn-launch" id="cp-create">Create</button>
      </div>
    </div>`;
  document.body.appendChild(backdrop);
  const close = () => backdrop.remove();
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  backdrop.querySelector("#cp-cancel").addEventListener("click", close);

  const rootEl = backdrop.querySelector("#cp-root");
  const nameEl = backdrop.querySelector("#cp-name");
  const reposEl = backdrop.querySelector("#cp-repos");
  const errEl = backdrop.querySelector("#cp-error");
  const base = (p) => String(p).replace(/\/+$/, "").split("/").pop();

  const addCandidate = (path, checked) => {
    if (reposEl.querySelector(`input[data-path="${CSS.escape(path)}"]`)) return;
    const empty = reposEl.querySelector(".po-empty");
    if (empty) empty.remove();
    const l = document.createElement("label");
    l.className = "pf-check";
    l.innerHTML = `<input type="checkbox" data-path="${esc(path)}"${checked ? " checked" : ""} /><span>${esc(path)}</span>`;
    reposEl.appendChild(l);
  };

  // The checklist is PREFILLED from the root's children (§8). When the Go side
  // cannot enumerate them the root itself is still offered, so the modal is
  // never a dead end — a project of one is a legitimate shape (§2).
  const fillFor = async (root) => {
    reposEl.replaceChildren();
    let kids = [];
    if (suggest) { try { kids = (await suggest(root)) || []; } catch (e) { console.error("suggest", e); } }
    for (const k of kids) addCandidate(typeof k === "string" ? k : k.path, true);
    addCandidate(root, kids.length === 0);
  };

  rootEl.addEventListener("change", () => {
    const r = rootEl.value.trim();
    if (!r) return;
    if (!nameEl.value.trim()) nameEl.value = base(r);
    fillFor(r);
  });
  if (picker) backdrop.querySelector("#cp-pick").addEventListener("click", async () => {
    try {
      const p = await picker("Project root");
      if (!p) return;
      rootEl.value = p;
      if (!nameEl.value.trim()) nameEl.value = base(p);
      fillFor(p);
    } catch (e) { errEl.textContent = String(e); }
  });
  if (picker) backdrop.querySelector("#cp-extra-pick").addEventListener("click", async () => {
    try { const p = await picker("Repo outside the root"); if (p) backdrop.querySelector("#cp-extra").value = p; }
    catch (e) { errEl.textContent = String(e); }
  });
  backdrop.querySelector("#cp-extra-add").addEventListener("click", () => {
    const extra = backdrop.querySelector("#cp-extra");
    const v = extra.value.trim();
    if (!v) return;
    addCandidate(v, true);
    extra.value = "";
  });

  const createBtn = backdrop.querySelector("#cp-create");
  createBtn.addEventListener("click", async () => {
    const root = rootEl.value.trim();
    if (!root) { errEl.textContent = "Pick a root directory."; return; }
    const repos = [...reposEl.querySelectorAll("input:checked")].map((c) => c.getAttribute("data-path"));
    createBtn.disabled = true;
    try { await window.go.main.App.CreateProject(root, nameEl.value.trim() || base(root), repos); }
    catch (e) { errEl.textContent = String(e); createBtn.disabled = false; return; }
    close();
    await poll();
    openProject(root);
  });
}

// Hidden projects live behind an explicit gesture, never an ambient list: the
// point of §6 is that a client's name is not on screen unless you asked for
// it. Restoring one is a deliberate two-step (open this, then Show), which is
// the same reasoning as the chip's armed confirm.
function openHiddenProjects() {
  if (document.querySelector(".modal-backdrop")) return;
  const hidden = latestProjects.filter((p) => p.hidden);
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal" role="dialog" aria-label="Hidden projects">
      <h2>Hidden projects</h2>
      <ul class="po-list">${hidden.map((p) =>
        `<li class="po-repo" data-root="${esc(p.root)}"><span class="po-rlabel">${esc(p.name)}</span>` +
        `<span class="po-rpath">${esc(p.root)}</span>` +
        `<span class="po-racts"><button class="tact po-show">show</button></span></li>`).join("")}</ul>
      <div class="modal-actions"><button class="btn-ghost" id="hp-close">Close</button></div>
    </div>`;
  document.body.appendChild(backdrop);
  const close = () => backdrop.remove();
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  backdrop.querySelector("#hp-close").addEventListener("click", close);
  backdrop.querySelectorAll(".po-repo").forEach((li) => {
    li.querySelector(".po-show").addEventListener("click", async () => {
      try { await window.go.main.App.SetProjectHidden(li.getAttribute("data-root"), false); }
      catch (e) { console.error("show", e); }
      close();
      poll();
    });
  });
}

const newProjectBtn = document.getElementById("new-project");
if (newProjectBtn) newProjectBtn.addEventListener("click", openCreateProject);

// Projects-pane collapse: manual toggle + auto-collapse under the width
// threshold (effectiveCollapsed). The toggle sets the manual preference; a
// resize re-evaluates. The manual choice only takes effect above the threshold.
const ppCollapseBtn = document.getElementById("pp-collapse");
if (ppCollapseBtn) ppCollapseBtn.addEventListener("click", () => {
  manualCollapsed = !manualCollapsed;
  try { localStorage.setItem(PP_KEY, manualCollapsed ? "1" : "0"); } catch { /* storage off */ }
  applyCollapse();
});
window.addEventListener("resize", applyCollapse);

// ---- fan-out (same prompt across many projects) ----
async function openFanout() {
  if (document.querySelector(".modal-backdrop")) return;
  let projects = [];
  try { projects = await window.go.main.App.ListProjects(); } catch (e) { console.error("projects", e); }
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal fan-modal" role="dialog" aria-label="Fan out">
      <h2>Fan out — one prompt, many projects</h2>
      <div class="field">
        <label>Projects</label>
        <div id="fan-projects" class="fan-projects">${projects.map((p) => `<label class="pf-check${p.missing ? " dimmed" : ""}"><input type="checkbox" data-path="${esc(p.path)}"${p.missing ? " disabled" : ""} /><span>${esc(p.label)}${p.isRoot ? " · project root" : ""}${p.missing ? " · missing" : ""}</span></label>`).join("")}</div>
      </div>
      <div class="fan-row">
        <div class="field"><label for="fan-model">Model</label><select id="fan-model">${optionsHtml(MODELS)}</select></div>
        <div class="field"><label for="fan-mode">Permission mode</label><select id="fan-mode">${optionsHtml(MODES)}</select></div>
      </div>
      <div class="field"><label for="fan-seed">Seed prompt</label><textarea id="fan-seed" placeholder="Sent to every selected project"></textarea></div>
      <div class="modal-error" id="fan-error"></div>
      <div class="modal-actions">
        <button class="btn-ghost" id="fan-cancel">Cancel</button>
        <button class="btn-launch" id="fan-launch" disabled>Launch</button>
      </div>
    </div>`;
  document.body.appendChild(backdrop);
  const close = () => backdrop.remove();
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  backdrop.querySelector("#fan-cancel").addEventListener("click", close);

  const launchBtn = backdrop.querySelector("#fan-launch");
  const selected = () => [...backdrop.querySelectorAll("#fan-projects input:checked")].map((c) => c.getAttribute("data-path"));
  const updateCount = () => { const n = selected().length; launchBtn.textContent = n ? `Launch ${n}` : "Launch"; launchBtn.disabled = n === 0; };
  backdrop.querySelector("#fan-projects").addEventListener("change", updateCount);
  updateCount();

  launchBtn.addEventListener("click", async () => {
    const paths = selected();
    if (!paths.length) return;
    const model = backdrop.querySelector("#fan-model").value;
    const mode = backdrop.querySelector("#fan-mode").value;
    const seed = backdrop.querySelector("#fan-seed").value;
    launchBtn.disabled = true;
    let res;
    try { res = await window.go.main.App.Fanout(paths, model, mode, seed); }
    catch (e) { backdrop.querySelector("#fan-error").textContent = "Fan-out failed: " + e; launchBtn.disabled = false; return; }
    if (res.error) { backdrop.querySelector("#fan-error").textContent = res.error; launchBtn.disabled = false; return; }
    close();
    if (res.first) selectSession(res.first);
    poll();
  });
}

// ---- workflows ----
function openWorkflows() {
  if (document.querySelector(".modal-backdrop")) return;
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal wf-modal" role="dialog" aria-label="Workflows">
      <h2>Workflows</h2>
      <div id="wf-body" class="wf-body">Loading…</div>
      <div class="modal-actions"><button class="btn-ghost" id="wf-close">Close</button></div>
    </div>`;
  document.body.appendChild(backdrop);
  const close = () => backdrop.remove();
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  backdrop.querySelector("#wf-close").addEventListener("click", close);
  renderWF(backdrop.querySelector("#wf-body"), close);
}

function wfRunHtml(r) {
  const marks = [];
  if (r.pendingSeed) marks.push(`<span class="wf-mark">seed pending</span>`);
  if (r.seedFailed) marks.push(`<span class="wf-mark wf-bad">seed FAILED</span>`);
  return `<li class="wf-item${r.defErr ? " wf-err" : ""}" data-run="${r.id}">
    <div class="wf-main">
      <span class="wf-name">${esc(r.name)}#${r.id}</span>
      <span class="wf-sub">${esc(r.stepLabel)} · ${esc(r.status)}</span>
      ${marks.join("")}
    </div>
    <div class="wf-acts">
      ${r.pendingSeed ? `<button class="tact wf-retry" title="Retry seed">retry</button>` : ""}
      ${r.live ? `<button class="tact wf-attach" title="Attach session">attach</button>` : ""}
      <button class="tact wf-adv" title="Advance a step">n ▸</button>
      <button class="tact wf-abandon" title="Abandon run">✕</button>
    </div></li>`;
}

function wfDefHtml(d, i) {
  return `<li class="wf-item wf-def" data-def="${i}">
    <div class="wf-main">
      <span class="wf-name">${esc(d.name)}</span>
      <span class="wf-sub">${d.steps} step${d.steps === 1 ? "" : "s"}${d.project ? " · " + esc(d.project) : ""}</span>
    </div>
    <div class="wf-acts"><button class="tact wf-start" title="Start workflow">start ▸</button></div></li>`;
}

function wfErrHtml(e) {
  const base = String(e.path).split("/").pop();
  return `<li class="wf-item wf-err"><div class="wf-main"><span class="wf-name">${esc(base)}</span><span class="wf-sub">${esc(e.err)}</span></div></li>`;
}

async function renderWF(body, close) {
  body.textContent = "Loading…";
  let runs = [], wf = { defs: [], errors: [] };
  try {
    [runs, wf] = await Promise.all([
      window.go.main.App.ListRuns(),
      window.go.main.App.ListWorkflows(),
    ]);
  } catch (e) { console.error("workflows", e); }

  const errs = wf.errors || [];
  const runsHtml = runs.length ? `<ul class="wf-list">${runs.map(wfRunHtml).join("")}</ul>` : `<div class="wf-empty">No active runs.</div>`;
  const defsHtml = (wf.defs.length || errs.length)
    ? `<ul class="wf-list">${wf.defs.map(wfDefHtml).join("") + errs.map(wfErrHtml).join("")}</ul>`
    : `<div class="wf-empty">No workflow files in ~/.loom/workflows.</div>`;
  body.innerHTML = `<div class="wf-sec">Runs</div>${runsHtml}<div class="wf-sec">Workflows</div>${defsHtml}`;

  runs.forEach((r) => {
    const li = body.querySelector(`[data-run="${r.id}"]`);
    if (!li) return;
    const on = (sel, fn) => { const el = li.querySelector(sel); if (el) el.addEventListener("click", fn); };
    on(".wf-attach", async () => { try { const n = await window.go.main.App.AttachRun(r.id); if (n) { close(); selectSession(n); } } catch (e) { console.error("attach", e); } });
    on(".wf-adv", () => wfAdvance(body, close, r));
    on(".wf-abandon", () => wfAbandon(body, close, r));
    on(".wf-retry", async () => { try { await window.go.main.App.RetryRunSeed(r.id); } catch (e) { console.error("retry", e); } renderWF(body, close); });
  });
  wf.defs.forEach((d, i) => {
    const li = body.querySelector(`[data-def="${i}"]`);
    if (li) li.querySelector(".wf-start").addEventListener("click", async () => {
      try { await window.go.main.App.StartWorkflow(d.path); } catch (e) { console.error("start", e); }
      renderWF(body, close);
    });
  });
}

async function wfAdvance(body, close, r) {
  let p;
  try { p = await window.go.main.App.PreviewAdvance(r.id); }
  catch (e) { console.error("preview", e); renderWF(body, close); return; }
  const finish = p.finish;
  const rel = finish ? "" :
    `<div class="wf-crow">relation <b>${esc(p.relation)}</b>${p.relation === "continue" ? " — sends into the current session" : " — launches a new session"}</div>`;
  const seed = (!finish && p.seed) ? `<div class="wf-seed">${esc(p.seed.slice(0, 240))}${p.seed.length > 240 ? "…" : ""}</div>` : "";
  const warn = p.unavailable ? `<div class="wf-warn">some {{prev.*}} tokens were unavailable</div>` : "";
  body.innerHTML = `
    <div class="wf-confirm">
      <div class="wf-ctitle">${finish ? "Finish this run?" : "Advance to “" + esc(p.label) + "”"}</div>
      ${rel}${seed}${warn}
      <div class="wf-cerr" id="wf-cerr"></div>
      <div class="modal-actions">
        <button class="btn-ghost" id="wf-cancel">Cancel</button>
        <button class="btn-launch" id="wf-go">${finish ? "Finish" : "Advance"}</button>
      </div>
    </div>`;
  body.querySelector("#wf-cancel").addEventListener("click", () => renderWF(body, close));
  body.querySelector("#wf-go").addEventListener("click", async () => {
    if (finish) {
      try { await window.go.main.App.FinishRun(r.id); } catch (e) { console.error("finish", e); }
      renderWF(body, close); return;
    }
    let res;
    try { res = await window.go.main.App.AdvanceRun(r.id, false); }
    catch (e) { body.querySelector("#wf-cerr").textContent = String(e); return; }
    if (res.continueDead) {
      const errEl = body.querySelector("#wf-cerr");
      errEl.innerHTML = `The continue target session is gone. <button class="tact" id="wf-fork">Fork instead</button>`;
      body.querySelector("#wf-fork").addEventListener("click", async () => {
        try { await window.go.main.App.AdvanceRun(r.id, true); } catch (e) { console.error("fork", e); }
        renderWF(body, close);
      });
      return;
    }
    if (res.error) { body.querySelector("#wf-cerr").textContent = res.error; return; }
    renderWF(body, close); // advanced or stale → refresh
  });
}

function wfAbandon(body, close, r) {
  body.innerHTML = `
    <div class="wf-confirm">
      <div class="wf-ctitle">Abandon ${esc(r.name)}#${r.id}?</div>
      <div class="wf-crow">The run is marked abandoned. Its session keeps running (not killed).</div>
      <div class="modal-actions">
        <button class="btn-ghost" id="wf-cancel">Cancel</button>
        <button class="btn-launch" id="wf-go">Abandon</button>
      </div>
    </div>`;
  body.querySelector("#wf-cancel").addEventListener("click", () => renderWF(body, close));
  body.querySelector("#wf-go").addEventListener("click", async () => {
    try { await window.go.main.App.AbandonRun(r.id); } catch (e) { console.error("abandon", e); }
    renderWF(body, close);
  });
}

// ---- preferences ----
async function openPrefs() {
  if (document.querySelector(".modal-backdrop")) return;
  let prefs = { editor: "", notifications: true, autoSummarize: false, terminalTheme: "light" };
  try { prefs = await window.go.main.App.GetPrefs(); } catch (e) { console.error("prefs", e); }
  const editors = [["", "Auto-detect"], ["cursor", "Cursor"], ["code", "VS Code"], ["zed", "Zed"]];
  const themes = [["light", "Light (Blush)"], ["dark", "Dark"]];
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal prefs-modal" role="dialog" aria-label="Preferences">
      <h2>Preferences</h2>
      <div class="field">
        <label for="pf-editor">Editor for opening files</label>
        <select id="pf-editor">${editors.map(([v, l]) => `<option value="${v}"${v === prefs.editor ? " selected" : ""}>${l}</option>`).join("")}</select>
      </div>
      <div class="field">
        <label for="pf-term">Terminal theme</label>
        <select id="pf-term">${themes.map(([v, l]) => `<option value="${v}"${v === (prefs.terminalTheme || "light") ? " selected" : ""}>${l}</option>`).join("")}</select>
      </div>
      <label class="pf-check"><input type="checkbox" id="pf-notif"${prefs.notifications ? " checked" : ""} /><span>Native notification when a session needs you</span></label>
      <label class="pf-check"><input type="checkbox" id="pf-autosum"${prefs.autoSummarize ? " checked" : ""} /><span>Auto-summarize finished sessions <em>(uses a little Claude quota)</em></span></label>
      <div class="modal-actions">
        <button class="btn-ghost" id="pf-cancel">Cancel</button>
        <button class="btn-launch" id="pf-save">Save</button>
      </div>
    </div>`;
  document.body.appendChild(backdrop);
  const close = () => backdrop.remove();
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  backdrop.querySelector("#pf-cancel").addEventListener("click", close);
  backdrop.querySelector("#pf-save").addEventListener("click", async () => {
    const next = {
      editor: backdrop.querySelector("#pf-editor").value,
      notifications: backdrop.querySelector("#pf-notif").checked,
      autoSummarize: backdrop.querySelector("#pf-autosum").checked,
      terminalTheme: backdrop.querySelector("#pf-term").value,
    };
    try { await window.go.main.App.SetPrefs(next); } catch (e) { console.error("setprefs", e); }
    applyTermTheme(next.terminalTheme); // recolor the open terminal + pane at once
    close();
  });
}

// ---- quick reply (triage without attaching) ----
function openReply(name) {
  if (document.querySelector(".modal-backdrop")) return;
  const s = latestSessions.find((x) => x.name === name);
  const label = s ? displayName(s) : name;
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal reply-modal" role="dialog" aria-label="Quick reply">
      <h2>Reply to ${esc(label)}</h2>
      <input id="reply-input" class="search-input" type="text" placeholder="Type a message — Enter sends it to the session…" autocomplete="off" spellcheck="false" />
      <div class="modal-actions">
        <button class="btn-ghost" id="reply-cancel">Cancel</button>
        <button class="btn-launch" id="reply-send">Send</button>
      </div>
    </div>`;
  document.body.appendChild(backdrop);
  const input = backdrop.querySelector("#reply-input");
  input.focus();
  const close = () => backdrop.remove();
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  backdrop.querySelector("#reply-cancel").addEventListener("click", close);
  const send = async () => {
    if (!input.value.trim()) return;
    try { await window.go.main.App.SendReply(name, input.value); }
    catch (e) { console.error("reply", e); }
    close();
    poll();
  };
  backdrop.querySelector("#reply-send").addEventListener("click", send);
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") { e.preventDefault(); send(); }
    else if (e.key === "Escape") { e.preventDefault(); close(); }
  });
}

// ---- diff review ----
// Split a unified diff into per-file chunks (each starts with "diff --git"),
// pulling the file path from the header for the collapsible section title.
function splitPatchByFile(patch) {
  if (!patch || !patch.trim()) return [];
  return patch.split(/\n(?=diff --git )/).filter((c) => c.trim()).map((body) => {
    const first = body.split("\n", 1)[0];
    const mm = first.match(/^diff --git a\/(.+?) b\/(.+)$/);
    return { file: mm ? mm[2] : first, body };
  });
}

// Count added/removed lines in a file chunk (excluding the +++/--- headers).
function patchCounts(body) {
  let add = 0, del = 0;
  for (const ln of body.split("\n")) {
    if (ln.startsWith("+") && !ln.startsWith("+++")) add++;
    else if (ln.startsWith("-") && !ln.startsWith("---")) del++;
  }
  return { add, del };
}

function renderPatch(patch) {
  return patch.split("\n").map((ln) => {
    let cls = "";
    if (ln.startsWith("+++") || ln.startsWith("---") || ln.startsWith("diff ") || ln.startsWith("index ")) cls = "d-meta";
    else if (ln.startsWith("@@")) cls = "d-hunk";
    else if (ln.startsWith("+")) cls = "d-add";
    else if (ln.startsWith("-")) cls = "d-del";
    return `<span class="dl ${cls}">${esc(ln)}</span>`;
  }).join("\n");
}

async function openDiff(name) {
  if (document.querySelector(".modal-backdrop")) return;
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal diff-modal" role="dialog" aria-label="Uncommitted changes">
      <h2>Uncommitted changes</h2>
      <div id="diff-body" class="diff-body">Loading…</div>
      <div class="modal-actions"><button class="btn-ghost" id="diff-close">Close</button></div>
    </div>`;
  document.body.appendChild(backdrop);
  const close = () => backdrop.remove();
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  backdrop.querySelector("#diff-close").addEventListener("click", close);

  const body = backdrop.querySelector("#diff-body");
  let d;
  try { d = await window.go.main.App.SessionDiff(name); }
  catch (e) { body.textContent = "diff failed: " + e; return; }

  const repos = (d && d.repos) || [];
  if (!repos.length) { body.innerHTML = `<div class="diff-empty">No working directory for this session.</div>`; return; }
  if (!repos.some((r) => r.dirty || r.error)) {
    body.innerHTML = `<div class="diff-empty">No uncommitted changes.</div>`;
    return;
  }

  // One section per repo (§8). The diff is sectioned rather than concatenated
  // precisely so this splitter keeps working: an injected "── repo ──" header
  // between two patches would be parsed as part of the preceding file's hunk.
  // Single-repo sessions get the same markup with the heading suppressed, so
  // the common case reads exactly as it did before.
  const solo = repos.length === 1;
  let html = "";
  for (const r of repos) {
    let inner = "";
    if (r.error) {
      inner = `<div class="diff-empty">${esc(r.error)}</div>`;
    } else if (!r.dirty) {
      inner = `<div class="diff-empty">No uncommitted changes.</div>`;
    } else {
      if (r.stat) inner += `<pre class="diff-stat">${esc(r.stat)}</pre>`;
      if (r.untracked && r.untracked.length) {
        // git reports untracked paths relative to THEIR repo, while the editor
        // binding resolves a relative path against the session cwd — which is
        // the wrong base for every section but the primary. Absolutise here.
        inner += `<div class="diff-untracked"><span class="du-head">new files</span>` +
          r.untracked.map((f) => `<span class="du-file" data-path="${esc(r.path ? r.path + "/" + f : f)}">${esc(f)}</span>`).join("") + `</div>`;
      }
      for (const f of splitPatchByFile(r.patch)) {
        const c = patchCounts(f.body);
        inner += `<details class="diff-file" open>
          <summary><span class="df-name">${esc(f.file)}</span><span class="df-counts"><span class="df-add">+${c.add}</span> <span class="df-del">−${c.del}</span></span></summary>
          <pre class="diff-patch">${renderPatch(f.body)}</pre>
        </details>`;
      }
    }
    html += solo ? inner : `<section class="diff-repo">
      <div class="dr-head"><span class="dr-label">${esc(r.label || r.path)}</span><span class="dr-path">${esc(r.path)}</span></div>
      ${inner}</section>`;
  }
  body.innerHTML = html;
  body.querySelectorAll(".du-file").forEach((el) => {
    el.addEventListener("click", () => openFileToken(el.getAttribute("data-path")));
  });
}

// ---- terminal ----
let term = null;
let fit = null;
let dataUnsub = null;
let exitUnsub = null;
let termRO = null; // ResizeObserver on the terminal host — see selectSession
let attachGen = 0; // bumped per selectSession; guards stale async callbacks

function b64ToBytes(b64) {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

function teardownTerminal() {
  if (dataUnsub) { dataUnsub(); dataUnsub = null; }
  if (exitUnsub) { exitUnsub(); exitUnsub = null; }
  if (termRO) { termRO.disconnect(); termRO = null; }
  window.removeEventListener("resize", onResize);
  if (activeName) { window.go.main.App.CloseSession(activeName); }
  if (term) { term.dispose(); term = null; }
}

function selectSession(name) {
  teardownTerminal();
  activeName = name;
  stageView = { kind: "session", name };
  const gen = ++attachGen;

  const stage = document.getElementById("stage");
  stage.replaceChildren();
  const header = document.createElement("div");
  header.id = "stage-header";
  stage.appendChild(header);
  const outer = document.createElement("div");
  outer.id = "terminal";
  stage.appendChild(outer);
  // xterm mounts into this padding-free inner wrapper so FitAddon reads the
  // content width (not the border-box width incl. padding) — see #term-inner in
  // tokens.css. Without it the grid overflows and clips text on the right.
  const host = document.createElement("div");
  host.id = "term-inner";
  outer.appendChild(host);
  renderStageHeader(name);
  renderRail(latestSessions, latestRecent); // reflect active highlight immediately

  term = new Terminal({
    fontFamily: getComputedStyle(document.documentElement).getPropertyValue("--font-mono"),
    fontSize: 13,
    theme: xtermThemeFor(termThemeMode),
    cursorBlink: true,
    // Unicode11Addon registers via the proposed unicode API; without this
    // flag loadAddon THROWS, selectSession dies mid-flight, and clicking a
    // thread leaves a blank stage with no attach.
    allowProposedApi: true,
  });
  // Match tmux and the Claude Code TUI, which measure emoji as 2 cells wide.
  // xterm.js defaults to Unicode v6, which measures several emoji as 1 cell —
  // so color glyphs paint over the following text and line wrapping drifts.
  term.loadAddon(new Unicode11Addon());
  term.unicode.activeVersion = "11";
  fit = new FitAddon();
  term.loadAddon(fit);
  term.open(host);
  fit.fit();

  dataUnsub = window.runtime.EventsOn("pty:data:" + name, (b64) => term.write(b64ToBytes(b64)));
  exitUnsub = window.runtime.EventsOn("pty:exit:" + name, () => {
    term.write("\r\n\x1b[2m[session ended]\x1b[0m\r\n");
  });
  term.onData((data) => window.go.main.App.SendInput(name, data));
  registerFileLinks(term);

  window.go.main.App.AttachSession(name)
    .then(() => {
      if (gen !== attachGen) return; // a newer session was selected meanwhile
      fit.fit();
      window.go.main.App.ResizeSession(name, term.cols, term.rows);
    })
    .catch((e) => {
      if (gen !== attachGen) return;
      term.write("\r\n\x1b[31mattach failed: " + e + "\x1b[0m\r\n");
    });

  // A ResizeObserver on the terminal host is the RELIABLE resize signal. The
  // window 'resize' event can fire mid-maximize, before layout settles, so
  // fit() reads an intermediate size and the grid ends up wider/taller than the
  // pane — clipping text on the right and eating the bottom padding (both
  // observed). The observer fires AFTER layout with the final size; a rAF
  // coalesces the burst a resize produces. window 'resize' is kept as a
  // belt-and-braces fallback. Observing the host (not .xterm) can't loop: the
  // host's size comes from the flex layout, not from the grid fit changes.
  window.addEventListener("resize", onResize);
  let roPending = false;
  termRO = new ResizeObserver(() => {
    if (roPending) return;
    roPending = true;
    requestAnimationFrame(() => { roPending = false; onResize(); });
  });
  termRO.observe(host);
  // The very first fit can run before the mono font has been measured, giving a
  // wrong cell width and thus the wrong column count; re-fit once fonts are
  // ready so the grid matches the pane on the initial attach too.
  document.fonts.ready.then(() => { if (gen === attachGen) onResize(); });
}

function onResize() {
  if (!term || !fit || !activeName) return;
  fit.fit();
  window.go.main.App.ResizeSession(activeName, term.cols, term.rows);
}

// Detect file paths in terminal output and make them ⌘-clickable. Matches a
// path with a directory segment (…/file.ext) or a bare filename with a line
// (file.ext:88) — both with optional :line[:col] — to avoid underlining every
// word.ext token. The name before the extension may be empty so dotfiles like
// .env / .gitignore linkify too (…/​.env:1 or .env:1). ⌘-click resolves against
// the session cwd and opens the editor (the backend no-ops if it isn't a real
// file); a plain click is left to normal terminal text-selection, the iTerm2 /
// VS Code convention.
const FILE_LINK_RE =
  /(?:\.{0,2}\/)?(?:[\w.@~+-]+\/)+[\w.@~+-]*\.[A-Za-z][\w]{0,9}(?::\d+(?::\d+)?)?|[\w.@~+-]*\.[A-Za-z][\w]{0,9}:\d+(?::\d+)?/g;

// http(s) URLs. Stops at whitespace, quotes, and closing brackets; trailing
// sentence punctuation is stripped in the handler below.
const URL_RE = /\bhttps?:\/\/[^\s)\]}'"<>]+/g;

// Well-known extension-less files (Makefile, Dockerfile, …). Matched only with
// a directory segment or a :line so bare words in prose don't linkify; the
// (?![\w.@~+-]) / (?<![\w.@~+-]) guards stop partial matches like READMEISH.
const EXTLESS_NAMES = "Makefile|Dockerfile|Containerfile|Jenkinsfile|Procfile|Rakefile|Gemfile|Justfile|Vagrantfile|Brewfile|Caddyfile|Taskfile|Gruntfile|Podfile|LICENSE|README|CHANGELOG|CODEOWNERS|NOTICE|AUTHORS";
const EXTLESS_RE = new RegExp(
  "(?:\\.{0,2}\\/)?(?:[\\w.@~+-]+\\/)+(?:" + EXTLESS_NAMES + ")(?![\\w.@~+-])(?::\\d+(?::\\d+)?)?" +
  "|(?<![\\w.@~+-])(?:" + EXTLESS_NAMES + ")(?![\\w.@~+-]):\\d+(?::\\d+)?", "g");

function registerFileLinks(t) {
  t.registerLinkProvider({
    provideLinks(y, cb) {
      const bufLine = t.buffer.active.getLine(y - 1);
      if (!bufLine) { cb(undefined); return; }
      const text = bufLine.translateToString(true);
      const links = [];
      const urlSpans = [];
      let m;
      // ⌘-clickable, like file paths — a plain click stays terminal text
      // selection so it doesn't fight the app's own mouse handling.
      const link = (start, len, activate) => ({
        text: text.substr(start, len),
        range: { start: { x: start + 1, y }, end: { x: start + len, y } },
        activate,
        // Teach the ⌘-click gesture — links underline on hover but only open on
        // ⌘-click, which isn't obvious (people try a plain click first).
        hover: (e) => showLinkTip(e),
        leave: hideLinkTip,
      });

      // URLs first, so a URL's :port isn't misread as a file:line.
      URL_RE.lastIndex = 0;
      while ((m = URL_RE.exec(text)) !== null) {
        const url = m[0].replace(/[.,;:!?]+$/, ""); // drop trailing sentence punctuation
        if (!url) continue;
        urlSpans.push([m.index, m.index + url.length]);
        links.push(link(m.index, url.length, (e) => { if (e && e.metaKey) openURLToken(url); }));
      }

      // File paths (with an extension) and well-known extension-less files,
      // skipping any that overlap a matched URL.
      const notInURL = (s, e) => !urlSpans.some(([a, b]) => s < b && e > a);
      for (const re of [FILE_LINK_RE, EXTLESS_RE]) {
        re.lastIndex = 0;
        while ((m = re.exec(text)) !== null) {
          const s = m.index, e = m.index + m[0].length;
          if (!notInURL(s, e)) continue;
          const token = m[0];
          links.push(link(s, m[0].length, (ev) => { if (ev && ev.metaKey) openFileToken(token); }));
        }
      }
      cb(links.length ? links : undefined);
    },
  });
}

function openFileToken(token) {
  if (!activeName) return;
  let path = token;
  let line = 0;
  const cm = token.match(/^(.+?):(\d+)(?::\d+)?$/);
  if (cm) { path = cm[1]; line = parseInt(cm[2], 10); }
  window.go.main.App.OpenInEditor(activeName, path, line).catch((e) => console.error("open", e));
}

function openURLToken(url) {
  window.go.main.App.OpenURL(url).catch((e) => console.error("openurl", e));
}

// A small "⌘-click to open" tooltip shown while hovering a terminal link, so
// the ⌘-click gesture is discoverable.
let linkTip = null;
function showLinkTip(e) {
  if (!linkTip) {
    linkTip = document.createElement("div");
    linkTip.className = "link-tip";
    linkTip.textContent = "⌘-click to open";
    document.body.appendChild(linkTip);
  }
  linkTip.style.left = (e.clientX + 12) + "px";
  linkTip.style.top = (e.clientY - 28) + "px";
  linkTip.style.display = "block";
}
function hideLinkTip() { if (linkTip) linkTip.style.display = "none"; }

// ---- poll ----
async function poll() {
  try {
    const listProjects = bound("ListProjectDetails");
    const [sessions, recent, projects] = await Promise.all([
      window.go.main.App.ListSessions(),
      window.go.main.App.ListRecent(),
      listProjects ? listProjects().catch(() => latestProjects) : Promise.resolve(latestProjects),
    ]);
    latestSessions = sessions;
    latestRecent = recent;
    latestProjects = projects || [];
    if (!navInit) { navInit = true; initNav(); } // first poll: sync stage to restored selection
    renderRail();
    renderAttention(sessions);
    renderHideChip();
    if (activeName) renderStageHeader(activeName);
    // The overview refreshes only its session lists: re-rendering the whole
    // pane every 1.5s would blow away a name the user is mid-way through
    // typing into the rename field.
    if (stageView.kind === "project") {
      refreshProjectSessions();
      // §7.1: one more batched call, ONLY while the project page is up. Off
      // this page the snapshot is not requested at all, which is what keeps
      // §7.5's cost ceiling a property of the design rather than a hope.
      refreshOrchestration().catch((e) => console.error("orchestration", e));
      // §7.4: a stat-only probe on the same tick. It re-fetches the parsed
      // document set only when the (path, size, mtime) fingerprint moved, so an
      // ADR written while this page is open appears within one poll instead of
      // waiting for a manifest rev change or a manual Refresh.
      probeDocuments().catch((e) => console.error("documents", e));
    }
  } catch (e) {
    console.error("poll failed", e);
  }
}
poll();
setInterval(poll, 1500);

// ---- launcher modal ----
const MODELS = [["", "Default"], ["opus", "opus"], ["sonnet", "sonnet"], ["fable", "fable"]];
const MODES = [
  ["", "Default"], ["plan", "plan"], ["acceptEdits", "acceptEdits"],
  ["auto", "auto"], ["bypassPermissions", "bypassPermissions"],
];
function optionsHtml(pairs) {
  return pairs.map(([v, t]) => `<option value="${v}">${t}</option>`).join("");
}

// The launcher's target picker (§5): a project root, a single repo, or a
// multi-repo selection. In multi mode the PRIMARY is the topmost checked repo
// in LIST order, not the first one clicked — the same rule internal/ui's
// fan-out documents, so the two frontends cannot disagree about which repo a
// selection means.
async function openLauncher(preselect) {
  if (document.querySelector(".modal-backdrop")) return; // don't stack modals
  // Append the backdrop synchronously BEFORE any await, so a second rapid click
  // sees it via the guard above and can't stack a duplicate during the load.
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal" role="dialog" aria-label="New session">
      <h2>New session</h2>
      <div class="field">
        <label>Target</label>
        <div class="seg" id="f-seg">
          <button class="seg-b active" data-mode="single">One target</button>
          <button class="seg-b" data-mode="multi">Several repos</button>
        </div>
      </div>
      <div class="field" id="f-single-field">
        <label for="f-project">Project or repo</label>
        <select id="f-project"></select>
      </div>
      <div class="field" id="f-multi-field" hidden>
        <label>Repos <span class="f-primary" id="f-primary"></span></label>
        <div id="f-multi" class="fan-projects"></div>
      </div>
      <div class="field">
        <label for="f-model">Model</label>
        <select id="f-model">${optionsHtml(MODELS)}</select>
      </div>
      <div class="field">
        <label for="f-mode">Permission mode</label>
        <select id="f-mode">${optionsHtml(MODES)}</select>
      </div>
      <div class="field">
        <label for="f-seed">Seed prompt (optional)</label>
        <textarea id="f-seed" placeholder="Initial prompt or /slash-command"></textarea>
      </div>
      <div class="modal-error" id="f-error"></div>
      <div class="modal-actions">
        <button class="btn-ghost" id="f-cancel">Cancel</button>
        <button class="btn-launch" id="f-launch">Launch</button>
      </div>
    </div>`;
  document.body.appendChild(backdrop);

  const close = () => backdrop.remove();
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  backdrop.querySelector("#f-cancel").addEventListener("click", close);

  // Load targets and fill the (already-mounted) controls. The list is ordered
  // by the Go side; both the select and the checklist keep that order, which
  // is what makes "topmost checked" a stable, predictable rule.
  let targets = [];
  try {
    targets = await window.go.main.App.ListProjects();
  } catch (e) {
    console.error("ListProjects failed", e);
  }
  const optLabel = (t) => t.label + (t.isRoot ? " · project root" : "") + (t.missing ? " · missing" : "");
  backdrop.querySelector("#f-project").innerHTML = targets
    .map((t) => `<option value="${esc(t.path)}"${t.missing ? " disabled" : ""}${t.path === preselect ? " selected" : ""}>${esc(optLabel(t))}</option>`)
    .join("");
  // Missing targets stay listed but non-launchable (§7): they are dimmed here
  // for the same reason they stay in the rail — so the user can see what to
  // re-point instead of wondering where a repo went.
  backdrop.querySelector("#f-multi").innerHTML = targets
    .map((t) => `<label class="pf-check${t.missing ? " dimmed" : ""}"><input type="checkbox" data-path="${esc(t.path)}"${t.missing ? " disabled" : ""}${t.path === preselect ? " checked" : ""} /><span>${esc(optLabel(t))}</span></label>`)
    .join("");

  const singleField = backdrop.querySelector("#f-single-field");
  const multiField = backdrop.querySelector("#f-multi-field");
  const primaryEl = backdrop.querySelector("#f-primary");
  let picker = "single";

  // querySelectorAll is document order, so checked[0] IS the topmost checked
  // row — independent of the order the boxes were toggled in.
  const checkedPaths = () =>
    [...backdrop.querySelectorAll("#f-multi input:checked")].map((c) => c.getAttribute("data-path"));
  const labelFor = (p) => (targets.find((t) => t.path === p) || {}).label || p;
  const syncPrimary = () => {
    const c = checkedPaths();
    primaryEl.textContent = c.length ? `primary: ${labelFor(c[0])}` : "";
  };
  backdrop.querySelector("#f-multi").addEventListener("change", syncPrimary);
  syncPrimary();

  backdrop.querySelector("#f-seg").addEventListener("click", (e) => {
    const b = e.target.closest(".seg-b");
    if (!b) return;
    picker = b.getAttribute("data-mode");
    backdrop.querySelectorAll(".seg-b").forEach((x) => x.classList.toggle("active", x === b));
    singleField.hidden = picker !== "single";
    multiField.hidden = picker !== "multi";
  });

  const launchBtn = backdrop.querySelector("#f-launch");
  launchBtn.addEventListener("click", async () => {
    const model = backdrop.querySelector("#f-model").value;
    const mode = backdrop.querySelector("#f-mode").value;
    const seed = backdrop.querySelector("#f-seed").value;
    const errEl = backdrop.querySelector("#f-error");
    let path, addDirs = [];
    if (picker === "multi") {
      const c = checkedPaths();
      if (!c.length) { errEl.textContent = "Check at least one repo."; return; }
      path = c[0];
      addDirs = c.slice(1);
    } else {
      path = backdrop.querySelector("#f-project").value;
    }
    if (!path) { errEl.textContent = "Pick a project to launch."; return; }
    errEl.textContent = "";
    launchBtn.disabled = true;
    try {
      const name = await window.go.main.App.LaunchSession(path, model, mode, seed, addDirs);
      close();
      selectSession(name);
      poll();
    } catch (e) {
      errEl.textContent = "Launch failed: " + e;
      launchBtn.disabled = false;
    }
  });
}

// ---- command palette (⌘K) ----
function buildPaletteItems() {
  const items = [
    { label: "New session", hint: "launch", run: () => openLauncher() },
    { label: "New project", hint: "group repos into an initiative", run: () => openCreateProject() },
    { label: "Fan out to projects", hint: "one prompt · many repos", run: () => openFanout() },
    { label: "Workflows", hint: "runs & definitions", run: () => openWorkflows() },
    { label: "Search history", hint: "find past work", run: () => openSearch() },
  ];
  // Projects the §6 predicate would show. Hidden ones are deliberately absent:
  // the palette is an ambient surface and a hidden client's name must not
  // appear in one. They are reachable only through the explicit gesture below.
  for (const p of latestProjects) {
    if (p.ungrouped || !projectVisible(p)) continue;
    items.push({ label: p.name, hint: "project", run: () => openProject(p.root) });
  }
  if (latestProjects.some((p) => p.hidden)) {
    items.push({ label: "Hidden projects…", hint: "restore one", run: () => openHiddenProjects() });
  }
  for (const s of latestSessions) {
    items.push({
      label: displayName(s),
      hint: `${s.project} · ${statusWord(s.status)}`,
      run: () => selectSession(s.name),
    });
  }
  for (const s of latestRecent) {
    items.push({
      label: displayName(s),
      hint: `${s.project} · resume`,
      run: async () => {
        try { const nn = await window.go.main.App.ResumeSession(s.name); if (nn) selectSession(nn); poll(); }
        catch (e) { console.error("palette-resume", e); }
      },
    });
  }
  return items;
}

function openPalette() {
  if (document.querySelector(".modal-backdrop")) return;
  const items = buildPaletteItems();
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal palette" role="dialog" aria-label="Command palette">
      <input id="p-input" class="search-input" type="text" placeholder="Jump to a session or action…" autocomplete="off" spellcheck="false" />
      <ul id="p-list" class="palette-list"></ul>
    </div>`;
  document.body.appendChild(backdrop);

  const input = backdrop.querySelector("#p-input");
  const list = backdrop.querySelector("#p-list");
  input.focus();
  const close = () => backdrop.remove();
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });

  let filtered = items;
  let active = 0;

  function render() {
    list.replaceChildren();
    filtered.forEach((it, i) => {
      const li = document.createElement("li");
      li.className = "pitem" + (i === active ? " active" : "");
      li.innerHTML = `<span class="pi-label">${esc(it.label)}</span><span class="pi-hint">${esc(it.hint)}</span>`;
      li.addEventListener("mousemove", () => { if (active !== i) { active = i; paint(); } });
      li.addEventListener("click", () => run(i));
      list.appendChild(li);
    });
  }
  function paint() {
    [...list.children].forEach((li, i) => li.classList.toggle("active", i === active));
  }
  function run(i) {
    const it = filtered[i];
    if (!it) return;
    close();
    it.run();
  }

  input.addEventListener("input", () => {
    const q = input.value.trim().toLowerCase();
    filtered = q ? items.filter((it) => (it.label + " " + it.hint).toLowerCase().includes(q)) : items;
    active = 0;
    render();
  });
  input.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { e.preventDefault(); active = Math.min(active + 1, filtered.length - 1); paint(); scrollActive(list); }
    else if (e.key === "ArrowUp") { e.preventDefault(); active = Math.max(active - 1, 0); paint(); scrollActive(list); }
    else if (e.key === "Enter") { e.preventDefault(); run(active); }
    else if (e.key === "Escape") { e.preventDefault(); close(); }
  });
  render();
}

function scrollActive(list) {
  const el = list.querySelector(".pitem.active");
  if (el) el.scrollIntoView({ block: "nearest" });
}

// ---- search modal ----
function snippetHtml(s) {
  return esc(s).replace(/\u0001/g, "<b>").replace(/\u0002/g, "</b>");
}

function openSearch() {
  if (document.querySelector(".modal-backdrop")) return; // don't stack modals
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal search-modal" role="dialog" aria-label="Search history">
      <input id="s-input" class="search-input" type="text" placeholder="Search your session history…" autocomplete="off" spellcheck="false" />
      <ul id="s-results" class="search-results"></ul>
      <div id="s-hint" class="search-hint">Type to search past sessions — titles, asks, outcomes, and files.</div>
    </div>`;
  document.body.appendChild(backdrop);

  const input = backdrop.querySelector("#s-input");
  const results = backdrop.querySelector("#s-results");
  const hint = backdrop.querySelector("#s-hint");
  input.focus();

  const close = () => backdrop.remove();
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  input.addEventListener("keydown", (e) => { if (e.key === "Escape") close(); });

  let tid = null;
  input.addEventListener("input", () => {
    clearTimeout(tid);
    const q = input.value.trim();
    tid = setTimeout(async () => {
      if (!q) {
        results.replaceChildren();
        hint.textContent = "Type to search past sessions — titles, asks, outcomes, and files.";
        return;
      }
      let hits = [];
      try { hits = await window.go.main.App.SearchSessions(q); } catch (e) { console.error("search", e); }
      results.replaceChildren();
      hint.textContent = hits.length ? "" : "No matches.";
      for (const h of hits) {
        const li = document.createElement("li");
        li.className = "sresult";
        const label = (h.title && h.title.trim()) || (h.ask && h.ask.trim()) || "session";
        li.innerHTML =
          `<div class="sr-top"><span class="sr-title">${esc(label)}</span>` +
          (h.project ? `<span class="sr-proj">${esc(h.project)}</span>` : "") + `</div>` +
          (h.snippet ? `<div class="sr-snip">${snippetHtml(h.snippet)}</div>` : "");
        li.addEventListener("click", async () => {
          try {
            const nn = await window.go.main.App.ResumeSearchHit(h.sessionId, h.cwd);
            close();
            if (nn) selectSession(nn);
            poll();
          } catch (e) { console.error("resume-search", e); }
        });
        results.appendChild(li);
      }
    }, 200);
  });
}
