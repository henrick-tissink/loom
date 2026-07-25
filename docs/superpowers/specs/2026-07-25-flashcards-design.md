# Flashcards — learn a managed codebase inside loom

Status: design (awaiting review)
Date: 2026-07-25
Migrations claimed: **v19** (`flashcards`), **v20** (`flashcard_reviews`) — head is v18 (`internal/store/store.go`).

## 1. Goal

Let the user *work through the entire app and understand every part of it* — the
architecture and decisions they're proud of, and the code underneath — by
generating, curating, and reviewing spaced-repetition flashcards **inside loom**,
over any managed project.

Two intents, both served:

- **Comprehension** ("understand every part"): a coverage map over the project's
  parts and a "work through this part" review flow.
- **Retention** ("so I can work through it"): an SM-2 scheduler, plus export to
  Anki/CSV for anyone who wants daily review off a phone.

Audience is n=1 (the author). This is a real in-app feature, not a script — a
deliberate, reaffirmed choice (§13 records the risks that were weighed).

## 2. What the adversarial review changed

Four reviewers (pedagogy, loom-fit, product, correctness) pressure-tested an
earlier shape. Two returned FLAWED. This spec is the corrected design; the
corrections are load-bearing, so they're recorded here rather than lost:

1. **Generation is NOT a loom workflow and NOT a background job.** loom has
   exactly one headless `claude -p` (the hardened `internal/memory.Summarizer`)
   and an explicit principle (ARCHITECTURE.md §12, :539) that there is "no
   headless or scripted launch path." Generation is a new hardened `claude -p`
   modeled on the Summarizer: **human-triggered, previewed, one part at a time**
   (§5).
2. **A schema can't prove a card's answer is *true*.** Auto-authored `code`
   cards can teach falsehoods about the user's own code (the reviewer's proof:
   `status.Fuse` returns `Running` in *every* branch when the pane is active — a
   shallow card gets it wrong). A **cited-span + independent verify pass** gates
   every behavioral card (§6).
3. **Preserving an SM-2 schedule across an *answer* change entrenches the wrong
   answer.** An `answer_hash` forces a card **due now** when its answer changes
   (§8).
4. **"Mastery %" is a vanity metric**; a validated schema doesn't stop shallow
   cards. Replaced by a **measured pass-rate**, plus a **human curation gate**
   between generation and scheduling (§4, §9).
5. **Reuse, don't reinvent**: `internal/arch` (heading slugs), `internal/registry`
   /`internal/projects` (language-agnostic repo/dir enumeration — no `go list`),
   and the delegate trust boundary (child returns text; loom writes the db).

## 3. Coverage manifest — the spine

For a project, a **deterministic** pass builds a tree of *parts* so "every part"
is guaranteed by construction, not by trusting the model to be exhaustive:

- **Repos / directories** — from `internal/registry` + `internal/projects` at the
  granularity loom already models (root + `.git`-discovered repos, one nesting
  level). Language-agnostic; **no `go list`** (it would be the only Go-specific
  assumption in the app and would exclude non-Go projects).
- **Architecture sections** — `internal/arch` already parses docs into
  `Block{Kind: BlockHeading, Slug}`; reuse `arch.Documents`/`arch.Open`. Each
  heading slug is a part.
- **Specs / decisions** — the design specs discovered under the project (same
  `arch` machinery), each an authored decision with rationale.

The manifest is **loom-authored and derived on demand** (rebuilt from `arch` +
`registry`), not an agent-authored file. It is therefore trusted — unlike
delegate manifests (`<root>/.loom/manifests/`, read-only precisely because an
agent wrote them). Each node carries a target card mix and, at review time, its
measured pass-rate (§9).

## 4. Card model & types

A card is atomic — one recall target. Types describe pedagogical *shape*:

| type | shape | source | scheduled? |
|---|---|---|---|
| `concept` | "what is X / what is it for" | arch sections, dir/pkg docs | yes |
| `decision` | one claim **or** one tradeoff (split, never bundled) | specs | yes |
| `code` | a specific behavior/contract, e.g. "`Fuse` returns ___ when the pane is active" | code span | yes (verified) |
| `cloze` | fill-the-blank of a contract fact | code / arch | yes |
| `trace` | multi-step data-flow walk | lifecycle sections | **no** — unscored walkthrough |

Rules that keep cards good (pedagogy findings):

- **Recall-forcing stems only.** "Describe / what is" definitional stems are
  rejected at author time in favour of questions that demand a specific answer.
- **Atomicity enforced.** `decision` cards are split into atomic claim + tradeoff
  pairs; `cloze` is first-class for contract facts.
- **`trace` is not an SM-2 item.** A 5–8-step walk can't be honestly graded 1–4;
  traces render as an unscored "walkthrough" drill, or are decomposed into a
  chain of `cloze` step-cards.
