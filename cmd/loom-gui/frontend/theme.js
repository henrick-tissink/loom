// The single source of truth for loom's palette. Writes CSS custom properties
// at boot and exports the xterm theme + status→color map so the rail and the
// terminal can never disagree.
export const palette = {
  bg: "#15141B", rail: "#1A1922", surface: "#201E28", hairline: "#322E3C",
  text: "#E9E5F0", textDim: "#9C95A9", textFaint: "#645D72",
  needs_you: "#F5B14C", running: "#56C6A9", idle: "#7E8AA6",
  done: "#7FA98A", error: "#E06A5E", unknown: "#565065",
  termBg: "#121118",
};

export function statusColor(status) {
  return palette[status] || palette.unknown;
}

export function applyTokens() {
  const r = document.documentElement.style;
  r.setProperty("--bg", palette.bg);
  r.setProperty("--rail", palette.rail);
  r.setProperty("--surface", palette.surface);
  r.setProperty("--hairline", palette.hairline);
  r.setProperty("--text", palette.text);
  r.setProperty("--text-dim", palette.textDim);
  r.setProperty("--text-faint", palette.textFaint);
  r.setProperty("--needs", palette.needs_you);
  r.setProperty("--running", palette.running);
  r.setProperty("--font-ui", 'system-ui, -apple-system, "Segoe UI", sans-serif');
  r.setProperty("--font-mono", 'ui-monospace, "SF Mono", "JetBrains Mono", Menlo, monospace');
}

export const xtermTheme = {
  background: palette.termBg,
  foreground: "#C9C3D4",
  cursor: palette.needs_you,
  selectionBackground: "rgba(245,177,76,0.25)",
};
