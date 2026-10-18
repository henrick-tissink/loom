// Bundled variable fonts (Manrope for the chrome, JetBrains Mono for names,
// paths and the terminal): the same look offline, and no remote request.
import "@fontsource-variable/manrope";
import "@fontsource-variable/jetbrains-mono";
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
// Dock by default (Islands): the named list is an explicit expand.
try { const v = localStorage.getItem(PP_KEY); manualCollapsed = v == null ? true : v === "1"; } catch { manualCollapsed = true; }
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
// The terminal's face, and a load started at boot so it is usually in hand
// before the first session opens (@font-face is lazy otherwise).
const MONO_FACE = '"JetBrains Mono Variable"';
const monoReady = Promise.all([400, 600].map((w) => document.fonts.load(`${w} 13px ${MONO_FACE}`))).catch(() => {});
let termThemeMode = "dark"; // "dark" | "light", from preferences (themes the whole chrome)

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
  termThemeMode = mode === "light" ? "light" : "dark";
  document.documentElement.setAttribute("data-term-theme", termThemeMode);
  if (term) term.options.theme = xtermThemeFor(termThemeMode);
}

// Load the persisted terminal theme before the first session is opened.
// Guarded: the Wails binding may not be injected yet at module-eval time.
const _getPrefs = window.go && window.go.main && window.go.main.App && window.go.main.App.GetPrefs;
if (_getPrefs) {
  _getPrefs().then((p) => applyTermTheme(p && p.terminalTheme)).catch(() => {});
}

// The capsule at the top of the threads island: how many need you and how
// long the oldest has waited. Hidden while the queue itself is on screen.
function renderAttention(sessions) {
  const q = sessions.filter((s) => s.status === "needs_you");
  const n = q.length;
  attnEl.classList.toggle("here", selProject === ATTENTION);
  if (n > 0) {
    const now = Math.floor(Date.now() / 1000);
    const oldest = q.reduce((m, s) => (s.since && (!m || s.since < m) ? s.since : m), 0);
    const sub = oldest ? `Oldest has waited ${fmtAge(Math.max(0, now - oldest))}` : "Open the queue";
    attnEl.innerHTML = `<span class="attn-dot"></span><span class="attn-body"><span>${n} ${n === 1 ? "needs" : "need"} you</span><span class="attn-sub">${sub}</span></span><span class="attn-key">↵ reply</span>`;
  } else {
    attnEl.textContent = "";
  }
}
attnEl.addEventListener("click", () => openAttention());

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

// waitingSuffix names what the pane waits on when claude's own registry says
// so ("dialog open", "shell"); a bare needs-you tells you less than the thing
// you have to answer.
function waitingSuffix(s) {
  if (!s.waitingFor) return "";
  return ` <span class="tage">${esc(s.waitingFor)}</span>`;
}

// taskPillHtml is the finish line from the session's task file: Done-when
// ticks for a delivery task, a word for a consultation, nothing when the
// session never pinned one — which is itself information. It is what tells
// "asked a question mid-task" (2/6) apart from "finished, waiting for you to
// read the hand-back" (4/4) without opening the pane.
function taskPillHtml(s) {
  if (!s.taskStatus) return "";
  const tip = s.taskOutcome ? ` title="${esc(s.taskOutcome)}"` : "";
  if (s.taskStatus === "consultation") return `<span class="tpill tpill-consult"${tip}>consult</span>`;
  const complete = s.taskStatus === "done" || (s.taskTotal > 0 && s.taskDone >= s.taskTotal);
  const label = s.taskTotal > 0 ? `${s.taskDone}/${s.taskTotal}` : s.taskStatus;
  // The stop guard's verdict on the file's current version: ✓ when it ran
  // the `$` checks and they passed, ✗ when its last word was a failed check.
  const mark = s.verified ? ` <span class="tmark ok" title="The stop guard ran the Done-when checks on this version and they passed">✓</span>` : s.checksFailed ? ` <span class="tmark bad" title="The stop guard's last verdict: a Done-when check failed">✗</span>` : "";
  return `<span class="tpill${complete ? " tpill-ok" : ""}${s.checksFailed ? " tpill-bad" : ""}"${tip}>${esc(label)}${mark}</span>`;
}

// askLineHtml is the question a needs-you row is waiting on, with how long
// it has waited — so the queue reads as questions, not as ten identical
// "needs you" rows, and the reply modal is not typed blind.
function askLineHtml(s) {
  if (s.status !== "needs_you") return "";
  const age = s.since ? fmtAge(Math.max(0, Math.floor(Date.now() / 1000) - s.since)) : "";
  if (!s.ask && !age) return "";
  return `<span class="task-ask">${age ? `<span class="tage">${age}</span> ` : ""}${esc(sumPreview(s.ask || ""))}</span>`;
}

// budgetChipHtml marks a session past the context budget and says whether a
// hand-over checkpoint exists and how old it is: "can this one be handed
// over safely" is answered in the cockpit instead of inside the pane. The
// SessionStart hook re-injects a cwd's checkpoint only within 24 h, so the
// age is the point.
function budgetChipHtml(s) {
  if (!s.ctxTokens || s.ctxTokens < CTX_WINDOW) return "";
  const ck = s.checkpointAt ? `ckpt ${fmtAge(Math.max(0, Math.floor(Date.now() / 1000) - s.checkpointAt))}` : "no checkpoint";
  const tip = s.nextStep ? ` title="Next step: ${esc(s.nextStep)}"` : "";
  return `<span class="tchip tchip-over"${tip}>over budget · ${esc(ck)}</span>`;
}
function overBudget(s) { return !!s.ctxTokens && s.ctxTokens >= CTX_WINDOW; }

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
  return "session " + s.name.replace(/^(loom|ext)-/, "").slice(0, 8);
}

