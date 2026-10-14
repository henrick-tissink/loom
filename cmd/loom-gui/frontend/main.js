import "./tokens.css";
import "@xterm/xterm/css/xterm.css";
import { statusColor, statusWord, xtermThemeFor } from "./theme.js";
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

// fmtAge renders a compact duration ("47m", "3h", "2d") from seconds.
function fmtAge(sec) {
  if (sec < 60) return sec + "s";
  if (sec < 3600) return Math.floor(sec / 60) + "m";
  if (sec < 86400) return Math.floor(sec / 3600) + "h";
  return Math.floor(sec / 86400) + "d";
}

// idleSuffix shows how long a session has sat idle, so a 3-minute idle and a
// 3-day one don't read identically. Blank for fresh (<1m) idles to avoid churn.
function idleSuffix(s) {
  if (s.status !== "idle" || !s.activity) return "";
  const age = Math.floor(Date.now() / 1000) - s.activity;
  if (age < 60) return "";
  return ` <span class="tage">${fmtAge(age)}</span>`;
}

// armAction turns a button into a two-step confirm (the killButton idiom) for a
// bulk/destructive action: first click arms and relabels, second click fires;
// it disarms itself after 2.5s.
function armAction(btn, label, fn) {
  if (!btn) return;
  const orig = btn.textContent;
  let armed = false, timer = null;
  btn.addEventListener("click", async (e) => {
    e.stopPropagation();
    if (!armed) {
      armed = true; btn.textContent = label; btn.classList.add("armed");
      timer = setTimeout(() => { armed = false; btn.textContent = orig; btn.classList.remove("armed"); }, 2500);
      return;
    }
    clearTimeout(timer); armed = false; btn.classList.remove("armed"); btn.textContent = orig;
    await fn();
  });
}

// fmtBytes renders a byte count as a compact human size.
function fmtBytes(n) {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)) + " " + u[i];
}

