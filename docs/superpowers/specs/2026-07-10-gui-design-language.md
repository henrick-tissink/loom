# Loom GUI — Design Language (North-Star)

**Date:** 2026-07-10
**Status:** Direction — informs the chrome spec (C). Not built by the walking skeleton.
**Visual reference:** a published mockup exists (warp rail + embedded terminal +
touched-files panel). This document is the durable, buildable version of it.

## Why this document exists

The walking skeleton is deliberately unstyled — its job is to de-risk plumbing,
not to be pretty. This document pins the aesthetic the GUI is aiming at, so
(a) the chrome spec has a north-star, and (b) the skeleton can honor a few
cheap structural seams now instead of a painful retrofit later (see
"Forward-compatibility seams" in the skeleton spec).

## Thesis

Loom is a *loom*: many live threads — `claude` sessions — held under one hand.
The design is a calm **instrument**, not a generic dashboard. The signature is
the **warp rail**: each session is a thread whose *color is its status*, and the
one thread that needs you carries a slow, breathing golden glow. Status is what
the product is fundamentally about, so **the palette is built from the status
system** rather than an arbitrary brand accent imposed on top.

Committed to a **single dark theme** — this is a terminal cockpit; a light mode
would be a costume, not a choice.

## Color

Warm-charcoal ground so amber sits naturally; chroma is spent almost entirely on
status, everything else is disciplined neutral.

| Token | Hex | Role |
|---|---|---|
| `--bg` | `#15141B` | app ground (warm charcoal, not black) |
| `--rail` | `#1A1922` | side columns |
| `--surface` / `--surface-2` | `#201E28` / `#262332` | raised rows, chips |
| `--hairline` | `#322E3C` | borders |
| `--text` / `--text-dim` / `--text-faint` | `#E9E5F0` / `#9C95A9` / `#645D72` | text ramp |

**Status = identity** (these are the only saturated hues in the UI):

| Status | Hex | Feel |
|---|---|---|
| `needs_you` | `#F5B14C` | the golden thread — the hero, the one loud color |
| `running` | `#56C6A9` | calm live activity |
| `idle` | `#7E8AA6` | present but quiet |
| `done` | `#7FA98A` | resolved, recedes |
| `error` | `#E06A5E` | warm coral, sparing |
| `unknown` | `#565065` | dimmest |

`--accent` **is** `needs_you` amber. Semantic status color and the brand accent
are the same thing on purpose — attention is the brand.

## Typography

- **UI face:** `system-ui` stack. Personality comes from a strict type scale,
  600-weight tracked uppercase micro-labels (`letter-spacing: 0.16–0.18em`), and
  restraint — not from a novelty display face.
- **Mono face (identity):** every session name, status, timestamp, file path, and
  the terminal itself is monospace, so the whole app reads as one instrument.
  Skeleton uses the `ui-monospace` system stack; **the real build should bundle a
  distinctive programming mono** (e.g. JetBrains Mono / Commit Mono / Berkeley
  Mono) via `@font-face` so terminals look identical across machines.
- Type roles live behind `--font-ui` / `--font-mono` so the bundled faces drop in
  without touching components.

## Layout

Frameless window (own titlebar, macOS traffic-light inset) → a three-column body:

```
┌ titlebar: lights · loom [woven ticks] ~/Sauce · [N needs you] + / ┐
├──────────┬─────────────────────────────┬──────────────────────────┤
│  WARP    │        TERMINAL STAGE       │       CONTEXT            │
│  RAIL    │  head: name · proj · status │  ask / outcome           │
│          │        · open-in-editor     │  TOUCHED FILES (click →) │
│ needs↑   │                             │  activity timeline       │
│ running  │  embedded xterm.js,         │                          │
│ quiet    │  themed to match palette    │  (collapsible)           │
└──────────┴─────────────────────────────┴──────────────────────────┘
```

- **Warp rail** — attention-sorted: `needs_you` pinned top, then `running`, then a
  `quiet` group (idle/done/error). Each row: a status-colored thread on the left
  edge, mono name, dim project, status word, relative time. The active session
  gets a matching thread on its right edge.
- **Terminal stage** — slim header (name, project chip, status pill, `Open in
  editor` / `Detach`), then the embedded terminal filling the rest. The terminal
  is themed to the palette (coordinated ANSI set, amber cursor, warm-charcoal
  background) so it reads as native, never a bolted-on console.
- **Context panel** — the session's ask/outcome, **touched files as click-to-open
  rows** (path · line · `open ↗` on hover), and a short activity timeline.
  Collapsible; hidden under ~900px.

## Motion

One earned moment: the `needs_you` thread **breathes** (~3.4s amber glow) because
that is the single event worth pulling the eye. Everything else is still —
status changes cross-fade the thread color, attach fades in, the terminal cursor
blinks. All motion gated behind `prefers-reduced-motion`.

## File links (feeds spec D)

Two surfaces, one action — open in the configured external editor at the line:
1. **In the terminal** — paths in claude's output are detected and made clickable
   (xterm.js link provider matching `path[:line[:col]]`).
2. **In loom's own views** — the touched-files rows and search snippets are
   already structured data; each file is a click target.

## Signature, restated

If loom is remembered for one thing, it is the **warp of live threads** down the
left edge — status made physical as colored threads, with a single golden one
that breathes when an agent needs you. Everything else stays quiet so that reads
instantly.
