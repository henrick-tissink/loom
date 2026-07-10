import "./tokens.css";
import "@xterm/xterm/css/xterm.css";
import { statusColor, statusWord, xtermTheme } from "./theme.js";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";

const threadsEl = document.getElementById("threads");
const attnEl = document.getElementById("attn");
let activeName = null;
let latestSessions = [];

function renderAttention(sessions) {
  const n = sessions.filter((s) => s.status === "needs_you").length;
  if (n > 0) {
    attnEl.innerHTML = `<span class="attn-dot"></span>${n} ${n === 1 ? "needs" : "need"} you`;
  } else {
    attnEl.textContent = "";
  }
}

document.getElementById("new-session").addEventListener("click", openLauncher);

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

// ---- rail (status-grouped, attention first) ----
const GROUPS = [
  { key: "Needs you", match: (s) => s === "needs_you" },
  { key: "Running", match: (s) => s === "running" },
  { key: "Quiet", match: (s) => s !== "needs_you" && s !== "running" },
];

function renderThreads(sessions) {
  threadsEl.replaceChildren();
  for (const g of GROUPS) {
    const rows = sessions.filter((s) => g.match(s.status));
    if (!rows.length) continue;
    const gh = document.createElement("li");
    gh.className = "group";
    gh.textContent = g.key;
    threadsEl.appendChild(gh);
    for (const s of rows) {
      const li = document.createElement("li");
      li.className = "thread" + (s.name === activeName ? " active" : "");
      const color = statusColor(s.status);
      li.style.setProperty("--tc", color);
      li.innerHTML =
        `<span class="tglyph i-${s.status}">${icon(STATUS_ICON[s.status] || STATUS_ICON.unknown, 3)}</span>` +
        `<span class="tinfo"><span class="tname">${esc(s.name)}</span><span class="tproj">${esc(s.project)}</span></span>` +
        `<span class="tstatus" style="color:${color}">${esc(statusWord(s.status))}</span>`;
      li.addEventListener("click", () => selectSession(s.name));
      threadsEl.appendChild(li);
    }
  }
}

function renderStageHeader(name) {
  const el = document.getElementById("stage-header");
  if (!el) return;
  const s = latestSessions.find((x) => x.name === name);
  const status = s ? s.status : "unknown";
  const project = s ? s.project : "";
  const color = statusColor(status);
  el.className = "stage-head";
  el.innerHTML =
    `<span class="sh-name">${esc(name)}</span>` +
    (project ? `<span class="sh-proj">${icon(FOLDER_ICON, 2)}${esc(project)}</span>` : "") +
    `<span class="sh-pill"><i style="background:${color}"></i><span style="color:${color}">${esc(statusWord(status))}</span></span>`;
}

// ---- terminal ----
let term = null;
let fit = null;
let dataUnsub = null;
let exitUnsub = null;

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

  const stage = document.getElementById("stage");
  stage.replaceChildren();
  const header = document.createElement("div");
  header.id = "stage-header";
  stage.appendChild(header);
  const host = document.createElement("div");
  host.id = "terminal";
  stage.appendChild(host);
  renderStageHeader(name);
  renderThreads(latestSessions); // reflect active highlight immediately

  term = new Terminal({
    fontFamily: getComputedStyle(document.documentElement).getPropertyValue("--font-mono"),
    fontSize: 13,
    theme: xtermTheme,
    cursorBlink: true,
  });
  fit = new FitAddon();
  term.loadAddon(fit);
  term.open(host);
  fit.fit();

  dataUnsub = window.runtime.EventsOn("pty:data:" + name, (b64) => term.write(b64ToBytes(b64)));
  exitUnsub = window.runtime.EventsOn("pty:exit:" + name, () => {
    term.write("\r\n\x1b[2m[session ended]\x1b[0m\r\n");
  });
  term.onData((data) => window.go.main.App.SendInput(name, data));

  window.go.main.App.AttachSession(name)
    .then(() => {
      fit.fit();
      window.go.main.App.ResizeSession(name, term.cols, term.rows);
    })
    .catch((e) => term.write("\r\n\x1b[31mattach failed: " + e + "\x1b[0m\r\n"));

  window.addEventListener("resize", onResize);
}

function onResize() {
  if (!term || !fit || !activeName) return;
  fit.fit();
  window.go.main.App.ResizeSession(activeName, term.cols, term.rows);
}

// ---- poll ----
async function poll() {
  try {
    const sessions = await window.go.main.App.ListSessions();
    latestSessions = sessions;
    renderThreads(sessions);
    renderAttention(sessions);
    if (activeName) renderStageHeader(activeName);
  } catch (e) {
    console.error("ListSessions failed", e);
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
  let projects = [];
  try {
    projects = await window.go.main.App.ListProjects();
  } catch (e) {
    console.error("ListProjects failed", e);
  }

  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal" role="dialog" aria-label="New session">
      <h2>New session</h2>
      <div class="field">
        <label for="f-project">Project</label>
        <select id="f-project">${projects.map((p) => `<option value="${esc(p.path)}">${esc(p.label)}</option>`).join("")}</select>
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
