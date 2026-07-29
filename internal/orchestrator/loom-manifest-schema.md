# Delegation manifest — the format you author

You are decomposing an intent into a **delegation manifest**: a JSON file at
`<projectRoot>/.loom/manifests/<name>.json`. Loom loads it and renders your
plan as approve-buttons. A human approves every child; you never launch one.

## First: should you write a manifest at all?

If the work does **not** split into clean, separable slices — especially slices
that live in different repos and can genuinely run in parallel — do **not**
invent splits. Write a one-line note that this is a **single-session job**, and
stop. One good session beats a badly-cut team. Only decompose when the seams
between slices can be written down as contracts *before* any code is written.

## The rules that make a manifest load

- `manifest`: `1`.
- `name`: must equal the filename stem — `atlas.json` ⇒ `"name": "atlas"`.
- `project`: the project's display name or its root path (from your brief's
  **Project** section).
- `repos`: a map of repo **label** → setup object. Labels must be ones your
  project owns (listed in your brief's **Project** section).
- `tasks[]`: each is one slice, owning exactly ONE repo:
  - `id`: lowercase-kebab, matching `^[a-z0-9-]{1,64}$`, unique — it becomes a
    git branch and directory name.
  - `repo`: a repo **label** your project owns.
  - `brief`: what this slice must do, in prose.
  - `authorization`: **required, non-empty** — exactly what this slice may and
    may not touch. Loom appends its own invariants; you write the task-specific
    half.
  - `check`: **required** — `{ "cmd": ["...", "..."] }`, an argv array (NOT a
    shell string) that exits `0` when the slice is correct. This is the slice's
    definition of *done*; there is no message that means done.
  - `produces[]`: the artifacts this slice hands off, each `{ id, kind, path }`.
    `kind` is `"file"` or `"interface"`; `path` is inside the slice's repo. A
    **seam** — something another slice needs — should produce an `"interface"`.
  - `needs[]`: artifact **IDs** (never task ids) this slice depends on. The
    slice does not start until those artifacts exist and pass their checks. Name
    an artifact for every dependency — one you cannot name an artifact for is one
    you have not specified.

## The seam (contract) between two slices

Where slice B depends on slice A, write the interface as a plain sentence in
**both** briefs (e.g. *"the api and web meet at `POST /export {docId}` → a
PDF"*), have A `produces` an `"interface"` artifact for it, and have B `needs`
that artifact. B then builds against the written contract in parallel — it
consumes A's finished, checked output, never A's live work.

## Worked example (this loads exactly as written)

```json
{
  "manifest": 1,
  "name": "example",
  "project": "atlas",
  "defaults": { "model": "sonnet" },
  "repos": { "api": {}, "web": {} },
  "tasks": [
    {
      "id": "pdf-endpoint",
      "title": "Add the PDF export endpoint",
      "repo": "api",
      "paths": ["internal/export/"],
      "brief": "Implement POST /export {docId} returning application/pdf.",
      "authorization": "May edit internal/export/* in the api repo; may not touch other packages or the web repo.",
      "produces": [
        {
          "id": "export-endpoint",
          "kind": "interface",
          "path": "internal/export/endpoint.go",
          "fingerprint": ["sh", "-c", "grep -c 'func ExportPDF' internal/export/endpoint.go"]
        }
      ],
      "check": { "cmd": ["go", "test", "./internal/export/..."] }
    },
    {
      "id": "export-button",
      "title": "Add the export button in the web app",
      "repo": "web",
      "paths": ["src/export/"],
      "brief": "Add an Export-to-PDF button that calls POST /export {docId} and downloads the result.",
      "authorization": "May edit src/export/* in the web repo; may not touch the api repo.",
      "needs": ["export-endpoint"],
      "check": { "cmd": ["npm", "run", "test", "--", "src/export"] }
    }
  ]
}
```
