// Pure helpers for review comments on the diff view (comments.go keeps
// them). No DOM here, so node --test can run them.

// numberPatch splits one file's chunk of a unified diff into lines with
// their kind and line number. Added and context lines number in the new
// file (side "new"); removed lines in the old one (side "old"). Header and
// hunk lines carry no number and cannot be commented on.
export function numberPatch(body) {
  const out = [];
  let oldN = 0, newN = 0, inHunk = false;
  for (const text of String(body).split("\n")) {
    const h = text.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
    if (h) {
      oldN = +h[1]; newN = +h[2]; inHunk = true;
      out.push({ text, kind: "hunk" });
      continue;
    }
    if (!inHunk || text.startsWith("diff --git ")) {
      inHunk = false;
      out.push({ text, kind: "meta" });
      continue;
    }
    if (text.startsWith("+")) out.push({ text, kind: "add", side: "new", line: newN++ });
    else if (text.startsWith("-")) out.push({ text, kind: "del", side: "old", line: oldN++ });
    else if (text.startsWith(" ")) { out.push({ text, kind: "ctx", side: "new", line: newN++ }); oldN++; }
    else out.push({ text, kind: "meta" }); // "\ No newline at end of file", trailing ""
  }
  return out;
}

// code is a diff line without its +/-/space mark.
export function code(l) { return l.text.slice(1); }

// selectRange is the commentable lines from index i to j (either order),
// all on one side: a range starting on a removed line keeps to removed
// lines, else to new-file lines. null when i is not commentable.
export function selectRange(lines, i, j) {
  const a = lines[i];
  if (!a || !a.side) return null;
  const [lo, hi] = i <= j ? [i, j] : [j, i];
  const picked = [];
  for (let k = lo; k <= hi; k++) if (lines[k] && lines[k].side === a.side) picked.push(k);
  if (!picked.length) return null;
  const first = lines[picked[0]], last = lines[picked[picked.length - 1]];
  return {
    side: a.side, line: first.line, endLine: last.line,
    quote: picked.map((k) => code(lines[k])).join("\n"),
    from: picked[0], to: picked[picked.length - 1],
  };
}

// anchor places each comment on this file's lines. A comment sits where
// its side and first line still show the code it quoted; failing that, on
// the nearest line of that side with the same code (the lines moved);
// failing that it is outdated. Returns { at: Map(index -> comments),
// outdated: comments }.
export function anchor(lines, comments) {
  const at = new Map();
  const outdated = [];
  for (const c of comments) {
    const first = String(c.quote || "").split("\n")[0];
    let best = -1, dist = Infinity;
    lines.forEach((l, k) => {
      if (l.side !== c.side) return;
      if (code(l) !== first && !(first === "" && !c.quote)) return;
      const d = Math.abs(l.line - c.line);
      if (d < dist) { best = k; dist = d; }
    });
    if (best < 0) { outdated.push(c); continue; }
    // the comment shows under the last line of its range
    let end = best;
    const span = (c.endLine || c.line) - c.line;
    for (let k = best + 1; k < lines.length && span > 0; k++) {
      if (lines[k].side !== c.side) continue;
      if (lines[k].line > lines[best].line + span) break;
      end = k;
    }
    if (!at.has(end)) at.set(end, []);
    at.get(end).push(c);
  }
  return { at, outdated };
}

// hash is a short FNV-1a hash of a file's patch: a viewed mark holds it,
// and a different hash means the file changed since it was viewed.
export function hash(s) {
  let h = 0x811c9dc5;
  const str = String(s);
  for (let i = 0; i < str.length; i++) {
    h ^= str.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h.toString(16).padStart(8, "0") + ":" + str.length.toString(16);
}

// viewedKey matches comments.go's key for a viewed file: per diff scope,
// since the two scopes' patches of one file differ.
export function viewedKey(scope, repo, file) { return scope + "\u0000" + repo + "\u0000" + file; }

// unshownNew is the new files a patch leaves out (Against shows new files'
// contents only up to a size and count), so the view can still list them.
export function unshownNew(untracked, shownFiles) {
  const shown = new Set(shownFiles);
  return (untracked || []).filter((f) => !shown.has(f));
}

// findingKey matches comments.go's FindingKey.
export function findingKey(f) { return `${f.file}:${f.line || 0}:${f.summary}`; }