// confirmModal shows a modal with Cancel / <okLabel> and resolves to a boolean.
// body is trusted HTML (callers build it). Reused by the cleanup flows.
function confirmModal(title, body, okLabel) {
  return new Promise((resolve) => {
    if (document.querySelector(".modal-backdrop")) return resolve(false);
    const bd = document.createElement("div");
    bd.className = "modal-backdrop";
    bd.innerHTML =
      `<div class="modal" role="dialog" aria-label="${esc(title)}"><h2>${esc(title)}</h2>` +
      `<div class="cm-body">${body}</div>` +
      `<div class="modal-actions"><button class="btn-ghost" id="cm-no">Cancel</button>` +
      `<button class="btn-launch" id="cm-yes">${esc(okLabel)}</button></div></div>`;
    document.body.appendChild(bd);
    const done = (v) => { document.removeEventListener("keydown", onEsc, true); bd.remove(); resolve(v); };
    const onEsc = (e) => { if (e.key === "Escape") done(false); };
    document.addEventListener("keydown", onEsc, true);
    bd.addEventListener("click", (e) => { if (e.target === bd) done(false); });
    bd.querySelector("#cm-no").addEventListener("click", () => done(false));
    bd.querySelector("#cm-yes").addEventListener("click", () => done(true));
  });
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
    `<span class="tright"><span class="tstatus" style="color:${color}">${esc(statusWord(s.status))}${idleSuffix(s)}</span><span class="tactions"></span></span>`;
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

// projectMissingRepos counts a project's MEMBER repos that have vanished from
// disk — surfaced in the rail so a deleted sub-repo isn't silent until you open
// the overview. (A missing project ROOT is a separate, already-shown signal.)
function projectMissingRepos(root) {
  const p = latestProjects.find((x) => x.root === root);
  if (!p || !p.repos) return 0;
  return p.repos.filter((r) => r.missing).length;
}

function appendSectionHead(sec) {
  const li = document.createElement("li");
  const isCollapsed = collapsedRoots.has(sec.root);
  const urgent = sec.live.filter((s) => s.status === "needs_you").length;
  const miss = projectMissingRepos(sec.root);
  li.className = "psec" + (isCollapsed ? " collapsed" : "") + (urgent ? " urgent" : "");
  li.innerHTML =
    `<button class="psec-caret" title="${isCollapsed ? "Expand" : "Collapse"}">${isCollapsed ? "▸" : "▾"}</button>` +
    `<span class="psec-name">${esc(sec.name)}</span>` +
    (miss ? `<span class="psec-warn" title="${miss} member repo${miss === 1 ? "" : "s"} missing">!</span>` : "") +
    // The dot survives collapse on purpose: collapsing a section must never be
    // the reason an urgent session goes unnoticed.
    (urgent ? `<span class="psec-dot" title="${urgent} need you"></span>` : "") +
    `<span class="psec-count">${sec.live.length || ""}</span>` +
    `<button class="psec-add" title="New session in ${esc(sec.name)}">+</button>`;
  li.querySelector(".psec-caret").addEventListener("click", (e) => {
    e.stopPropagation();
    setCollapsed(sec.root, !collapsedRoots.has(sec.root));
    renderRail(latestSessions, latestRecent);
  });
  li.querySelector(".psec-add").addEventListener("click", (e) => { e.stopPropagation(); openLauncher(sec.root); });
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
    const miss = projectMissingRepos(p.root);
    li.className = "prow" + (p.missing ? " missing" : "");
    li.innerHTML = `<span class="prow-name">${esc(p.name)}</span>` +
      (miss ? `<span class="psec-warn" title="${miss} member repo${miss === 1 ? "" : "s"} missing">!</span>` : "") +
      (p.missing ? `<span class="prow-tag">missing</span>` : "") +
      `<button class="psec-add prow-add" title="New session here">+</button>`;
    li.addEventListener("click", () => openProject(p.root));
    li.querySelector(".prow-add").addEventListener("click", (e) => { e.stopPropagation(); openLauncher(p.root); });
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
// Clicking a rail section header replaces the stage with this.
function projectByRoot(root) {
  return latestProjects.find((p) => p.root === root) || null;
}

function openProject(root) {
  teardownTerminal();
  activeName = null;
  selProject = root; // keep the projects pane in sync (openProject has many callers)
  stageView = { kind: "project", root };
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
       <span class="po-sstatus">${kind === "live" ? esc(statusWord(s.status)) + idleSuffix(s) : esc(s.status)}</span>
     </li>`;
  const block = (title, rows, action) =>
    `<div class="po-sub"><span>${title}</span>${action || ""}</div>` +
    (rows.length ? `<ul class="po-list">${rows.join("")}</ul>` : `<div class="po-empty">none</div>`);
  const killAll = live.length ? `<button class="po-subact" id="po-killall">Kill all</button>` : "";
  const clearFin = fin.length ? `<button class="po-subact" id="po-clearfin">Clear finished</button>` : "";
  return block("Live", live.map((s) => row(s, "live")), killAll) +
    block("Finished", fin.map((s) => row(s, "finished")), clearFin);
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
  const root = stageView.root;
  armAction(host.querySelector("#po-killall"), "Kill all?", async () => {
    for (const s of latestSessions.filter((s) => (s.projectRoot || "") === root)) {
      try { await window.go.main.App.KillSession(s.name); } catch (e) { console.error("killall", e); }
    }
    poll();
  });
  armAction(host.querySelector("#po-clearfin"), "Clear all?", async () => {
    for (const s of latestRecent.filter((s) => (s.projectRoot || "") === root)) {
      try { await window.go.main.App.DismissSession(s.name); } catch (e) { console.error("clearfin", e); }
    }
    poll();
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
        <button class="tact po-reveal"${r.missing ? " disabled" : ""} title="Reveal in Finder">reveal</button>
        <button class="tact po-copy" title="Copy path">copy</button>
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
        <button class="sh-btn" id="po-clean">Clean repo</button>
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
    </div>`;

  refreshProjectSessions();

  const ungrouped = p.ungrouped;
  const on = (id, fn) => { const el = document.getElementById(id); if (el) el.addEventListener("click", fn); };
  // Ungrouped is a real row in the model, but it owns no directory: renaming,
  // re-pointing or adding repos to it is meaningless, and soloing it is
  // explicitly suppressed by §6.1.
  for (const id of ["po-solo", "po-rename", "po-repoint", "po-addrepo"]) {
    const el = document.getElementById(id);
    if (el && ungrouped) el.disabled = true;
  }
  on("po-clean", () => repoCleanupDialog(p.root));
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
    const reveal = li.querySelector(".po-reveal");
    if (reveal && !reveal.disabled) reveal.addEventListener("click", () =>
      window.go.main.App.RevealInFinder(path).catch((e) => console.error("reveal", e)));
    li.querySelector(".po-copy").addEventListener("click", async (e) => {
      const b = e.currentTarget;
      try {
        await navigator.clipboard.writeText(path);
        const t = b.textContent; b.textContent = "copied"; setTimeout(() => { b.textContent = t; }, 1200);
      } catch (err) { console.error("copy", err); }
    });
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
// in LIST order, not the first one clicked.
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

// repoCleanupDialog previews git-ignored build junk in a project's repos and
// deletes it on confirm.
async function repoCleanupDialog(root) {
  const fn = bound("RepoCleanupPreview");
  let d = { entries: [], total: 0 };
  if (fn) { try { d = (await fn(root)) || d; } catch (e) { console.error("repo scan", e); } }
  const entries = d.entries || [];
  if (!entries.length) { await confirmModal("Clean repo", `<div class="cln-empty">Nothing to clean — no build junk found.</div>`, "OK"); return; }
  const rows = entries.map((e) => `<div class="cln-file"><span class="cln-fp">${esc(e.rel)}${e.isDir ? "/" : ""}</span><span class="cln-fs">${fmtBytes(e.size)}</span></div>`).join("");
  const body = `<div class="cln-note">Only git-ignored build junk — tracked files and configs like <code>.env</code> are never touched.</div><div class="cln-files">${rows}</div>`;
  const ok = await confirmModal("Clean repo", body, `Delete ${entries.length} · ${fmtBytes(d.total)}`);
  if (!ok) return;
  const rfn = bound("RepoCleanupRun");
  try {
    const freed = rfn ? await rfn(root, entries.map((e) => e.abs)) : 0;
    await confirmModal("Cleaned", `<div class="cln-empty">Freed ${fmtBytes(freed)}.</div>`, "Done");
  } catch (e) { console.error("repo clean", e); await confirmModal("Cleanup failed", `<div class="cln-empty">Could not remove some paths.</div>`, "OK"); }
}
