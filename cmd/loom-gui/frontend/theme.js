// loom palette — "Blush": warm rose, light. Single source for the status
// colors (rail threads + status words) and the terminal theme. Chrome neutrals
// live in tokens.css :root.

export const palette = {
  needs_you: "#C56A4A",
  running: "#3E9A80",
  idle: "#8A8FC0",
  done: "#6FAE66",
  error: "#C64C40",
  unknown: "#A99CA2",
};

export function statusColor(status) {
  return palette[status] || palette.unknown;
}

// Human label for a status (the rail's right-hand word).
export function statusWord(status) {
  return status === "needs_you" ? "needs you" : (status || "unknown");
}

// Light terminal tuned for the Blush base — ANSI colors chosen for contrast on
// the warm ivory background, cursor in the peach accent.
export const xtermThemeLight = {
  background: "#FCF7F5",
  foreground: "#50454B",
  cursor: "#EE9E86",
  cursorAccent: "#FCF7F5",
  selectionBackground: "rgba(238,158,134,0.22)",
  black: "#50454B", red: "#C5604C", green: "#4E9C6A", yellow: "#B8863C",
  blue: "#5B7FB8", magenta: "#9A79B8", cyan: "#3E93A0", white: "#8A8090",
  brightBlack: "#B5A8AE", brightRed: "#C5604C", brightGreen: "#4E9C6A",
  brightYellow: "#B8863C", brightBlue: "#5B7FB8", brightMagenta: "#9A79B8",
  brightCyan: "#3E93A0", brightWhite: "#50454B",
};

// Warm-dark terminal (a dark cousin of Blush) — soft plum base, peach cursor,
// ANSI colors lifted for contrast on the dark ground.
export const xtermThemeDark = {
  background: "#241E22",
  foreground: "#E7DADE",
  cursor: "#EE9E86",
  cursorAccent: "#241E22",
  selectionBackground: "rgba(238,158,134,0.28)",
  black: "#3A3238", red: "#E8897A", green: "#7FC79E", yellow: "#D8B36A",
  blue: "#8FB0E0", magenta: "#C39AD8", cyan: "#6FC0CC", white: "#E7DADE",
  brightBlack: "#6E626A", brightRed: "#E8897A", brightGreen: "#7FC79E",
  brightYellow: "#D8B36A", brightBlue: "#8FB0E0", brightMagenta: "#C39AD8",
  brightCyan: "#6FC0CC", brightWhite: "#FBF5F3",
};

export function xtermThemeFor(mode) {
  return mode === "dark" ? xtermThemeDark : xtermThemeLight;
}

// Terminal-pane background (the padding area around xterm) per theme.
export const terminalBg = { light: "#FCF7F5", dark: "#241E22" };