- **Curation gate.** Claude *drafts*; the user keeps/edits/kills each card before
  it becomes active. Un-curated cards are never scheduled (they can't become due).

Every card stores a `source_ref` (`symbol@file` for `code`/`trace`, heading slug
for `concept`/`decision`), an `answer_hash`, and a stable `anchor` natural key
(§7) — **never** keyed on question text.

## 5. Generation — hardened `claude -p`, human-triggered

Modeled directly on `internal/memory/summarize.go` (the sanctioned, spike-verified
headless path):

- **Trigger**: an explicit, previewed user action — "Generate cards for
  *&lt;part&gt;*" — **one part at a time**, never a background spray across a
  project. Mirrors the opt-in, one-at-a-time summarize discipline (§10, §12.4).
- **Argv (BINDING, reused verbatim)**: `claude -p <prompt> --model haiku
  --no-session-persistence --tools "" --strict-mcp-config --setting-sources ""
  --exclude-dynamic-system-prompt-sections`, `ScrubEnv` stripping
  `CLAUDECODE`/`CLAUDE_CODE_*`, context timeout (90s default).
- **Sandbox by construction**: the part's source text (the specific file span, the
  heading body, the spec section) is fed **in the prompt** — `--tools ""` means
  no file/tool access. The child emits card JSON on stdout; **loom** validates and
  writes rows. The child never touches `loom.db`.
- **Output schema**: strict JSON (required fields, type enum, `source_ref` span
  that must resolve). Low temperature.
- **Cost**: full first-run ≈ 50–60 nodes × (author + verify) on Haiku ≈ cents,
  one-time. Regeneration re-authors only stale cards (§8), so a typical commit
  costs cents, not dollars.

## 6. Correctness gate

Behavioral cards (`code`, `trace`, contract `cloze`) must pass before they are
storable. Non-negotiable — a confidently-wrong card is worse than no card.

1. **Cited span, resolves deterministically.** The card carries `symbol@file`
   (+ line span). A validator confirms the span resolves and that `source_hash`
   (§8) covers exactly it. Proves provenance, not truth.
2. **Independent verify pass.** A *second* hardened `claude -p` (same sandboxed
   argv) is given **only** the cited span + the card's question, answers from
   scratch, and its answer is compared to the stored answer. Mismatch → **reject,
   don't store**.
3. **Prefer deterministic extraction** where cheap and exact — e.g. function
   signatures via `go/ast`/`go/types` for the Go repos — over free-authoring.
4. **Malformed / off-topic** → bounded retry (≤2, feeding the validation error
   back); still bad → the node is marked **`gen_failed` visibly** (mirrors
   `seed_status` / `· seed FAILED`), never stored partial.

`concept`/`decision` cards (opinion/rationale, not verifiable behavior) skip the
verify pass but still pass the curation gate (§4).

## 7. Storage (`loom.db`)

**v19 — `flashcards`** (`CREATE TABLE IF NOT EXISTS`):

| col | note |
|---|---|
| `id` | surrogate PK |
| `project` | project key |
| `part` | manifest node (repo/dir path, heading slug, or spec) |
| `anchor` | **stable natural key**: `project + type + normalized source path + slug/symbol`. Never card text. |
| `type` | `concept`\|`decision`\|`code`\|`cloze`\|`trace` |
| `front`, `back` | card text |
| `source_ref` | `symbol@file:span` or heading slug |
| `source_hash` | structural hash of the anchored unit (§8) |
| `answer_hash` | hash of `back` — drives re-learn on change (§8) |
| `status` | `draft`\|`active`\|`gen_failed`\|`stale`\|`suspended` |
| `created_at`, `curated_at` | |

**v20 — `flashcard_reviews`** (`CREATE TABLE IF NOT EXISTS`): `card_id`, `ease`,
`interval`, `due_at`, `reps`, `lapses`, `last_grade`, `last_reviewed`.

House rule respected: `CREATE TABLE IF NOT EXISTS` on both; if any `ALTER` is ever
needed it takes its own slot. Spec written against head **v18**; if another
in-flight spec lands first, renumber (the orchestrator/delegation v10/v11
collision is the cautionary precedent).

## 8. Staleness & regeneration

- **Structural anchor, not file hash.** `source_hash` is the hash of the
  **gofmt-normalized, comment-stripped declaration body** keyed by qualified
  symbol (`pkg.Type.Method`) via `go/ast` for Go; heading-slug + body hash for
  arch/spec nodes. Whitespace/comment edits and cross-file moves → no churn; a
  rename **orphans** the old card and flags it, rather than silently re-keying
  (mirrors the memory index keeping `file_missing` rows, ARCHITECTURE.md §6.2).