// externalTag marks a claude running in a terminal outside loom: loom sees
// its status, question and finish line, but has no pane for it.
function externalTag(s) {
  return s.external ? `<span class="tpill tpill-ext" title="Running in a terminal outside loom${s.cwd ? " — " + esc(s.cwd) : ""}">terminal</span>` : "";
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

// ---- live rail: rows survive the poll ----
// Both nav panes are rebuilt from the DTOs on every 1.5s poll. Swapping the
// whole list in each time threw away hover, restarted every animation and
// made motion impossible, so the rebuild now goes into a detached list and
// liveCommit swaps in only the rows whose content changed (matched by
// data-key, compared by data-sig). Rows that moved slide from where they
// were; rows that are new fade in. Click handlers close over a session's
// name only, and everything else they read is in the row's HTML, so keeping
// an unchanged node is safe.
const reduceMotion = window.matchMedia ? window.matchMedia("(prefers-reduced-motion: reduce)") : { matches: false };
const MOVE_MS = 240, ENTER_MS = 180, FLASH_MS = 1800, BREATHE_MS = 2400;

// Where the append* helpers put rows: the live list, or a detached one while
// renderThreadsPane builds the next state.
let rowsTarget = threadsEl;
// The view the threads list last drew (a project root, or the Attention queue).
let threadsView = null;

// seal records a row's identity and content; call it once the row is built.
function seal(el, key) {
  el.dataset.key = key;
  el.dataset.sig = el.className + "\u0001" + (el.style.getPropertyValue("--tc") || "") + "\u0001" + (el.title || "") + "\u0001" + el.innerHTML;
  return el;
}

// phase locks a looping animation to one wall clock, so a row replaced
// mid-cycle carries on where the old one was instead of restarting. Applied
// after seal: the inline delay changes every poll and must not count as
// content.
function phase(el, periodMs) { if (el) el.style.animationDelay = `-${Math.round(performance.now() % periodMs)}ms`; }

function liveCommit(parent, next, animate) {
  const old = new Map();
  for (const c of parent.children) if (c.dataset.key) old.set(c.dataset.key, c);
  const tops = new Map();
  if (animate) for (const [k, c] of old) tops.set(k, c.getBoundingClientRect().top);
  const want = [];
  for (const n of [...next.children]) {
    const k = n.dataset.key, o = k ? old.get(k) : null;
    want.push(o && o.dataset.sig === n.dataset.sig ? o : n);
    if (o) old.delete(k);
  }
  const keep = new Set(want);
  for (const c of [...parent.children]) if (!keep.has(c)) c.remove();
  want.forEach((n, i) => { if (parent.children[i] !== n) parent.insertBefore(n, parent.children[i] || null); });
  if (!animate || reduceMotion.matches) return;
  for (const n of want) {
    const k = n.dataset.key;
    if (!k || typeof n.animate !== "function") continue;
    if (tops.has(k)) {
      const dy = tops.get(k) - n.getBoundingClientRect().top;
      if (Math.abs(dy) > 1) n.animate([{ transform: `translateY(${dy}px)` }, { transform: "none" }], { duration: MOVE_MS, easing: "cubic-bezier(0.2, 0.8, 0.2, 1)" });
    } else {
      n.animate([{ opacity: 0, transform: "translateY(-4px)" }, { opacity: 1, transform: "none" }], { duration: ENTER_MS, easing: "ease-out" });
    }
  }
}

// A session that turns needs-you glows once. Transitions are found by
// comparing with the statuses the rail last drew; the first render has
// nothing to compare against, so a restart does not light up the queue.
let railStatus = null;
const flashAt = new Map(); // name -> performance.now() when it turned needs-you

function noteTransitions() {
  const t = performance.now();
  const prev = railStatus;
  railStatus = new Map(latestSessions.map((s) => [s.name, s.status]));
  for (const [n, at] of flashAt) if (t - at > FLASH_MS) flashAt.delete(n);
  if (!prev) return;
  for (const [n, st] of railStatus) {
    if (st === "needs_you" && prev.has(n) && prev.get(n) !== "needs_you") flashAt.set(n, t);
  }
}

function applyFlashes(parent) {
  const t = performance.now();
  for (const el of parent.children) {
    const at = flashAt.get(el.dataset.key);
    if (at == null || el.classList.contains("flash")) continue;
    el.style.animationDelay = `-${Math.round(t - at)}ms`;
    el.classList.add("flash");
    el.addEventListener("animationend", function done(e) {
      if (e.animationName !== "loom-flash") return;
      el.classList.remove("flash");
      el.style.animationDelay = "";
      el.removeEventListener("animationend", done);
    });
  }
}

function appendGroup(label) {
  const gh = document.createElement("li");
  gh.className = "group";
  gh.textContent = label;
  rowsTarget.appendChild(seal(gh, "g:" + label));
}

// emptyRow puts a keyed placeholder in the list being built.
function emptyRow(html) {
  rowsTarget.innerHTML = html;
  if (rowsTarget.firstElementChild) seal(rowsTarget.firstElementChild, "empty");
}

// projLabel overrides the sub-label (the repo basename) — the Attention queue
// mixes projects, so it shows each thread's PROJECT there instead.
function appendLiveRow(s, projLabel) {
  const li = document.createElement("li");
  li.className = "thread" + (s.name === activeName ? " active" : "") + (s.external ? " external" : "");
  const color = statusColor(s.status);
  li.style.setProperty("--tc", color);
  li.innerHTML =
    `<span class="tglyph i-${esc(s.status)}">${icon(STATUS_ICON[s.status] || STATUS_ICON.unknown, 3)}</span>` +
    `<span class="tinfo"><span class="tname">${esc(displayName(s))}</span><span class="tproj">${esc(projLabel != null ? projLabel : s.project)}${externalTag(s)}${restartTag(s)}${taskPillHtml(s)}${heldChip(s)}</span>${askLineHtml(s)}${ctxGaugeHtml(s.ctxTokens)}${budgetChipHtml(s)}</span>` +
    `<span class="tright"><span class="tstatus" style="color:${color}">${esc(statusWord(s.status))}${waitingSuffix(s)}${idleSuffix(s)}</span><span class="tactions"></span></span>`;
  li.addEventListener("click", () => selectSession(s.name));
  rowsTarget.appendChild(li);
  // No pane, so nothing to reply into, hand over from, or kill: the row is
  // information only, and its stage says where the claude actually runs.
  if (s.external) { sealLive(li, s); return; }
  const acts = li.querySelector(".tactions");
  acts.appendChild(actionBtn('<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>', "Quick reply", () => openReply(s.name)));
  if (overBudget(s)) {
    acts.appendChild(actionBtn('<path d="M12 5v14M5 12h14"/>', "Fresh session from checkpoint (same directory; this one stays open)", async () => {
      try { const nn = await window.go.main.App.FreshSession(s.name); if (nn) selectSession(nn); poll(); }
      catch (e) { console.error("fresh", e); }
    }));
  }
  acts.appendChild(killButton(s.name));
  sealLive(li, s);
}

function sealLive(li, s) {
  seal(li, s.name);
  if (s.status === "running") phase(li.querySelector(".tglyph"), BREATHE_MS);
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
  // No LLM summary (the common case: it costs quota and is opt-in) → the
  // outcome the indexer extracted, so a finished row reads as what it ended with.
  else if (s.outcome) sumLine = `<span class="tsum" title="${esc(s.outcome)}">${esc(sumPreview(s.outcome))}</span>`;
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
  // Keyed by name like its live row, so a session that ends slides down
  // into Finished instead of vanishing and reappearing.
  rowsTarget.appendChild(seal(li, s.name));
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
  if (btn) { btn.textContent = on ? "›" : "‹"; btn.title = on ? "Expand projects" : "Collapse projects"; btn.setAttribute("aria-label", btn.title); }
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
  const out = document.createElement("ul");

  if (collapsed) {
    const a = document.createElement("li");
    a.className = "pj-tok" + (selProject === ATTENTION ? " on" : "");
    a.title = "Attention" + (need ? ` — ${need} need you` : "");
    a.innerHTML = `<span class="attn-dot"></span>` + (need ? `<span class="pj-badge">${need}</span>` : "");
    a.addEventListener("click", openAttention);
    out.appendChild(seal(a, "attn"));
    for (const p of projects) {
      const li = document.createElement("li");
      li.className = "pj-tok" + (selProject === p.root ? " on" : "") + (p.ung ? " ung" : "");
      li.title = p.name;
      li.innerHTML = (p.ung ? "·" : `<span>${esc(initials(p.name))}</span>`) +
        (p.need ? `<span class="pj-badge">${p.need}</span>` : (p.run ? `<span class="pj-tdot"></span>` : ""));
      li.addEventListener("click", () => selectProject(p.root));
      li.addEventListener("contextmenu", (e) => { e.preventDefault(); projectMenu(p, e.clientX, e.clientY); });
      out.appendChild(seal(li, "tok:" + p.root));
      phase(li.querySelector(".pj-tdot"), BREATHE_MS);
    }
    liveCommit(projectsEl, out, false);
    return;
  }

  const a = document.createElement("li");
  a.className = "pj-attn" + (selProject === ATTENTION ? " on" : "");
  a.innerHTML = `<span class="attn-dot"></span><span class="pj-name">Attention</span>` + (need ? `<span class="pj-count">${need}</span>` : "");
  a.addEventListener("click", openAttention);
  out.appendChild(seal(a, "attn"));
  const div = document.createElement("li"); div.className = "pj-div"; out.appendChild(seal(div, "div"));
  for (const p of projects) {
    const li = document.createElement("li");
    li.className = "pj-row" + (selProject === p.root ? " on" : "") + (p.missing ? " missing" : "") + (p.ung ? " ung" : "");
    li.innerHTML = `<span class="pj-name">${esc(p.name)}</span><span class="pj-meta">` +
      (p.run && !p.need ? `<span class="pj-run"></span>` : "") +
      (p.need ? `<span class="pj-count">${p.need}</span>` : "") + `</span>`;
    li.addEventListener("click", () => selectProject(p.root));
    li.addEventListener("contextmenu", (e) => { e.preventDefault(); projectMenu(p, e.clientX, e.clientY); });
    out.appendChild(seal(li, "row:" + p.root));
    phase(li.querySelector(".pj-run"), BREATHE_MS);
  }
  liveCommit(projectsEl, out, false);
}

// ---- threads pane (2026-07-24 three-pane nav) ----

function renderThreadsPane() {
  // The attention capsule is redundant while the queue itself is on screen.
  attnEl.classList.toggle("here", selProject === ATTENTION);
  if (!threadsHeadEl || !threadsEl) return;
  noteTransitions();
  // Motion only within one view: switching project is a new list, not a move.
  const view = String(selProject);
  const animate = view === threadsView;
  threadsView = view;
  const next = document.createElement("ul");
  rowsTarget = next;
  try { buildThreads(); } finally { rowsTarget = threadsEl; }
  liveCommit(threadsEl, next, animate);
  applyFlashes(threadsEl);
}

function buildThreads() {
  if (selProject === ATTENTION) {
    // Freshest ask first: the one you were just working with is at the top,
    // and anything that has waited a day or more sinks under Stale rather
    // than sitting between live questions as if it were one. `since` only
    // changes on a new ask, so rows do not shuffle between polls.
    const now = Math.floor(Date.now() / 1000);
    const queue = latestSessions.filter((s) => s.status === "needs_you").slice()
      .sort((a, b) => (b.since || 0) - (a.since || 0));
    const stale = queue.filter((s) => s.since && now - s.since >= 86400);
    const fresh = queue.filter((s) => !stale.includes(s));
    threadsHeadEl.innerHTML =
      `<div class="th-head"><span class="th-title"><span class="attn-dot"></span><span>Needs you</span></span></div>` +
      `<div class="th-sub"><span>${queue.length} across all projects${stale.length ? ` · ${stale.length} stale` : ""}</span></div>`;
    for (const s of fresh) appendLiveRow(s, s.projectName || "");
    if (stale.length) { appendGroup("Stale · a day or more"); for (const s of stale) appendLiveRow(s, s.projectName || ""); }
    if (!queue.length) emptyRow(`<div class="th-empty">Nothing needs you right now.<br><span>Walk away — loom pulls you back.</span></div>`);
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
  if (!live.length && !fin.length) emptyRow(`<div class="th-empty">No threads in ${esc(name)} yet.<br><span>Press + New to start one.</span></div>`);
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
  // Chips restate what the row shows (finish line, budget) so the header is
  // a readout on its own; the actions are Diff, Fresh session (only when the
  // session is over budget) and Reply, the primary one.
  const wait = s && s.status === "needs_you" && s.since ? ` · ${fmtAge(Math.max(0, Math.floor(Date.now() / 1000) - s.since))}` : "";
  const task = s && s.taskStatus ? `<span class="sh-chip">task ${s.taskStatus === "consultation" ? "consult" : s.taskTotal > 0 ? `${s.taskDone}/${s.taskTotal}` : esc(s.taskStatus)}</span>` : "";
  const ctx = s && s.ctxTokens ? `<span class="sh-chip${overBudget(s) ? " over" : ""}">${Math.round(s.ctxTokens / 1000)}k${overBudget(s) ? " · over budget" : ""}</span>` : "";
  el.innerHTML =
    `<span class="sh-name">${esc(label)}</span>` +
    (project ? `<span class="sh-proj">${icon(FOLDER_ICON, 2)}${esc(project)}</span>` : "") +
    `<span class="sh-pill"><i style="background:${color}"></i><span style="color:${color}">${esc(statusWord(status))}${wait}</span></span>` +
    task + ctx + hooksChip(s) + (needsRestart(s) ? `<span class="sh-chip over" title="Started before the hooks were last changed; it keeps the previous hooks until restarted">restart for new hooks</span>` : "") +
    (s && s.external ? "" :
      `<span class="sh-acts">` +
      `<button class="sh-btn" id="sh-diff" title="Review uncommitted changes">${icon('<circle cx="6" cy="6" r="2.4"/><circle cx="6" cy="18" r="2.4"/><circle cx="18" cy="9" r="2.4"/><path d="M6 8.4v7.2M18 11.4v.6a4 4 0 0 1-4 4H8.4"/>', 2)}Diff</button>` +
      (s && overBudget(s) ? `<button class="sh-btn" id="sh-fresh" title="Fresh session from checkpoint (same directory; this one stays open)">Fresh session</button>` : "") +
      `<button class="sh-btn sh-primary" id="sh-reply" title="Quick reply (R)">Reply</button></span>`);
  const diffBtn = el.querySelector("#sh-diff");
  if (diffBtn) diffBtn.addEventListener("click", () => openDiff(name));
  const hooksBtn = el.querySelector("#sh-hooks");
  if (hooksBtn) hooksBtn.addEventListener("click", () => openHooksPanel(name));
  const replyBtn = el.querySelector("#sh-reply");
  if (replyBtn) replyBtn.addEventListener("click", () => openReply(name));
  const freshBtn = el.querySelector("#sh-fresh");
  if (freshBtn) freshBtn.addEventListener("click", async () => {
    try { const nn = await window.go.main.App.FreshSession(name); if (nn) selectSession(nn); poll(); }
    catch (e) { console.error("fresh", e); }
  });
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

// removeProjectFlow is the one path out of the dock: a confirm that says what
// goes (live sessions killed, finished ones cleared, the project forgotten and
// kept out of discovery), then App.RemoveProject and a jump to Attention,
// since the project the stage was showing no longer exists.
async function removeProjectFlow(root) {
  const p = projectByRoot(root);
  if (!p || p.ungrouped) return;
  const live = latestSessions.filter((s) => (s.projectRoot || "") === root && !s.external).length;
  const fin = latestRecent.filter((s) => (s.projectRoot || "") === root).length;
  const parts = [];
  if (live) parts.push(`kill <b>${live}</b> live session${live === 1 ? "" : "s"}`);
  if (fin) parts.push(`clear <b>${fin}</b> finished`);
  const what = parts.length ? `This will ${parts.join(" and ")}, then forget ` : "This will forget ";
  const ok = await confirmModal("Remove project?",
    `<p>${what}<b>${esc(p.name)}</b> and its repos. Discovery will not add it back; ` +
    `use <b>New project</b> at the same directory to bring it back.</p>`, "Remove");
  if (!ok) return;
  try { await window.go.main.App.RemoveProject(root); }
  catch (e) { console.error("remove project", e); flashProjectError(String(e)); return; }
  await poll();
  if (selProject === root) openAttention();
}

// projectMenu is the dock token's right-click: Open, Hide/Show, Remove. One
// menu at a time; any click or Escape outside it closes it.
function projectMenu(p, x, y) {
  closeProjectMenu();
  const m = document.createElement("div");
  m.className = "ctxmenu";
  m.id = "ctxmenu";
  const items = [["Open", () => selectProject(p.root)]];
  if (!p.ung) {
    // The dock's row objects carry counts, not flags; the hidden bit lives on
    // the project detail.
    const hidden = !!(projectByRoot(p.root) || {}).hidden;
    items.push(["Brief…", () => openBrief(p.root)]);
    items.push([hidden ? "Show" : "Hide", () => projectAction(() => window.go.main.App.SetProjectHidden(p.root, !hidden))]);
    items.push(["Remove…", () => removeProjectFlow(p.root), "danger"]);
  }
  for (const [label, fn, cls] of items) {
    const b = document.createElement("button");
    b.textContent = label;
    if (cls) b.classList.add(cls);
    b.addEventListener("click", () => { closeProjectMenu(); fn(); });
    m.appendChild(b);
  }
  document.body.appendChild(m);
  const r = m.getBoundingClientRect();
  m.style.left = Math.min(x, window.innerWidth - r.width - 8) + "px";
  m.style.top = Math.min(y, window.innerHeight - r.height - 8) + "px";
  setTimeout(() => {
    document.addEventListener("click", closeProjectMenu, { once: true, capture: true });
    document.addEventListener("contextmenu", closeProjectMenu, { once: true, capture: true });
    document.addEventListener("keydown", onMenuKey, true);
  }, 0);
}
function onMenuKey(e) { if (e.key === "Escape") closeProjectMenu(); }
function closeProjectMenu() {
  const m = document.getElementById("ctxmenu");
  if (m) m.remove();
  document.removeEventListener("keydown", onMenuKey, true);
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
        <button class="sh-btn sh-primary" id="po-brief" title="A deep account of what was done here, written by Claude">Brief</button>
        <button class="sh-btn" id="po-hide">${p.hidden ? "Show" : "Hide"}</button>
        <button class="sh-btn" id="po-solo">${p.solo ? "Leave solo" : "Solo"}</button>
        <button class="sh-btn" id="po-rename">Rename</button>
        <button class="sh-btn" id="po-repoint">Re-point</button>
        <button class="sh-btn" id="po-clean">Clean repo</button>
        <button class="sh-btn sh-danger" id="po-removeproj" title="Kill its sessions, forget the project, keep it out of the dock">Remove</button>
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
  for (const id of ["po-brief", "po-solo", "po-rename", "po-repoint", "po-addrepo", "po-removeproj"]) {
    const el = document.getElementById(id);
    if (el && ungrouped) el.disabled = true;
  }
  on("po-clean", () => repoCleanupDialog(p.root));
  on("po-brief", () => openBrief(p.root));
  on("po-removeproj", () => removeProjectFlow(p.root));
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

// ---- project brief (internal/brief) ----
//
// A project's study brief: pick a window, see what evidence it would be
// written from, have Claude write it (minutes, Opus), then read it here with
// "Ask about this" / "Review" on every task, or open it as a page. Writing
// is never automatic; it uses quota, so the estimate comes first.

const briefUI = { root: null, window: "24h", est: null, estErr: "", estGen: 0, list: [], doc: null, docId: null, running: [], started: {}, err: "" };
const BRIEF_WINDOWS = [["24h", "24 hours"], ["3d", "3 days"], ["5d", "5 days"], ["1w", "1 week"]];
let briefTimer = null;

function openBrief(root, id) {
  teardownTerminal();
  activeName = null;
  selProject = root;
  stageView = { kind: "brief", root };
  if (briefUI.root !== root) Object.assign(briefUI, { root, est: null, estErr: "", list: [], doc: null, docId: null, running: [], started: {}, err: "" });
  if (id) briefUI.docId = id;
  renderBrief();
  renderRail();
  refreshBriefList(true);
  estimateBrief();
}

async function refreshBriefList(openNewest) {
  const root = briefUI.root;
  try {
    const [list, running] = await Promise.all([window.go.main.App.BriefList(root), window.go.main.App.BriefRunning(root)]);
    if (stageView.kind !== "brief" || stageView.root !== root) return;
    briefUI.list = list || [];
    briefUI.running = running || [];
    if (openNewest && !briefUI.docId && briefUI.list.length) briefUI.docId = briefUI.list[0].id;
  } catch (e) { briefUI.err = String(e); }
  renderBriefSide();
  if (briefUI.docId && (!briefUI.doc || briefUI.doc.meta.id !== briefUI.docId)) loadBriefDoc(briefUI.docId);
  else if (!briefUI.docId) renderBriefReader();
}

async function estimateBrief() {
  const root = briefUI.root, win = briefUI.window, gen = ++briefUI.estGen;
  briefUI.est = null; briefUI.estErr = "";
  renderBriefSide();
  try {
    const est = await window.go.main.App.BriefEstimate(root, win);
    if (gen !== briefUI.estGen) return;
    briefUI.est = est;
  } catch (e) {
    if (gen !== briefUI.estGen) return;
    briefUI.estErr = String(e);
  }
  renderBriefSide();
}

async function writeBrief() {
  const root = briefUI.root, win = briefUI.window;
  if (briefUI.running.includes(win)) return;
  briefUI.running = [...briefUI.running, win];
  briefUI.started[win] = Date.now();
  briefUI.err = "";
  renderBriefSide();
  try {
    const m = await window.go.main.App.BriefGenerate(root, win);
    if (stageView.kind === "brief" && stageView.root === root) briefUI.docId = m.id;
  } catch (e) {
    if (stageView.kind === "brief" && stageView.root === root) briefUI.err = "Writing failed: " + e;
  }
  briefUI.running = briefUI.running.filter((w) => w !== win);
  delete briefUI.started[win];
  if (stageView.kind === "brief" && stageView.root === root) { await refreshBriefList(false); estimateBrief(); }
}

async function loadBriefDoc(id) {
  briefUI.docId = id;
  renderBriefSide();
  renderBriefReader();
  try {
    const doc = await window.go.main.App.BriefOpen(id);
    if (stageView.kind !== "brief" || briefUI.docId !== id) return;
    briefUI.doc = doc;
  } catch (e) {
    if (stageView.kind !== "brief" || briefUI.docId !== id) return;
    // Clear the id too, or the reader waits on "Opening…" forever; the
    // error shows in the side panel.
    briefUI.err = "Could not open this brief: " + e; briefUI.doc = null; briefUI.docId = "";
    renderBriefSide();
  }
  renderBriefReader();
}

function fmtTokens(n) { return n >= 1000 ? Math.round(n / 1000) + "k" : String(n); }
function briefWinLabel(k) { return (BRIEF_WINDOWS.find(([w]) => w === k) || [k, k])[1]; }
// "the 3-day brief": the window as an adjective.
function briefWinAdj(k) { return { "24h": "24-hour", "3d": "3-day", "5d": "5-day", "1w": "1-week" }[k] || k; }
function fmtClock(d) { return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false }); }
function fmtDate(sec, withTime) {
  const d = new Date(sec * 1000);
  const s = d.toLocaleDateString(undefined, { weekday: "short", day: "numeric", month: "short" });
  return withTime ? s + " " + fmtClock(d) : s;
}
// fmtWhen is a saved brief's date as people say it: Today, Yesterday, a
// weekday this week, else the date.
function fmtWhen(sec) {
  const d = new Date(sec * 1000), now = new Date();
  const day = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const ago = Math.round((day(now) - day(d)) / 86400000);
  const t = fmtClock(d);
  if (ago === 0) return "Today " + t;
  if (ago === 1) return "Yesterday " + t;
  if (ago > 1 && ago < 7) return d.toLocaleDateString(undefined, { weekday: "long" }) + " " + t;
  return d.toLocaleDateString(undefined, { day: "numeric", month: "short" }) + " " + t;
}
function fmtElapsed(ms) {
  const s = Math.max(0, Math.floor(ms / 1000));
  return Math.floor(s / 60) + ":" + String(s % 60).padStart(2, "0");
}
function fmtTook(ms) {
  if (!ms) return "";
  const s = Math.round(ms / 1000);
  return s < 60 ? s + "s" : Math.floor(s / 60) + "m " + String(s % 60).padStart(2, "0") + "s";
}

function renderBrief() {
  const stage = document.getElementById("stage");
  const p = projectByRoot(briefUI.root);
  stage.replaceChildren();
  stage.innerHTML = `
    <div id="stage-header" class="stage-head po-head">
      <span class="sh-name">${esc(p ? p.name : "Project")}</span>
      <span class="br-crumb">Brief</span>
      <span class="po-acts"><button class="sh-btn" id="br-back" title="Back to the project">Project</button></span>
    </div>
    <div class="br">
      <aside class="br-side" id="br-side"></aside>
      <div class="br-main" id="br-reader"></div>
    </div>`;
  document.getElementById("br-back").addEventListener("click", () => openProject(briefUI.root));
  renderBriefSide();
  renderBriefReader();
}

const briefWave = `<span class="br-wave" aria-hidden="true"><i></i><i></i><i></i><i></i><i></i></span>`;

function renderBriefSide() {
  const el = document.getElementById("br-side");
  if (!el || stageView.kind !== "brief") { clearInterval(briefTimer); briefTimer = null; return; }
  const writing = briefUI.running.includes(briefUI.window);
  const est = briefUI.est;
  let estHtml;
  if (briefUI.estErr) estHtml = `<div class="br-est br-esterr"><b>Could not collect the evidence</b><div class="br-cap">${esc(briefUI.estErr)}</div><button class="sh-btn br-retry" id="br-retry">Try again</button></div>`;
  else if (!est) estHtml = `<div class="br-est br-loading">${briefWave}<span>Reading transcripts and git…</span></div>`;
  else if (!est.sessions && !est.commits) estHtml = `<div class="br-est br-none">Nothing happened here in the ${esc(est.label)}.</div>`;
  else estHtml = `
    <div class="br-est">
      <div class="br-stats">
        <div><b>${est.sessions}</b><span>session${est.sessions === 1 ? "" : "s"}</span></div>
        <div><b>${est.commits}</b><span>your commit${est.commits === 1 ? "" : "s"}</span></div>
        <div><b>${est.otherCommits}</b><span>by others</span></div>
        <div><b>~${fmtTokens(est.inputTokens)}</b><span>tokens in</span></div>
      </div>
      <div class="br-cap">Written by ${esc(est.model)} · takes a few minutes · uses plan quota</div>
    </div>`;
  const canWrite = est && (est.sessions || est.commits) && !writing;
  const started = briefUI.started[briefUI.window];
  const action = writing
    ? `<div class="br-writing">${briefWave}<div><b>Writing the ${esc(briefWinAdj(briefUI.window))} brief</b>
        <div class="br-wmeta">${started ? `<span class="br-clock" id="br-clock">${fmtElapsed(Date.now() - started)}</span> · ` : ""}usually 3–6 minutes</div>
        <div class="br-cap">You can leave this view; the brief is saved when it is done.</div></div></div>`
    : `<button class="sh-btn sh-primary br-write" id="br-write"${canWrite ? "" : " disabled"}>Write brief</button>`;
  // A brief being written for another window shows in the list; the one
  // for this window already has the card above.
  const pending = briefUI.running.filter((w) => w !== briefUI.window).map((w) => `
      <li class="br-item br-pending"><span class="br-iwin">${esc(w)}</span><span class="br-iday">${briefWave} Writing…</span><span class="br-imeta">the ${esc(briefWinAdj(w))} brief</span></li>`).join("");
  el.innerHTML = `
    <section class="br-new">
      <h3>New brief</h3>
      <div class="br-chips" role="radiogroup" aria-label="Window">${BRIEF_WINDOWS.map(([k, l]) => `<button class="br-chip${briefUI.window === k ? " on" : ""}" role="radio" aria-checked="${briefUI.window === k}" data-w="${k}">${l}</button>`).join("")}</div>
      ${estHtml}
      ${action}
      ${briefUI.err ? `<div class="modal-error">${esc(briefUI.err)}</div>` : ""}
    </section>
    <section class="br-saved">
      <h3>Saved briefs${briefUI.list.length ? ` <span class="br-count">${briefUI.list.length}</span>` : ""}</h3>
      ${briefUI.list.length || pending ? `<ul class="br-list">${pending}${briefUI.list.map((m) => `
        <li class="br-item${briefUI.docId === m.id ? " on" : ""}" data-id="${esc(m.id)}" tabindex="0">
          <span class="br-iwin">${esc(m.window)}</span>
          <span class="br-iday">${esc(fmtWhen(m.createdAt))}</span>
          <span class="br-imeta">${m.sessions} session${m.sessions === 1 ? "" : "s"} · ${m.commits} commit${m.commits === 1 ? "" : "s"}</span>
        </li>`).join("")}</ul>` : `<div class="br-none">None yet. Your briefs are kept here.</div>`}
    </section>`;
  el.querySelectorAll(".br-chip").forEach((b) => b.addEventListener("click", () => {
    if (briefUI.window === b.getAttribute("data-w")) return;
    briefUI.window = b.getAttribute("data-w");
    estimateBrief();
  }));
  const wb = document.getElementById("br-write");
  if (wb) wb.addEventListener("click", writeBrief);
  const rb = document.getElementById("br-retry");
  if (rb) rb.addEventListener("click", estimateBrief);
  el.querySelectorAll(".br-item[data-id]").forEach((li) => {
    const open = () => loadBriefDoc(li.getAttribute("data-id"));
    li.addEventListener("click", open);
    li.addEventListener("keydown", (e) => { if (e.key === "Enter") open(); });
  });
  // The clock ticks in place; nothing else re-renders while a brief is written.
  clearInterval(briefTimer); briefTimer = null;
  if (writing && started) briefTimer = setInterval(() => {
    const c = document.getElementById("br-clock");
    if (!c || stageView.kind !== "brief") { clearInterval(briefTimer); briefTimer = null; return; }
    c.textContent = fmtElapsed(Date.now() - started);
  }, 1000);
}

function renderBriefReader() {
  const el = document.getElementById("br-reader");
  if (!el || stageView.kind !== "brief") return;
  const doc = briefUI.doc && briefUI.doc.meta.id === briefUI.docId ? briefUI.doc : null;
  if (!doc) {
    el.innerHTML = briefUI.docId
      ? `<div class="br-skel" aria-label="Opening the brief"><i class="w30"></i><i class="w60 tall"></i><i class="w45"></i><b></b><i></i><i></i><i class="w80"></i><b></b><i></i><i class="w70"></i></div>`
      : `<div class="br-empty">
          <div class="br-empty-mark">${briefWave}</div>
          <h2>What did you actually do?</h2>
          <p>A brief reads what your sessions did and what git says, then explains every task in depth, so you can study it at the end of a day.</p>
          <ol>
            <li><b>Pick a window</b> on the left. Loom collects the evidence first; nothing is sent yet.</li>
            <li><b>Write brief.</b> Claude explains each task: what changed, how it works, what was decided, what proves it, what is still open.</li>
            <li><b>Ask about any task</b> in a read-only session, or make it a page to keep.</li>
          </ol>
        </div>`;
    return;
  }
  const m = doc.meta;
  const daily = m.window === "24h";
  const range = `${esc(fmtDate(m.since, daily))} → ${esc(fmtDate(m.until, daily))}`;
  const stat = (n, one, many) => `<span><b>${n}</b> ${n === 1 ? one : many}</span>`;
  const took = fmtTook(m.durationMs);
  const hero = `
    <header class="br-hero" id="br-top" data-title="${esc("Last " + briefWinLabel(m.window))}">
      <div class="br-eyebrow">Last ${esc(briefWinLabel(m.window))}</div>
      <h1>${range}</h1>
      <div class="br-herostats">
        ${stat(m.sessions, "session", "sessions")}
        ${stat(m.commits, "commit of yours", "commits of yours")}
        ${m.otherCommits ? stat(m.otherCommits, "by others", "by others") : ""}
        <span><b>~${fmtTokens(m.inputTokens)}</b> tokens of evidence</span>
      </div>
      <div class="br-byline">Written by Claude (${esc(m.model)}) ${esc(fmtWhen(m.createdAt).replace(/^(Today|Yesterday)/, (w) => w.toLowerCase()))}${took ? ` in ${took}` : ""}. Every claim cites its evidence; hover a citation to see the session.</div>
    </header>`;
  const outline = [];
  let si = 0;
  const body = doc.parts.map((p) => {
    if (p.kind === "head") return p.html.trim() ? `<section class="br-sec br-intro">${p.html}</section>` : "";
    if (p.kind === "task") {
      outline.push({ id: "br-task-" + p.task, title: p.title, num: p.task });
      return `<section class="br-task" id="br-task-${p.task}" data-title="${esc(p.title)}">
          <header class="br-th">
            <span class="br-tnum">${String(p.task).padStart(2, "0")}</span>
            <h2>${esc(p.title)}</h2>
            <span class="br-tacts">
              <button class="sh-btn sh-primary br-ask" data-t="${p.task}" title="A read-only Claude session that has read this task and its evidence">Ask about this</button>
              <button class="sh-btn br-review" data-t="${p.task}" title="A read-only Claude session that reviews this change independently">Review</button>
            </span>
          </header>
          <div class="br-tbody">${p.html}</div>
        </section>`;
    }
    if (p.kind === "appendix") {
      outline.push({ id: "br-appx", title: p.title, appx: true });
      return `<details class="br-appx" id="br-appx" data-title="${esc(p.title)}">
          <summary><span>${esc(p.title)}</span><span class="br-cap">the git facts as collected, and which session each S-number is</span></summary>
          <div class="br-appx-body">${p.html}</div>
        </details>`;
    }
    const id = "br-sec-" + ++si;
    const ov = /^overview$/i.test(p.title);
    outline.push({ id, title: p.title, lead: ov });
    return `<section class="br-sec${ov ? " br-overview" : ""}" id="${id}" data-title="${esc(p.title)}"><h2 class="br-sh">${esc(p.title)}</h2>${p.html}</section>`;
  }).join("");
  const tasks = outline.filter((o) => o.num);
  const first = outline.findIndex((o) => o.num);
  const before = outline.filter((o, i) => !o.num && (first < 0 || i < first));
  const after = outline.filter((o, i) => !o.num && first >= 0 && i > first);
  const oItem = (o) => `<a class="br-oi${o.num ? " br-onum" : ""}" data-go="${o.id}">${o.num ? `<span>${String(o.num).padStart(2, "0")}</span>` : ""}<b title="${esc(o.title)}">${esc(o.title)}</b></a>`;
  el.innerHTML = `
    <div class="br-bar">
      <button class="br-cur" id="br-cur" title="Contents"><span id="br-cur-t">${esc("Last " + briefWinLabel(m.window))}</span><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" aria-hidden="true"><path d="M6 9l6 6 6-6"/></svg></button>
      <span class="br-bacts">
        <button class="sh-btn" id="br-page" title="Open as a standalone page in your browser, to print or keep">Make artefact</button>
        <button class="sh-btn" id="br-files" title="Show brief.md, evidence.md and the page in Finder">Files</button>
      </span>
    </div>
    <div class="br-body" id="br-body">
      <article class="br-doc">${hero}${body}</article>
      <nav class="br-outline" id="br-outline" aria-label="In this brief">
        <div class="br-oh">In this brief</div>
        ${before.map(oItem).join("")}
        ${tasks.length ? `<div class="br-og">Tasks <span>${tasks.length}</span></div>${tasks.map(oItem).join("")}` : ""}
        ${after.length ? `<div class="br-og"></div>${after.map(oItem).join("")}` : ""}
      </nav>
    </div>`;
  const id = m.id;
  const flash = (b, busy, done, fn) => {
    const label = b.textContent;
    b.disabled = true; b.textContent = busy;
    fn().then(() => { b.textContent = done; setTimeout(() => { b.textContent = label; b.disabled = false; }, 1800); })
      .catch((e) => { b.textContent = label; b.disabled = false; briefUI.err = String(e); renderBriefSide(); });
  };
  const pb = document.getElementById("br-page");
  pb.addEventListener("click", () => flash(pb, "Opening…", "Opened in browser", () => window.go.main.App.BriefArtefact(id)));
  const fb = document.getElementById("br-files");
  fb.addEventListener("click", () => flash(fb, "Opening…", "Shown in Finder", () => window.go.main.App.BriefReveal(id)));
  const bodyEl = document.getElementById("br-body");
  // Narrow: the outline is a menu under the bar's title. It is placed from
  // the button, so it opens in the same spot however far the reader scrolled.
  const curBtn = document.getElementById("br-cur"), outlineEl = document.getElementById("br-outline");
  const closeOutline = () => bodyEl.classList.remove("show-outline");
  curBtn.addEventListener("click", (e) => {
    e.stopPropagation();
    if (getComputedStyle(curBtn.querySelector("svg")).display === "none") return; // wide: the outline is already beside the text
    const r = curBtn.getBoundingClientRect();
    outlineEl.style.left = r.left + "px";
    outlineEl.style.top = r.bottom + 4 + "px";
    bodyEl.classList.toggle("show-outline");
  });
  // Assigned, not added: the reader element outlives each render.
  el.onclick = (e) => { if (!outlineEl.contains(e.target)) closeOutline(); };
  el.onkeydown = (e) => { if (e.key === "Escape") closeOutline(); };
  el.querySelectorAll(".br-oi").forEach((a) => a.addEventListener("click", () => {
    const t = document.getElementById(a.getAttribute("data-go"));
    if (!t) return;
    if (t.tagName === "DETAILS") t.open = true;
    bodyEl.classList.remove("show-outline");
    t.scrollIntoView({ behavior: "smooth", block: "start" });
  }));
  const ask = async (b, mode) => {
    const label = b.textContent;
    b.disabled = true; b.textContent = "Opening…";
    try {
      const name = await window.go.main.App.BriefAsk(id, parseInt(b.getAttribute("data-t"), 10), mode);
      selectSession(name);
      poll();
    } catch (e) { b.disabled = false; b.textContent = label; briefUI.err = "Could not open the session: " + e; renderBriefSide(); }
  };
  el.querySelectorAll(".br-ask").forEach((b) => b.addEventListener("click", () => ask(b, "explain")));
  el.querySelectorAll(".br-review").forEach((b) => b.addEventListener("click", () => ask(b, "review")));
  // Links in a brief open in the browser; nothing navigates the app itself.
  el.querySelectorAll(".br-doc a[href]").forEach((a) => a.addEventListener("click", (e) => {
    e.preventDefault();
    const href = a.getAttribute("href") || "";
    if (/^https?:\/\//i.test(href)) openURLToken(href);
  }));
  // Scroll-spy: the bar names the section under it and the outline marks it.
  const marks = [...el.querySelectorAll(".br-doc [data-title]")];
  const links = new Map([...el.querySelectorAll(".br-oi")].map((a) => [a.getAttribute("data-go"), a]));
  const cur = document.getElementById("br-cur-t");
  let ticking = false;
  const spy = () => {
    ticking = false;
    // At the end the last short sections can never reach the top, so the
    // line moves down to where the eye is.
    const box = el.getBoundingClientRect(), end = el.scrollTop + el.clientHeight >= el.scrollHeight - 4;
    const top = box.top + (end ? box.height * 0.6 : 72);
    let at = marks[0];
    for (const s of marks) { if (s.getBoundingClientRect().top <= top) at = s; else break; }
    if (!at) return;
    cur.textContent = at.getAttribute("data-title");
    links.forEach((a, k) => a.classList.toggle("on", k === at.id));
  };
  el.onscroll = () => { if (!ticking) { ticking = true; requestAnimationFrame(spy); } };
  el.scrollTop = 0;
  spy();
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
  let prefs = { editor: "", notifications: true, autoSummarize: false, terminalTheme: "dark" };
  try { prefs = await window.go.main.App.GetPrefs(); } catch (e) { console.error("prefs", e); }
  const editors = [["", "Auto-detect"], ["cursor", "Cursor"], ["code", "VS Code"], ["zed", "Zed"]];
  const themes = [["dark", "Dark"], ["light", "Light"]];
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
        <label for="pf-term">Theme (chrome, terminal and Claude)</label>
        <select id="pf-term">${themes.map(([v, l]) => `<option value="${v}"${v === (prefs.terminalTheme || "dark") ? " selected" : ""}>${l}</option>`).join("")}</select>
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
      ${s && s.ask ? `<p class="reply-ask">${esc(s.ask)}</p>` : ""}
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
  const sel = latestSessions.find((x) => x.name === name);
  if (sel && sel.external) {
    // A claude loom did not start has no pane to attach: the stage says
    // where it runs and what loom can and cannot do with it.
    renderStageHeader(name);
    renderRail(latestSessions, latestRecent);
    const note = document.createElement("div");
    note.id = "stage-empty";
    note.className = "stage-note";
    note.innerHTML = `This claude runs in a terminal outside loom.<br><span class="sn-path">${esc(sel.cwd || "")}</span><br>` +
      `Loom shows its status, question and finish line, but cannot attach, reply or kill it.${sel.ask ? `<br><br><span class="sn-ask">${esc(sel.ask)}</span>` : ""}`;
    stage.appendChild(note);
    return;
  }
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

  // xterm measures its cell from the font at open(). If the mono face is not
  // loaded yet it measures a fallback, so note that and re-measure on load.
  const monoLate = !document.fonts.check(`13px ${MONO_FACE}`);
  term = new Terminal({
    fontFamily: getComputedStyle(document.documentElement).getPropertyValue("--font-mono").trim(),
    fontSize: 13,
    // Line height stays xterm's 1.0: the DOM renderer draws box glyphs as
    // text, so any extra leading breaks Claude's │ ╭ ╮ frames into dashes.
    fontWeight: 400,
    fontWeightBold: 600,
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
  // wrong cell width and thus the wrong column count; once the face is loaded,
  // make xterm re-measure (a changed fontFamily string is what triggers it; the
  // trailing space is the same CSS list) and re-fit so the grid matches the pane.
  monoReady.then(() => {
    if (gen !== attachGen || !term) return;
    if (monoLate) term.options.fontFamily = term.options.fontFamily + " ";
    onResize();
  });
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
// ---- Agent Discipline (docs/DISCIPLINE.md, phase 3) ----
// The doctor's report behind a dot on the dock footer and an island; the
// gate sheet is the one place the GUI writes to the machine, and only when
// the person clicks Install after the gate reports PASS. The Go side caches
// the report by mtime, so this polls it every 10s, not per 1.5s tick.
let disc = null; // last DisciplineDTO
const DISC_POLL_MS = 10000;

function discColor(d) {
  if (!d || !d.report) return "var(--text-faint)";
  if (d.report.rev && d.report.rev.layout === "none") return "var(--text-faint)";
  return { green: "var(--st-running)", amber: "var(--over)", red: "var(--st-error)" }[d.report.health] || "var(--text-faint)";
}

function discWord(d) {
  if (!d || !d.report || !d.report.rev) return "unknown";
  if (d.report.rev.layout === "none") return "not installed";
  return { green: "enforcing", amber: "needs attention", red: "broken" }[d.report.health] || "unknown";
}

function renderDiscDot() {
  const dot = document.getElementById("disc-dot");
  const btn = document.getElementById("disc-btn");
  if (!dot || !btn) return;
  dot.style.background = discColor(disc);
  dot.classList.toggle("hollow", !disc || !disc.report || disc.report.rev.layout === "none");
  btn.classList.toggle("on", stageView.kind === "discipline");
  btn.title = "Agent Discipline — " + (disc ? disc.headline : "checking…");
}

async function discPoll(force) {
  const fn = bound("DisciplineDoctor");
  if (!fn) return;
  try { disc = await fn(!!force); } catch (e) { console.error("discipline", e); return; }
  renderDiscDot();
  if (stageView.kind === "discipline") renderDiscipline();
}

// A session started before the last hook flip still runs the previous
// hooks: Claude Code reads settings.json and the hook files at start.
function needsRestart(s) {
  return !!(disc && disc.flippedAt && s && s.startedAt && s.startedAt < disc.flippedAt && disc.report && disc.report.rev.layout !== "none");
}
function restartTag(s) {
  return needsRestart(s) ? `<span class="tpill tpill-restart" title="Started before the hooks were last changed (${new Date(disc.flippedAt * 1000).toLocaleString()}); it keeps the previous hooks until restarted">restart for new hooks</span>` : "";
}

function openDiscipline() {
  teardownTerminal();
  activeName = null;
  stageView = { kind: "discipline" };
  renderDiscipline();
  renderRail();
  renderDiscDot();
  discPoll(true);
}

function fmtTS(ts) {
  if (!ts) return "";
  const d = new Date(ts);
  if (isNaN(d.getTime())) return String(ts);
  return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

function discChecks(r) {
  // One row per check the doctor makes, in the handbook's order. Each is
  // [label, ok|warn|bad, detail].
  const rows = [];
  const files = r.files || [];
  const count = (st) => files.filter((f) => f.state === st).length;
  const cur = count("current"), stale = count("stale"), mod = count("modified"), miss = count("missing");
  const pr = r.prereq || {};
  const tool = (name, t, need) => {
    if (!t) return;
    let st = t.found ? "ok" : (need ? "bad" : "warn");
    if (name === "python3" && t.found && !t.runs) st = "bad";
    if (name === "claude" && t.tested === "newer") st = "warn";
    rows.push([name, st, t.found ? [t.version, t.path, t.tested && t.tested !== "tested" ? t.tested + " than the bundle was tested with" : ""].filter(Boolean).join(" · ") : "not found"]);
  };
  tool("python3", pr.python3, true); tool("claude", pr.claude, true); tool("jq", pr.jq, false); tool("git", pr.git, false); tool("tmux", pr.tmux, false);
  const fileSt = miss ? "bad" : (stale ? "warn" : "ok");
  rows.push(["files", fileSt, `${cur} current${stale ? `, ${stale} stale` : ""}${mod ? `, ${mod} yours` : ""}${miss ? `, ${miss} missing` : ""}`]);
  const reg = r.registrations || {};
  rows.push(["hook registrations", reg.matched === reg.expected ? "ok" : "bad", `${reg.matched}/${reg.expected}${reg.disableAllHooks ? " · disableAllHooks is set" : ""}${(reg.others || []).length ? ` · ${reg.others.length} other hook${reg.others.length === 1 ? "" : "s"} registered` : ""}`]);
  const se = r.settings || {};
  rows.push(["status line", se.statusLineSet ? "ok" : "warn", se.statusLineSet ? (se.statusLineOurs ? "the context bar" : "your own") : "not set"]);
  const env = se.env || {};
  const envSet = Object.values(env).filter((v) => v.set).length, envN = Object.keys(env).length;
  rows.push(["env keys", envSet === envN ? "ok" : "warn", envN ? `${envSet}/${envN} set${se.contextBudget ? ` · budget ${Math.round(se.contextBudget / 1000)}k` : ""}` : "none"]);
  const op = r.operate || {};
  rows.push(["deny rule", op.denyRule ? "ok" : "warn", op.denyRule ? "hook directory protected from edits" : "not set"]);
  const pa = r.path || {};
  const res = (x) => x && x.known ? (x.deadline && x.python3 ? "ok" : "bad") : "warn";
  rows.push(["deadline on PATH", res(pa.login), `login ${pa.login && pa.login.deadline ? "yes" : "no"} · tmux ${pa.tmux && pa.tmux.deadline ? "yes" : "no"} · settings env ${pa.settingsEnv && pa.settingsEnv.deadline ? "yes" : "no"}`]);
  const st = r.state || {};
  const dirs = st.dirs || [];
  const badDirs = dirs.filter((d) => !d.exists || !d.writable).length;
  rows.push(["state dirs", badDirs ? "bad" : "ok", `${dirs.length - badDirs}/${dirs.length} writable${st.legacyLogs ? " · logs still beside the hooks" : ""}`]);
  const ru = r.rules || {};
  rows.push(["rules", ru.importLines > 0 || ru.inline ? "ok" : "warn", ru.inline ? "inline in CLAUDE.md" : ru.importLines > 0 ? `imported (${ru.rulesFileState || "ok"})` : "not imported"]);
  const fi = r.firing || {};
  rows.push(["hooks firing", fi.logsFound ? ((fi.quiet || []).length ? "warn" : "ok") : "warn", fi.logsFound ? ((fi.quiet || []).length ? `quiet for 7 days: ${fi.quiet.join(", ")}` : "every hook has fired") : "no log yet"]);
  return rows;
}

function discFileRows(d) {
  const r = d.report;
  // keptPaths is the receipt's Kept set spelled the way the report spells
  // installedPath (through the current link), resolved on the Go side.
  const keptSet = new Set(d.keptPaths || []);
  const rows = (r.files || []).filter((f) => f.state !== "current" && f.state !== "absent" && f.state !== "inline");
  for (const f of r.files || []) if (f.state === "current" && f.installedPath && keptSet.has(f.installedPath) && !rows.includes(f)) rows.push(f);
  if (!rows.length) return `<div class="po-empty">Every file matches the bundle.</div>`;
  const tag = (f) => {
    if (f.state === "modified" && keptSet.has(f.installedPath)) return `<span class="po-tag po-tag-solo" title="Recorded as yours: upgrades leave it alone">kept · yours</span>`;
    if (f.state === "modified") return `<span class="po-tag po-tag-solo">yours</span>`;
    if (keptSet.has(f.installedPath)) return `<span class="po-tag">kept</span>`;
    if (f.state === "stale") return `<span class="po-tag">stale · ${esc(f.matches || "older")}</span>`;
    return `<span class="po-tag">${esc(f.state)}</span>`;
  };
  return `<ul class="po-list">${rows.map((f) => `
    <li class="po-repo" data-file="${esc(f.path)}">
      <span class="po-rlabel">${esc(f.path)}</span>
      <span class="po-rpath">${esc(f.installedPath || f.target)}</span>
      ${tag(f)}
      <span class="po-racts">
        ${f.state === "modified" ? `<button class="tact dsc-diff" title="Show your edit against the bundle">diff</button>` : ""}
        ${f.state === "modified" && !keptSet.has(f.installedPath) ? `<button class="tact dsc-keep" title="Record this as yours: upgrades leave it alone">keep</button>` : ""}
        ${f.state === "modified" || keptSet.has(f.installedPath) ? `<button class="tact dsc-reset" title="Put the bundle's copy back (yours goes to a backup)">reset</button>` : ""}
      </span>
    </li>`).join("")}</ul>`;
}

// ---- Rates and Re-runs tabs (phase 4): the hooks' own logs ----
let discTab = "checks";
try { const v = localStorage.getItem("loom.discTab"); if (["checks", "rates", "reruns"].includes(v)) discTab = v; } catch { /* storage off */ }
let discWindow = "change"; // change | 7d | 30d | all

function fmtPct(n, of) {
  if (!of) return "—";
  const p = 100 * n / of;
  return (p >= 10 ? p.toFixed(0) : p.toFixed(1)) + "%";
}

function windowBar(dto) {
  const opts = [["change", "since change"], ["7d", "7 days"], ["30d", "30 days"], ["all", "all"]];
  const since = dto.since ? `since ${esc(fmtTS(dto.since * 1000))}` : "all records";
  return `<div class="dsc-winbar"><div class="seg" id="dsc-win">${opts.map(([v, l]) => `<button class="seg-b${dto.window === v ? " active" : ""}" data-win="${v}">${l}</button>`).join("")}</div><span class="po-empty">${since}${dto.hidden ? " · hidden projects' records left out" : ""}</span></div>`;
}

// A daily sparkline for one span: holds and nudges as two separate bar
// rows over the day's runs, never stacked into one number.
function sparkline(days) {
  if (!days || !days.length) return "";
  const max = Math.max(1, ...days.map((d) => d.runs));
  const w = 6, gap = 2, h = 22;
  const bars = days.map((d, i) => {
    const x = i * (w + gap);
    const run = Math.max(1, Math.round(h * d.runs / max));
    const hold = Math.round(h * d.holds / max);
    const nudge = Math.round(h * d.nudges / max);
    return `<g><title>${esc(d.day)}: ${d.runs} runs, ${d.holds} holds, ${d.nudges} nudges</title>` +
      `<rect x="${x}" y="${h - run}" width="${w}" height="${run}" class="sp-run"/>` +
      (hold ? `<rect x="${x}" y="${h - hold}" width="${w}" height="${hold}" class="sp-hold"/>` : "") +
      (nudge ? `<rect x="${x}" y="${h - nudge}" width="${w / 2}" height="${nudge}" class="sp-nudge"/>` : "") + `</g>`;
  }).join("");
  return `<svg class="spark" width="${days.length * (w + gap)}" height="${h}" viewBox="0 0 ${days.length * (w + gap)} ${h}" aria-hidden="true">${bars}</svg>`;
}

function ratesHtml(dto) {
  if (!dto.rows.length) return windowBar(dto) + `<div class="po-empty">No hook records in this window.</div>`;
  const rows = dto.rows.map((r) => {
    const spans = r.spans.map((sp, i) => {
      const mark = sp.mark ? `<span class="dsc-mark${sp.inferred ? " inferred" : ""}" title="${sp.inferred ? "Inferred from the logs or the old hooks' edit times" : "From loom's ledger"}">${esc(sp.mark)}</span>` : (i === 0 ? `<span class="dsc-mark none">before any change</span>` : "");
      const top = Object.entries(sp.decisions || {}).sort((a, b) => b[1] - a[1]).slice(0, 4).map(([k, v]) => `${v} ${esc(k)}`).join(" · ");
      return `<div class="dsc-span${sp.inferred ? " inferred" : ""}">
        <div class="dsc-span-head">${mark}<span class="po-rpath">${/^0001-/.test(sp.from || "") ? "start" : esc(fmtTS(sp.from))} → ${esc(fmtTS(sp.to))}</span></div>
        <div class="dsc-span-row">${sparkline(sp.days)}<span class="dsc-num"><b>${sp.runs}</b> runs</span><span class="dsc-num hold"><b>${fmtPct(sp.holds, sp.runs)}</b> holds <small>${sp.holds}</small></span><span class="dsc-num nudge"><b>${fmtPct(sp.nudges, sp.runs)}</b> nudges <small>${sp.nudges}</small></span></div>
        ${top ? `<div class="po-rpath dsc-top">${top}</div>` : ""}
      </div>`;
    }).join("");
    return `<section class="po-block dsc-hook"><div class="po-bhead"><h3>${esc(r.hook)}</h3>${r.rev ? `<span class="po-tag">${esc(r.rev)}</span>` : ""}</div>${spans}</section>`;
  }).join("");
  return windowBar(dto) + `<p class="po-empty">Holds are blocks and denies; nudges are advisory. They are never added together, and a span never crosses a hook change.</p>` + rows;
}

function rerunsHtml(dto) {
  if (!dto.rows.length) return windowBar(dto) + `<div class="po-empty">No test runs logged in this window.</div>`;
  const home = (dto.rows[0].project || "").match(/^\/Users\/[^/]+|^\/home\/[^/]+/);
  const short = (p) => home ? p.replace(home[0], "~") : p;
  const rows = dto.rows.map((r, i) => `
    <li class="po-repo dsc-rerun" data-i="${i}">
      <span class="po-rlabel">${esc(short(r.project))}</span>
      <span class="dsc-num"><b>${r.runs}</b> runs</span>
      <span class="dsc-num"><b>${r.afterChange}</b> after a change</span>
      <span class="dsc-num hold"><b>${fmtPct(r.noChange, r.runs)}</b> no change <small>${r.noChange}</small></span>
      <span class="dsc-num">${r.medianGapS ? `median gap <b>${fmtAge(r.medianGapS)}</b>` : ""}</span>
      ${r.commands.length ? `<button class="tact dsc-cmds">commands</button>` : ""}
      <div class="dsc-cmdlist" hidden>${r.commands.map((c) => `<code>${esc(c)}</code>`).join("")}</div>
    </li>`).join("");
  return windowBar(dto) + `<p class="po-empty">A re-run with no change is a test command run again with the same tree (HEAD, diff and untracked files) as the run before it.</p><ul class="po-list">${rows}</ul>`;
}

async function renderDiscTab() {
  const el = document.getElementById("dsc-tab");
  if (!el) return;
  const fn = bound(discTab === "rates" ? "HookRates" : "HookReruns");
  if (!fn) { el.innerHTML = `<div class="po-empty">Not available in this build.</div>`; return; }
  let dto;
  try { dto = await fn(discWindow); } catch (e) { el.innerHTML = `<div class="modal-error">${esc(String(e))}</div>`; return; }
  if (stageView.kind !== "discipline" || document.getElementById("dsc-tab") !== el) return;
  el.innerHTML = discTab === "rates" ? ratesHtml(dto) : rerunsHtml(dto);
  for (const b of el.querySelectorAll("#dsc-win .seg-b")) b.addEventListener("click", () => { discWindow = b.dataset.win; renderDiscTab(); });
  for (const b of el.querySelectorAll(".dsc-cmds")) b.addEventListener("click", (e) => { e.stopPropagation(); const l = b.parentElement.querySelector(".dsc-cmdlist"); if (l) l.hidden = !l.hidden; });
}

// ---- per-session: finish-line marks, the held chip, the hooks panel ----
function heldChip(s) {
  if (!s.held) return "";
  return `<span class="tpill tpill-held" title="${esc(s.held.hook)} blocked this session ${s.held.count} times for the same reason since its task started">held ×${s.held.count} · ${esc(s.held.decision.replace(/^(block|deny)_/, ""))}</span>`;
}

function hooksChip(s) {
  if (!s || (!s.holds && !s.nudges)) return "";
  const parts = [];
  if (s.holds) parts.push(`${s.holds} hold${s.holds === 1 ? "" : "s"}`);
  if (s.nudges) parts.push(`${s.nudges} nudge${s.nudges === 1 ? "" : "s"}`);
  return `<button class="sh-chip sh-chip-btn${s.held ? " over" : ""}" id="sh-hooks" title="What the hooks did to this session since its task started">${parts.join(" · ")}</button>`;
}

function classLine(c) {
  const hook = c.hook, n = c.count, of = c.of;
  const unit = { bash_guard: "Bash calls", stop_guard: "stops", ctx_budget: "prompts", precompact: "compactions", session_checkpoint: "starts", test_rerun: "test runs" }[hook] || "runs";
  const verb = c.class === "hold" ? (hook === "bash_guard" ? "denied" : "blocked") : c.class === "nudge" ? "nudged" : "repeated with no change";
  return `<li><b>${n}</b> of ${of} ${esc(unit)} ${verb} <span class="po-rpath">${esc(hook)}</span></li>`;
}

async function openHooksPanel(name) {
  if (document.querySelector(".modal-backdrop")) return;
  const fn = bound("SessionHooks");
  if (!fn) return;
  const s = latestSessions.find((x) => x.name === name);
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal hooks-modal" role="dialog" aria-label="Hooks">
      <h2>Hooks · ${esc(s ? displayName(s) : name)}</h2>
      <div id="hooks-body" class="dsc-sheet-body"><div class="po-empty">Loading…</div></div>
      <div class="modal-actions"><button class="btn-ghost" id="hooks-close">Close</button></div>
    </div>`;
  document.body.appendChild(backdrop);
  const close = () => backdrop.remove();
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  backdrop.querySelector("#hooks-close").addEventListener("click", close);
  const body = backdrop.querySelector("#hooks-body");
  let p;
  try { p = await fn(name); } catch (e) { body.innerHTML = `<div class="modal-error">${esc(String(e))}</div>`; return; }
  const groups = (p.stats && p.stats.groups) || [];
  const since = p.since ? `since the task started ${esc(fmtTS(p.since * 1000))}` : "over the whole session";
  // Commands stay masked until a row is expanded: a hold's command can
  // name a client, and this panel is often open on a shared screen.
  body.innerHTML =
    `<p class="po-empty">${since}</p>` +
    `<section class="po-block"><div class="po-bhead"><h3>Holds</h3></div>` +
    (groups.length ? `<ul class="po-list">${groups.map((g, i) => `<li class="po-repo dsc-hold" data-i="${i}"><span class="po-rlabel">${esc(g.hook)} · ${esc(g.decision)}</span>${g.count > 1 ? `<span class="po-tag${g.count >= 3 ? " po-tag-solo" : ""}">×${g.count}</span>` : ""}<span class="po-rpath">${g.count > 1 ? `${esc(fmtTS(g.first))} → ` : ""}${esc(fmtTS(g.last))}</span>${g.command ? `<button class="tact dsc-show">command</button><code class="dsc-cmd" hidden>${esc(g.command)}</code>` : ""}</li>`).join("")}</ul>` : `<div class="po-empty">Nothing held this session.</div>`) +
    `</section>` +
    `<section class="po-block"><div class="po-bhead"><h3>Rates</h3></div>` +
    (p.classes.length ? `<ul class="dsc-classes">${p.classes.map(classLine).join("")}</ul>` : `<div class="po-empty">No hook has fired for this session yet.</div>`) +
    `</section>` +
    (p.recent.length ? `<section class="po-block"><div class="po-bhead"><h3>Recent decisions</h3></div><ul class="dsc-steps">${p.recent.slice(0, 15).map((r) => `<li class="gs-step"><span class="po-rpath">${esc(fmtTS(r.ts))}</span><span class="dsc-k">${esc(r.hook)}</span><span>${esc(r.decision)}</span>${r.agentType ? `<span class="po-tag">${esc(r.agentType)}</span>` : ""}</li>`).join("")}</ul></section>` : "");
  for (const b of body.querySelectorAll(".dsc-show")) b.addEventListener("click", (e) => { e.stopPropagation(); const c = b.parentElement.querySelector(".dsc-cmd"); if (c) { c.hidden = !c.hidden; b.textContent = c.hidden ? "command" : "hide"; } });
}

function renderDiscipline() {
  const stage = document.getElementById("stage");
  if (!stage || stageView.kind !== "discipline") return;
  const d = disc;
  // Keep an open diff or a mid-flight error: the island re-renders on every
  // 10s poll, so only replace the DOM when something it shows changed.
  const sig = d ? JSON.stringify([d.checkedAt, d.headline, d.action, d.staged, d.report.health, d.report.files, d.keptPaths, d.ledger.length, discTab]) : "";
  if (stage.dataset.discSig === sig && stage.querySelector("#dsc-root")) return;
  stage.dataset.discSig = sig;
  stage.replaceChildren();
  if (!d) {
    stage.innerHTML = `<div id="stage-header" class="stage-head"><span class="sh-name">Agent Discipline</span></div><div class="po" id="dsc-root"><div class="po-empty">Checking…</div></div>`;
    return;
  }
  const r = d.report;
  const color = discColor(d);
  const actionLabel = { install: "Review install…", upgrade: `Upgrade to ${esc(r.rev.bundled)}…`, repair: "Repair…" }[d.action] || "";
  const installed = r.rev.layout === "rev";
  const checks = discChecks(r);
  const sev = (s) => s === "ok" ? "var(--st-running)" : s === "warn" ? "var(--over)" : "var(--st-error)";
  const led = (d.ledger || []).slice(-6).reverse();
  stage.innerHTML = `
    <div id="stage-header" class="stage-head po-head">
      <span class="sh-name">Agent Discipline</span>
      <span class="sh-pill"><i style="background:${color}"></i><span style="color:${color}">${esc(discWord(d))}</span></span>
      ${installed ? `<span class="sh-chip">${esc(r.rev.installed)}</span>` : ""}
      ${r.rev.bundled && r.rev.bundled !== r.rev.installed ? `<span class="sh-chip" title="Revision carried by this build of loom">bundle ${esc(r.rev.bundled)}</span>` : ""}
      <span class="po-acts">
        <button class="sh-btn" id="dsc-recheck" title="Run the doctor again now">Re-check</button>
        ${actionLabel ? `<button class="sh-btn sh-primary" id="dsc-action">${actionLabel}</button>` : ""}
        ${installed && d.receipt ? `<button class="sh-btn sh-danger" id="dsc-uninstall" title="Undo what loom installed, from the receipt">Uninstall</button>` : ""}
      </span>
    </div>
    <div class="po" id="dsc-root">
      <div class="modal-error" id="dsc-error"></div>
      <p class="dsc-headline">${esc(d.headline)}</p>
      <div class="seg dsc-tabs" id="dsc-tabs">${["checks", "rates", "reruns"].map((t) => `<button class="seg-b${discTab === t ? " active" : ""}" data-tab="${t}">${{ checks: "Checks", rates: "Rates", reruns: "Re-runs" }[t]}</button>`).join("")}</div>
      <div id="dsc-tab" class="dsc-tab">${discTab === "checks" ? discChecksHtml(d) : `<div class="po-empty">Loading…</div>`}</div>
    </div>`;
  for (const b of stage.querySelectorAll("#dsc-tabs .seg-b")) b.addEventListener("click", () => { discTab = b.dataset.tab; try { localStorage.setItem("loom.discTab", discTab); } catch { /* storage off */ } renderDiscipline(); });
  if (discTab !== "checks") renderDiscTab();
  const on = (id, fn) => { const el = document.getElementById(id); if (el) el.addEventListener("click", fn); };
  on("dsc-recheck", () => discPoll(true));
  on("dsc-action", () => openGateSheet(d.action));
  on("dsc-uninstall", () => uninstallFlow());
  wireFileRows(stage);
}

function wireFileRows(stage) {
  for (const b of stage.querySelectorAll(".dsc-copy")) b.addEventListener("click", (e) => { e.stopPropagation(); copyCommand(b); });
  for (const li of stage.querySelectorAll("li[data-file]")) {
    const file = li.dataset.file;
    const diffB = li.querySelector(".dsc-diff"), keepB = li.querySelector(".dsc-keep"), resetB = li.querySelector(".dsc-reset");
    if (diffB) diffB.addEventListener("click", (e) => { e.stopPropagation(); discFileAction("diff", file); });
    if (keepB) keepB.addEventListener("click", (e) => { e.stopPropagation(); discFileAction("keep", file); });
    if (resetB) armAction(resetB, "reset?", () => discFileAction("reset", file));
  }
}

// The Checks tab: issues, the doctor's table, files, history, paths.
function discChecksHtml(d) {
  const r = d.report;
  const checks = discChecks(r);
  const sev = (s) => s === "ok" ? "var(--st-running)" : s === "warn" ? "var(--over)" : "var(--st-error)";
  const led = (d.ledger || []).slice(-6).reverse();
  return `
      ${(r.issues || []).length ? `<div class="po-warns">${r.issues.map((i) => `<div class="po-warn dsc-issue dsc-${esc(i.severity)}"><b>${esc(i.group)}</b> ${esc(i.message)}${i.fix ? ` <span class="dsc-fix">· ${esc(i.fix)}</span>` : ""}</div>`).join("")}</div>` : ""}
      <section class="po-block">
        <div class="po-bhead"><h3>Checks</h3><span class="po-empty">${esc(fmtTS(r.checkedAt))}</span></div>
        <table class="dsc-table">${checks.map(([l, s, det]) => `<tr><td class="dsc-k">${esc(l)}</td><td class="dsc-s"><i style="background:${sev(s)}"></i></td><td class="dsc-v">${esc(det)}</td></tr>`).join("")}</table>
      </section>
      <section class="po-block">
        <div class="po-bhead"><h3>Files</h3></div>
        ${discFileRows(d)}
        <div class="diff-patch" id="dsc-patch" hidden></div>
      </section>
      <section class="po-block">
        <div class="po-bhead"><h3>History</h3>${d.receipt ? `<span class="po-empty">installed ${esc(fmtTS(d.receipt.installedAt))}${d.receipt.loomBuild ? " by " + esc(d.receipt.loomBuild) : ""}</span>` : ""}</div>
        ${led.length ? `<ul class="po-list">${led.map((e) => `<li class="po-repo"><span class="po-rlabel">${esc(e.op)}${e.fromRev || e.toRev ? ` ${esc(e.fromRev || "—")} → ${esc(e.toRev || "—")}` : ""}</span><span class="po-rpath">${esc(fmtTS(e.ts))}${e.gate ? ` · gate ${e.gate.suitesPassed ? "pass" : "fail"}${e.gate.newAllows || e.gate.newDenies ? ` · ${e.gate.newAllows} new allows, ${e.gate.newDenies} new denies` : ""}` : ""}${e.note ? " · " + esc(e.note) : ""}</span>${e.backup ? `<span class="po-tag" title="${esc(e.backup)}">backup</span>` : ""}</li>`).join("")}</ul>` : `<div class="po-empty">Loom has not installed or changed anything here yet.</div>`}
      </section>
      ${dietHtml(r.diet)}
      <section class="po-block">
        <div class="po-bhead"><h3>Where</h3></div>
        <div class="dsc-where">${[["config", r.layout.configDir], ["hooks", r.layout.hooksDir], ["state", r.layout.stateDir], ["logs", r.layout.logDir], ["deadline", r.layout.deadline]].map(([k, v]) => `<span class="dsc-k">${k}</span><span class="po-rpath">${esc(v || "")}</span>`).join("")}</div>
      </section>
`;
}

// ---- Context diet (phase 5): what every session loads, what each project
// uses. Rows carry a copyable command; loom never runs one.
function dietHtml(d) {
  if (!d) return "";
  const plugins = d.pluginsUserScope || [];
  const enabled = plugins.filter((p) => p.enabled);
  const servers = (d.servers || []).filter((s) => s.calls > 0 || s.scope === "user" || s.scope === "plugin" || s.scope === "project");
  const projects = d.projects || [];
  const cols = servers.filter((s) => s.calls > 0).map((s) => s.name);
  const days = (ts) => ts ? fmtAge(Math.max(0, Math.floor(Date.now() / 1000 - ts))) + " ago" : "never";
  const loads = (p) => [p.skills ? `${p.skills} skill${p.skills === 1 ? "" : "s"}` : "", p.commands ? `${p.commands} command${p.commands === 1 ? "" : "s"}` : "", p.agents ? `${p.agents} agent${p.agents === 1 ? "" : "s"}` : "", p.hooks ? "hooks" : "", (p.mcpServers || []).length ? `mcp ${p.mcpServers.join(", ")}` : ""].filter(Boolean).join(" · ");
  const copyBtn = (cmd) => cmd ? `<button class="tact dsc-copy" data-cmd="${esc(cmd)}" title="${esc(cmd)}">copy command</button>` : "";
  const pluginRow = (p) => `<li class="po-repo dsc-plugin${p.enabled ? "" : " off"}">
      <span class="po-rlabel">${esc(p.name)}<span class="po-rpath"> @${esc(p.marketplace)}${p.version ? " " + esc(p.version) : ""}</span></span>
      ${p.enabled ? (p.alwaysOnTokens ? `<span class="po-tag" title="Added to every session, per claude plugin details">~${p.alwaysOnTokens >= 1000 ? (p.alwaysOnTokens / 1000).toFixed(1) + "k" : p.alwaysOnTokens} tok</span>` : "") : `<span class="po-tag">off</span>`}
      <span class="dsc-num">${esc(loads(p) || "nothing loaded")}</span>
      <span class="dsc-num ${p.enabled && !p.usageCount ? "hold" : ""}"><b>${p.usageCount ? `used ${p.usageCount}×` : "never used"}</b>${p.lastUsedAt ? ` <small>${esc(days(p.lastUsedAt))}</small>` : ""}</span>
      ${copyBtn(p.command)}
    </li>`;
  const grid = projects.length && cols.length ? `<div class="dsc-gridwrap"><table class="dsc-grid"><thead><tr><th>project</th>${cols.map((c) => `<th title="${esc(c)}">${esc((servers.find((s) => s.name === c) || {}).label || c)}</th>`).join("")}<th>off here</th></tr></thead><tbody>${projects.map((p) => `<tr><td class="dsc-k">${esc(p.name)}${p.trackedLocalSettings && p.trackedLocalSettings.length ? ` <span class="po-tag po-tag-solo" title="${esc(p.trackedLocalSettings.join("\n"))}">settings.local.json is git-tracked</span>` : ""}</td>${cols.map((c) => `<td class="dsc-cell${p.calls && p.calls[c] ? "" : " zero"}">${p.calls && p.calls[c] ? p.calls[c] : "·"}</td>`).join("")}<td class="po-rpath">${esc((p.disabled || []).map(dietLabel).join(", "))}</td></tr>`).join("")}</tbody></table></div>` : `<div class="po-empty">${d.usageSource ? "No MCP calls indexed for a visible project yet." : "MCP usage is unknown: loom's index was not opened."}</div>`;
  const unused = servers.filter((s) => s.command);
  return `
      <section class="po-block dsc-diet">
        <div class="po-bhead"><h3>Context diet</h3><span class="po-empty">${enabled.length} of ${plugins.length} user-scope plugins enabled${d.unscannedFiles ? ` · ${d.unscannedFiles} indexed files not yet scanned for MCP calls` : ""}</span></div>
        ${plugins.length ? `<ul class="po-list">${plugins.map(pluginRow).join("")}</ul>` : `<div class="po-empty">No plugins installed at user scope.</div>`}
        <p class="po-empty">Every enabled plugin and every MCP server adds its tool definitions to each session's context. Nothing here is applied by loom: copy the command and run it yourself.</p>
        ${grid}
        ${unused.length ? `<ul class="po-list">${unused.map((s) => `<li class="po-repo"><span class="po-rlabel">${esc(s.label)}</span><span class="dsc-num hold"><b>no calls</b> in any visible project</span>${copyBtn(s.command)}</li>`).join("")}</ul>` : ""}
        ${d.unattributed ? `<div class="po-empty">${d.unattributed} session${d.unattributed === 1 ? "" : "s"} with MCP calls outside the listed projects.</div>` : ""}
        ${(d.warnings || []).map((w) => `<div class="po-warn dsc-issue dsc-amber">${esc(w)}</div>`).join("")}
      </section>`;
}

function dietLabel(name) {
  if (name.startsWith("claude_ai_")) return "claude.ai " + name.slice(10).replace(/_/g, " ");
  if (name.startsWith("plugin_")) { const r = name.slice(7); const i = r.lastIndexOf("_"); return i > 0 ? `${r.slice(i + 1)} (plugin ${r.slice(0, i)})` : r; }
  return name;
}

async function copyCommand(btn) {
  try {
    await navigator.clipboard.writeText(btn.dataset.cmd);
    const was = btn.textContent;
    btn.textContent = "copied";
    setTimeout(() => { btn.textContent = was; }, 1200);
  } catch { flashDisc("Could not copy: " + btn.dataset.cmd); }
}

async function discFileAction(action, file) {
  const fn = bound("DisciplineAction");
  const err = document.getElementById("dsc-error");
  if (!fn) return;
  try {
    const out = await fn(action, file);
    if (action === "diff") {
      const el = document.getElementById("dsc-patch");
      if (el) { el.hidden = false; el.innerHTML = out ? renderPatch(out) : `<span class="dl">no difference</span>`; }
      return;
    }
    if (err) err.textContent = out;
    discPoll(true);
  } catch (e) { console.error("discipline", e); if (err) err.textContent = String(e); }
}

async function uninstallFlow() {
  const planFn = bound("DisciplineUninstallPlan"), doFn = bound("DisciplineUninstall");
  if (!planFn || !doFn) return;
  let plan;
  try { plan = await planFn(); } catch (e) { flashDisc(String(e)); return; }
  const body = `<p>Loom undoes what its receipt lists and nothing else: ${plan.steps.length} steps.</p>` +
    `<ul class="dsc-steps">${plan.steps.map((s) => `<li>${esc(s.kind)} <span class="po-rpath">${esc(s.target)}</span>${s.detail ? ` <span class="dsc-fix">${esc(s.detail)}</span>` : ""}</li>`).join("")}</ul>` +
    (plan.warnings.length ? `<div class="po-warns">${plan.warnings.map((w) => `<div class="po-warn">${esc(w)}</div>`).join("")}</div>` : "") +
    (plan.errors.length ? `<div class="modal-error">${plan.errors.map(esc).join("<br>")}</div>` : "") +
    `<p>Running sessions keep working; they pick up the change when restarted.</p>`;
  if (plan.errors.length) { await confirmModal("Cannot uninstall", body, "OK"); return; }
  if (!(await confirmModal("Uninstall Agent Discipline?", body, "Uninstall"))) return;
  try { await doFn(); } catch (e) { flashDisc(String(e)); }
  discPoll(true);
}

function flashDisc(msg) {
  const el = document.getElementById("dsc-error");
  if (el) el.textContent = msg;
  else console.error("discipline", msg);
}

// The gate sheet: choices → plan → "Stage & run gate" → result → Install,
// which is enabled only on PASS and, when the replay found verdict flips,
// only once they are acknowledged. Cancel at any point abandons the staged
// revision; nothing a session uses has changed until Install.
async function openGateSheet(action) {
  if (document.querySelector(".modal-backdrop")) return;
  const planFn = bound("DisciplinePlan"), stageFn = bound("DisciplineStage"), applyFn = bound("DisciplineApply"), abandonFn = bound("DisciplineAbandon");
  if (!planFn || !stageFn || !applyFn) return;
  const opts = { rules: true, statusLine: true, env: false, deny: true, replace: [] };
  const titles = { install: "Install Agent Discipline", upgrade: "Upgrade Agent Discipline", repair: "Repair Agent Discipline" };
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal disc-sheet" role="dialog" aria-label="${esc(titles[action] || "Agent Discipline")}">
      <h2>${esc(titles[action] || "Agent Discipline")}</h2>
      <div class="dsc-sheet-body" id="gs-body"><div class="po-empty">Planning…</div></div>
      <div class="modal-error" id="gs-error"></div>
      <div class="modal-actions">
        <span class="gs-status" id="gs-status"></span>
        <button class="btn-ghost" id="gs-cancel">Cancel</button>
        <button class="btn-launch" id="gs-stage">Stage &amp; run gate</button>
        <button class="btn-launch" id="gs-install" disabled title="Enabled once the gate reports PASS">Install</button>
      </div>
    </div>`;
  document.body.appendChild(backdrop);
  const body = backdrop.querySelector("#gs-body"), errEl = backdrop.querySelector("#gs-error"), status = backdrop.querySelector("#gs-status");
  const stageBtn = backdrop.querySelector("#gs-stage"), installBtn = backdrop.querySelector("#gs-install"), cancelBtn = backdrop.querySelector("#gs-cancel");
  let staged = null, busy = false;
  const close = async () => {
    if (busy) return;
    document.removeEventListener("keydown", onEsc, true);
    backdrop.remove();
    if (staged && abandonFn) { try { await abandonFn(); } catch (e) { console.error("abandon", e); } }
    discPoll(true);
  };
  const onEsc = (e) => { if (e.key === "Escape") close(); };
  document.addEventListener("keydown", onEsc, true);
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  cancelBtn.addEventListener("click", close);

  // Read only the controls that are on screen: before the first plan and on
  // the gate screen there are none, and the choices made earlier must hold.
  const readOpts = () => {
    const cb = (id, key) => { const el = body.querySelector("#" + id); if (el) opts[key] = el.checked; };
    cb("gs-rules", "rules"); cb("gs-statusline", "statusLine"); cb("gs-env", "env"); cb("gs-deny", "deny");
    const sels = [...body.querySelectorAll("select.gs-conflict")];
    if (sels.length) opts.replace = sels.filter((s) => s.value === "replace").map((s) => s.dataset.path);
  };
  const stepLine = (s) => `<li class="gs-step${s.enabled ? "" : " off"}"><span class="dsc-k">${esc(s.kind)}</span><span class="po-rpath">${esc(s.target)}</span>${s.detail ? `<span class="dsc-fix">${esc(s.detail)}</span>` : ""}${s.diff ? `<details class="gs-diff"><summary>diff</summary><div class="diff-patch">${renderPatch(s.diff)}</div></details>` : ""}</li>`;
  const renderPlan = (plan) => {
    const groups = [["core", "Core: hooks, context bar, deadline wrapper, registrations", null],
      ["rules", "Import the handbook's rules into CLAUDE.md", "gs-rules"],
      ["statusline", "Set the context bar as the status line (only when none is set)", "gs-statusline"],
      ["env", "Set the three optional env keys (auto-compact above budget, PATH)", "gs-env"],
      ["deny", "Deny edits to the hook directory from inside sessions", "gs-deny"]];
    const byGroup = (g) => plan.steps.filter((s) => s.group === g);
    body.innerHTML =
      `<p class="dsc-headline">${esc(plan.op)} ${esc(plan.fromRev && plan.fromRev !== plan.rev ? plan.fromRev + " → " + plan.rev : plan.rev)} on the ${esc(plan.layout)} layout · ${plan.steps.filter((s) => s.enabled).length} steps</p>` +
      (plan.errors.length ? `<div class="modal-error">${plan.errors.map(esc).join("<br>")}</div>` : "") +
      (plan.warnings.length ? `<div class="po-warns">${plan.warnings.map((w) => `<div class="po-warn">${esc(w)}</div>`).join("")}</div>` : "") +
      (plan.conflicts.length ? `<section class="po-block"><div class="po-bhead"><h3>Your edits</h3></div><ul class="po-list">${plan.conflicts.map((c) => `<li class="po-repo"><span class="po-rpath">${esc(c.path)}</span><select class="gs-conflict" data-path="${esc(c.path)}"><option value="keep"${c.choice === "keep" ? " selected" : ""}>keep mine</option><option value="replace"${c.choice === "replace" ? " selected" : ""}>replace with the bundle's</option></select></li>`).join("")}</ul></section>` : "") +
      groups.map(([g, label, id]) => {
        const steps = byGroup(g);
        if (!steps.length) return "";
        const head = id ? `<label class="pf-check"><input type="checkbox" id="${id}"${opts[g === "statusline" ? "statusLine" : g] ? " checked" : ""} /><span>${label}</span></label>` : `<div class="po-bhead"><h3>${label}</h3></div>`;
        return `<section class="po-block">${head}<ul class="dsc-steps">${steps.map(stepLine).join("")}</ul></section>`;
      }).join("") +
      (plan.hooks && plan.hooks.length ? `<section class="po-block"><div class="po-bhead"><h3>Registrations</h3></div><ul class="dsc-steps">${plan.hooks.map((h) => `<li class="gs-step"><span class="dsc-k">${esc(h.action)}</span><span class="po-rpath">${esc(h.id)}</span>${h.detail ? `<span class="dsc-fix">${esc(h.detail)}</span>` : ""}</li>`).join("")}</ul></section>` : "");
    stageBtn.disabled = plan.errors.length > 0;
    for (const id of ["gs-rules", "gs-statusline", "gs-env", "gs-deny"]) {
      const el = body.querySelector("#" + id);
      if (el) el.addEventListener("change", replan);
    }
    for (const sel of body.querySelectorAll("select.gs-conflict")) sel.addEventListener("change", replan);
  };
  const replan = async () => {
    readOpts();
    try { renderPlan(await planFn(opts)); } catch (e) { errEl.textContent = String(e); }
  };
  const renderGate = (res) => {
    const g = res.gate || {};
    const flips = ((g.replay && g.replay.newAllows) || []).length + ((g.replay && g.replay.newDenies) || []).length;
    const line = (ok, label, det) => `<tr><td class="dsc-s"><i style="background:${ok === null ? "var(--text-faint)" : ok ? "var(--st-running)" : "var(--st-error)"}"></i></td><td class="dsc-k">${esc(label)}</td><td class="dsc-v">${det}</td></tr>`;
    const pre = g.preflightFailures || [];
    body.innerHTML =
      `<p class="dsc-headline">${res.error ? "The gate did not pass. Nothing was changed." : g.passed ? "Gate PASS. Nothing a session uses has changed yet; Install switches the revision in." : "The gate did not pass. Nothing was changed."}</p>` +
      (res.error ? `<div class="modal-error">${esc(res.error)}</div>` : "") +
      `<table class="dsc-table">` +
      line(pre.length === 0, "preflight", pre.length ? pre.map(esc).join("<br>") : "python3 runs, every hook parses, the deadline wrapper works") +
      (g.suites && g.suites.ran ? line(g.suites.passed, "suites", g.suites.passed ? "OVERALL: PASS" : `<details><summary>FAIL</summary><pre class="gs-out">${esc(g.suites.output || "")}</pre></details>`) : line(null, "suites", "not run")) +
      (g.replay && g.replay.ran ? line(flips === 0, "replay", `${g.replay.newAllows.length} newly allowed, ${g.replay.newDenies.length} newly denied over 30 days of logged denials`) : line(null, "replay", "no decisions log to replay")) +
      `</table>` +
      (flips ? `<section class="po-block"><div class="po-bhead"><h3>Verdicts that change</h3></div><ul class="dsc-steps">${g.replay.newAllows.map((s) => `<li class="gs-step"><span class="dsc-k">now allow</span><span class="po-rpath">${esc(s)}</span></li>`).join("")}${g.replay.newDenies.map((s) => `<li class="gs-step"><span class="dsc-k">now deny</span><span class="po-rpath">${esc(s)}</span></li>`).join("")}</ul><label class="pf-check"><input type="checkbox" id="gs-ack" /><span>I have read these ${flips} changes and want the new verdicts</span></label></section>` : "");
    const ack = body.querySelector("#gs-ack");
    const update = () => { installBtn.disabled = !(g.passed && !res.error && (flips === 0 || (ack && ack.checked))); installBtn.title = installBtn.disabled ? (g.passed ? "Acknowledge the verdict changes first" : "Enabled once the gate reports PASS") : "Switch the staged revision in"; };
    if (ack) ack.addEventListener("change", update);
    update();
  };

  stageBtn.addEventListener("click", async () => {
    if (busy) return;
    readOpts();
    busy = true; stageBtn.disabled = true; installBtn.disabled = true; cancelBtn.disabled = true;
    status.textContent = "Staging and running the gate… (suites take a minute)";
    errEl.textContent = "";
    try {
      const res = await stageFn(opts);
      staged = !res.error;
      renderGate(res);
      status.textContent = "";
    } catch (e) { errEl.textContent = String(e); staged = false; }
    busy = false; cancelBtn.disabled = false; stageBtn.disabled = false; stageBtn.textContent = "Stage again";
  });
  installBtn.addEventListener("click", async () => {
    if (busy || installBtn.disabled) return;
    const ack = body.querySelector("#gs-ack");
    busy = true; installBtn.disabled = true; stageBtn.disabled = true; cancelBtn.disabled = true;
    status.textContent = "Installing…";
    try {
      const res = await applyFn(!!(ack && ack.checked));
      staged = null;
      body.innerHTML = `<p class="dsc-headline">Installed. Sessions started from now on use the new hooks; running ones keep the old until restarted.</p>` +
        (res && res.backupDir ? `<p class="po-empty">Backup of settings.json and CLAUDE.md in ${esc(res.backupDir)}</p>` : "") +
        (res && res.log ? `<pre class="gs-out">${esc(res.log.join("\n"))}</pre>` : "");
      status.textContent = "";
      stageBtn.hidden = true; installBtn.hidden = true; cancelBtn.textContent = "Close";
    } catch (e) { errEl.textContent = String(e); status.textContent = ""; installBtn.disabled = false; stageBtn.disabled = false; }
    busy = false; cancelBtn.disabled = false;
  });
  await replan();
}

document.getElementById("disc-btn").addEventListener("click", openDiscipline);
discPoll(false);
setInterval(() => discPoll(false), DISC_POLL_MS);

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
    items.push({ label: "Brief: " + p.name, hint: "what was done here", run: () => openBrief(p.root) });
  }
  if (latestProjects.some((p) => p.hidden)) {
    items.push({ label: "Hidden projects…", hint: "restore one", run: () => openHiddenProjects() });
  }
  for (const s of latestSessions) {
    items.push({
      label: displayName(s),
      hint: `${s.project} · ${statusWord(s.status)}${s.waitingFor ? " · " + s.waitingFor : ""}${s.external ? " · terminal" : ""}`,
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
