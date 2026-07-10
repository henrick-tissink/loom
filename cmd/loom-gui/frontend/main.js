import "./tokens.css";
import "@xterm/xterm/css/xterm.css";
import { applyTokens, statusColor, xtermTheme } from "./theme.js";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";

applyTokens();

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
