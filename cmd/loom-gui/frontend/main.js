import "./tokens.css";
import "@xterm/xterm/css/xterm.css";
import { statusColor, statusWord, xtermTheme } from "./theme.js";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { Unicode11Addon } from "@xterm/addon-unicode11";

const threadsEl = document.getElementById("threads");
const attnEl = document.getElementById("attn");
let activeName = null;
let latestSessions = [];
let latestRecent = [];

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

document.getElementById("new-session").addEventListener("click", openLauncher);
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
const STATUS_ICON = {
  needs_you: '<path d="M12 8v5M12 16.5v.01"/>',
  running: '<path d="M9 6l8 6-8 6z"/>',
  idle: '<path d="M6 12h12"/>',
  done: '<path d="M5 13l4 4L19 7"/>',
  error: '<path d="M12 8v5M12 16.5v.01"/>',
  unknown: '<path d="M6 12h12"/>',
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

function appendLiveRow(s) {
  const li = document.createElement("li");
  li.className = "thread" + (s.name === activeName ? " active" : "");
  const color = statusColor(s.status);
  li.style.setProperty("--tc", color);
  li.innerHTML =
    `<span class="tglyph i-${esc(s.status)}">${icon(STATUS_ICON[s.status] || STATUS_ICON.unknown, 3)}</span>` +
    `<span class="tinfo"><span class="tname">${esc(displayName(s))}</span><span class="tproj">${esc(s.project)}</span>${ctxGaugeHtml(s.ctxTokens)}</span>` +
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

function renderRail(sessions, recent) {
  threadsEl.replaceChildren();
  for (const g of GROUPS) {
    const rows = sessions.filter((s) => g.match(s.status));
    if (!rows.length) continue;
    appendGroup(g.key);
    for (const s of rows) appendLiveRow(s);
  }
  if (recent && recent.length) {
    appendGroup("Finished");
    for (const s of recent) appendFinishedRow(s);
  }
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

// ---- preferences ----
async function openPrefs() {
  if (document.querySelector(".modal-backdrop")) return;
  let prefs = { editor: "", notifications: true, autoSummarize: false };
  try { prefs = await window.go.main.App.GetPrefs(); } catch (e) { console.error("prefs", e); }
  const editors = [["", "Auto-detect"], ["cursor", "Cursor"], ["code", "VS Code"], ["zed", "Zed"]];
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal prefs-modal" role="dialog" aria-label="Preferences">
      <h2>Preferences</h2>
      <div class="field">
        <label for="pf-editor">Editor for opening files</label>
        <select id="pf-editor">${editors.map(([v, l]) => `<option value="${v}"${v === prefs.editor ? " selected" : ""}>${l}</option>`).join("")}</select>
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
    };
    try { await window.go.main.App.SetPrefs(next); } catch (e) { console.error("setprefs", e); }
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
  if (d.error) { body.innerHTML = `<div class="diff-empty">${esc(d.error)}</div>`; return; }
  if (!d.dirty) { body.innerHTML = `<div class="diff-empty">No uncommitted changes.</div>`; return; }

  let html = "";
  if (d.stat) html += `<pre class="diff-stat">${esc(d.stat)}</pre>`;
  if (d.untracked && d.untracked.length) {
    html += `<div class="diff-untracked"><span class="du-head">new files</span>` +
      d.untracked.map((f) => `<span class="du-file" data-path="${esc(f)}">${esc(f)}</span>`).join("") + `</div>`;
  }
  if (d.patch) html += `<pre class="diff-patch">${renderPatch(d.patch)}</pre>`;
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
  window.removeEventListener("resize", onResize);
  if (activeName) { window.go.main.App.CloseSession(activeName); }
  if (term) { term.dispose(); term = null; }
}

function selectSession(name) {
  teardownTerminal();
  activeName = name;
  const gen = ++attachGen;

  const stage = document.getElementById("stage");
  stage.replaceChildren();
  const header = document.createElement("div");
  header.id = "stage-header";
  stage.appendChild(header);
  const host = document.createElement("div");
  host.id = "terminal";
  stage.appendChild(host);
  renderStageHeader(name);
  renderRail(latestSessions, latestRecent); // reflect active highlight immediately

  term = new Terminal({
    fontFamily: getComputedStyle(document.documentElement).getPropertyValue("--font-mono"),
    fontSize: 13,
    theme: xtermTheme,
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

  window.addEventListener("resize", onResize);
}

function onResize() {
  if (!term || !fit || !activeName) return;
  fit.fit();
  window.go.main.App.ResizeSession(activeName, term.cols, term.rows);
}

// Detect file paths in terminal output and make them ⌘-clickable. Matches a
// path with a directory segment (…/file.ext) or a bare filename with a line
// (file.ext:88) — both with optional :line[:col] — to avoid underlining every
// word.ext token. ⌘-click resolves against the session cwd and opens the
// editor (the backend no-ops if it isn't a real file); a plain click is left
// to normal terminal text-selection, the iTerm2 / VS Code convention.
const FILE_LINK_RE =
  /(?:\.{0,2}\/)?(?:[\w.@~+-]+\/)+[\w.@~+-]+\.[A-Za-z][\w]{0,9}(?::\d+(?::\d+)?)?|[\w.@~+-]+\.[A-Za-z][\w]{0,9}:\d+(?::\d+)?/g;

function registerFileLinks(t) {
  t.registerLinkProvider({
    provideLinks(y, cb) {
      const bufLine = t.buffer.active.getLine(y - 1);
      if (!bufLine) { cb(undefined); return; }
      const text = bufLine.translateToString(true);
      const links = [];
      let m;
      FILE_LINK_RE.lastIndex = 0;
      while ((m = FILE_LINK_RE.exec(text)) !== null) {
        const token = m[0];
        links.push({
          text: token,
          range: { start: { x: m.index + 1, y }, end: { x: m.index + token.length, y } },
          // Only ⌘-click opens; a plain click stays a normal terminal click so
          // it doesn't fight text selection.
          activate: (event) => { if (event && event.metaKey) openFileToken(token); },
        });
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

// ---- poll ----
async function poll() {
  try {
    const [sessions, recent] = await Promise.all([
      window.go.main.App.ListSessions(),
      window.go.main.App.ListRecent(),
    ]);
    latestSessions = sessions;
    latestRecent = recent;
    renderRail(sessions, recent);
    renderAttention(sessions);
    if (activeName) renderStageHeader(activeName);
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

async function openLauncher() {
  if (document.querySelector(".modal-backdrop")) return; // don't stack modals
  // Append the backdrop synchronously BEFORE any await, so a second rapid click
  // sees it via the guard above and can't stack a duplicate during the load.
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal" role="dialog" aria-label="New session">
      <h2>New session</h2>
      <div class="field">
        <label for="f-project">Project</label>
        <select id="f-project"></select>
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

  // Load projects and fill the (already-mounted) select.
  let projects = [];
  try {
    projects = await window.go.main.App.ListProjects();
  } catch (e) {
    console.error("ListProjects failed", e);
  }
  backdrop.querySelector("#f-project").innerHTML =
    projects.map((p) => `<option value="${esc(p.path)}">${esc(p.label)}</option>`).join("");

  const launchBtn = backdrop.querySelector("#f-launch");
  launchBtn.addEventListener("click", async () => {
    const path = backdrop.querySelector("#f-project").value;
    const model = backdrop.querySelector("#f-model").value;
    const mode = backdrop.querySelector("#f-mode").value;
    const seed = backdrop.querySelector("#f-seed").value;
    const errEl = backdrop.querySelector("#f-error");
    if (!path) { errEl.textContent = "Pick a project to launch."; return; }
    errEl.textContent = "";
    launchBtn.disabled = true;
    try {
      const name = await window.go.main.App.LaunchSession(path, model, mode, seed);
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
    { label: "Search history", hint: "find past work", run: () => openSearch() },
  ];
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
