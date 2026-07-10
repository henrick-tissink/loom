import "./tokens.css";
import "@xterm/xterm/css/xterm.css";
import { applyTokens, statusColor, xtermTheme } from "./theme.js";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";

applyTokens();

document.getElementById("new-session").addEventListener("click", openLauncher);

const threadsEl = document.getElementById("threads");
let activeName = null;

let term = null;
let fit = null;
let dataUnsub = null;
let exitUnsub = null;

function renderThreads(sessions) {
  threadsEl.replaceChildren();
  for (const s of sessions) {
    const li = document.createElement("li");
    li.className = "thread" + (s.name === activeName ? " active" : "");
    li.style.setProperty("--tc", statusColor(s.status));
    li.innerHTML =
      `<span><span class="name">${s.name}</span> ` +
      `<span class="proj">${s.project}</span></span>` +
      `<span class="st">${s.status.replace("_", " ")}</span>`;
    li.addEventListener("click", () => selectSession(s.name));
    threadsEl.appendChild(li);
  }
}

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
  const host = document.createElement("div");
  host.id = "terminal";
  stage.appendChild(host);

  term = new Terminal({
    fontFamily:
      getComputedStyle(document.documentElement).getPropertyValue("--font-mono"),
    fontSize: 13,
    theme: xtermTheme,
    cursorBlink: true,
  });
  fit = new FitAddon();
  term.loadAddon(fit);
  term.open(host);
  fit.fit();

  // Wails delivers our base64 payload as the first event arg.
  dataUnsub = window.runtime.EventsOn("pty:data:" + name, (b64) => {
    term.write(b64ToBytes(b64));
  });
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

async function poll() {
  try {
    const sessions = await window.go.main.App.ListSessions();
    renderThreads(sessions);
  } catch (e) {
    console.error("ListSessions failed", e);
  }
}

poll();
setInterval(poll, 1500);

const MODELS = [
  ["", "Default"], ["opus", "opus"], ["sonnet", "sonnet"], ["fable", "fable"],
];
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
        <select id="f-project">${projects.map((p) => `<option value="${p.path}">${p.label}</option>`).join("")}</select>
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