- **Answer-change forces re-learn.** On regen: if `answer_hash` is byte-identical
  (a pure question reword), keep the review row. If the answer changed → set the
  card **due now**, reset ease to default, flag "answer updated." This is the fix
  for the "mastered wrong answer hidden behind a 60-day interval" blocker.
- **No semantic dedup.** Embedding-similarity auto-drop deletes *distinct* cards
  (two `Fuse` branches sit at ~0.9 cosine but teach different things). Dedup key
  is **exact**: same `anchor` **and** normalized-question-stem hash. True
  restatements collapse; anything else is *flagged for review*, never auto-deleted
  (loom principle: visible bad state over silent drop).
- **Known false-negative** (stated, not hidden): a textual/structural hash can't
  see when an *unchanged* body's behavior shifts because a callee changed. First-
  order callee signatures are folded into the hash to narrow this; residual
  behavioral staleness is a documented limit (§13).

## 9. Scheduling & the honest metric

- **SM-2 for v1** (proven, small). FSRS is a fast-follow — its retrievability
  estimate is also what a truthful mastery number would require, so it's budgeted
  "sooner than later," not "someday."
- **No "mastery %".** Each manifest node reports a **measured rolling pass-rate on
  *due* reviews** ("7/10 correct, 3 lapsing, 4 not yet due"). A node is never
  coloured "done" because cards *exist*.
- **SRS mechanics that make it actually work**: a per-day **new-card cap**;
  **sibling-interference** avoidance (shuffle so cards from one node don't
  cluster); **leech detection** → auto-suspend + flag for re-authoring (a repeated
  lapse is often a signal the card is vague — a quality feedback loop).
- **Relevance weighting**: bias new-card introduction toward the node the user is
  actively in (loom knows the session cwd) and offer a "make a card from this"
  path so real confusions become cards, not just manifest trivia.

## 10. GUI — a stage surface, not a fourth mode

The GUI just moved to three panes (projects · threads · stage, 2026-07-24). A
study session is neither a project nor a thread, so it does **not** become a
fourth top-level nav mode. Instead, Study is a **stage surface reached from a
project overview** (the same way Attention is a pinned stage surface), keeping the
project→thread→stage axis intact.

Two views on that surface:

- **Coverage** — the manifest tree with per-node pass-rate and card counts;
  "Generate cards for this part" (previewed) and "Work through this part."
- **Review session** — due cards → flip → grade 1–4 → SM-2 update; curation
  actions (keep/edit/kill) for `draft` cards; unscored walkthrough for `trace`.

**Export**: one-click Anki (`.apkg`/CSV) and Markdown, so daily retention review
can live on a phone where the daily-due cadence actually fits — the in-loom pane
owns generation, curation, and comprehension "work-through"; the phone owns the
grind if the user wants it.

## 11. Alignment with loom's principles (§12)

- **No silent auto-anything**: generation is human-triggered and previewed; no
  background job, no auto-advance, no auto-inject.
- **Quota discipline**: one part at a time, Haiku, opt-in — same posture as
  summarization ("the ONE outbound LLM call").
- **Visible failure over silent drop**: `gen_failed` nodes, flagged orphans/leech,
  no semantic auto-delete.
- **Trust boundary**: the LLM child is sandboxed and returns text; only loom
  writes `loom.db`.

## 12. Build order

1. **Manifest + storage + generation + correctness gate** — `internal/flashcards`:
   deterministic manifest (reuse `arch`/`registry`); hardened `claude -p` author +
   verify passes; v19/v20 tables. Provable headless: a CLI subcommand generates and
   stores `draft` cards for a part (verified, awaiting curation), with the verify
   pass rejecting wrong ones. No GUI yet.
2. **SM-2 + review loop + curation** — scheduler, pass-rate metric, new-card cap,
   leech/interference handling; curation state machine (`draft`→`active`).
3. **GUI Study surface** — coverage view + review session off the project overview.
4. **Export** — Anki/CSV/Markdown.
5. **Staleness/regen** — structural anchors, `answer_hash` re-learn, orphan flagging.

FSRS and relevance-weighting are post-v1.

## 13. Known limits & risks (stated plainly)

- **Surface mismatch / abandonment risk** (product reviewer, FLAWED): a desktop
  dev tool is an imperfect surface for daily-due SRS, and n=1 self-built study
  systems have a high abandonment rate. Mitigations chosen over abandoning the
  feature: Anki export (retention can leave loom), comprehension "work-through"
  (value on first pass, before any daily habit forms), and a staged build so
  little is sunk before the review loop proves itself. The risk is accepted, not
  dismissed.
- **Behavioral staleness false-negative** (§8): a card can go silently stale when a
  callee's behavior changes but the cited body doesn't. Narrowed, not eliminated.
- **`concept`/`decision` cards are unverified** by design (no ground truth for
  rationale); the curation gate is their only quality control.
