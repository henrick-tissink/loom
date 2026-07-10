import "./tokens.css";
import { applyTokens, statusColor } from "./theme.js";

applyTokens();

const threadsEl = document.getElementById("threads");
let activeName = null;

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

// Placeholder until the embedded terminal lands.
function selectSession(name) {
  activeName = name;
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
