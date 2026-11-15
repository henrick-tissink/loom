import { test } from "node:test";
import assert from "node:assert/strict";
import { numberPatch, selectRange, anchor, hash, findingKey, viewedKey, unshownNew } from "./diffcomments.js";

const patch = [
  "diff --git a/a.go b/a.go",
  "index 1..2 100644",
  "--- a/a.go",
  "+++ b/a.go",
  "@@ -10,4 +10,5 @@ func x() {",
  " keep",
  "-old one",
  "-old two",
  "+new one",
  "+new two",
  "+new three",
  " tail",
  "\\ No newline at end of file",
  "",
].join("\n");

test("numberPatch numbers each side", () => {
  const ls = numberPatch(patch);
  const pick = (t) => ls.find((l) => l.text === t);
  assert.equal(pick("--- a/a.go").kind, "meta");
  assert.equal(pick("+++ b/a.go").side, undefined, "the +++ header is not an added line");
  assert.deepEqual([pick(" keep").side, pick(" keep").line], ["new", 10]);
  assert.deepEqual([pick("-old one").side, pick("-old one").line], ["old", 11]);
  assert.deepEqual([pick("-old two").line], [12]);
  assert.deepEqual([pick("+new one").line, pick("+new three").line], [11, 13]);
  assert.equal(pick(" tail").line, 14);
  assert.equal(pick("\\ No newline at end of file").side, undefined);
});

test("selectRange keeps to one side and quotes the code", () => {
  const ls = numberPatch(patch);
  const i = ls.findIndex((l) => l.text === "+new three");
  const j = ls.findIndex((l) => l.text === " keep");
  const r = selectRange(ls, i, j); // backwards, across removed lines
  assert.equal(r.side, "new");
  assert.equal(r.line, 10);
  assert.equal(r.endLine, 13);
  assert.equal(r.quote, "keep\nnew one\nnew two\nnew three");
  const d = selectRange(ls, ls.findIndex((l) => l.text === "-old one"), i);
  assert.equal(d.side, "old");
  assert.equal(d.quote, "old one\nold two");
  assert.equal(selectRange(ls, 0, 3), null, "a header line takes no comment");
});

test("anchor finds moved lines and reports outdated ones", () => {
  const ls = numberPatch(patch);
  const { at, outdated } = anchor(ls, [
    { id: "same", side: "new", line: 11, endLine: 12, quote: "new one\nnew two" },
    { id: "moved", side: "new", line: 40, endLine: 40, quote: "tail" },
    { id: "gone", side: "new", line: 11, endLine: 11, quote: "not here" },
    { id: "old", side: "old", line: 12, endLine: 12, quote: "old two" },
  ]);
  const where = (id) => [...at].find(([, cs]) => cs.some((c) => c.id === id))?.[0];
  assert.equal(ls[where("same")].text, "+new two", "a range shows under its last line");
  assert.equal(ls[where("moved")].text, " tail");
  assert.equal(ls[where("old")].text, "-old two");
  assert.deepEqual(outdated.map((c) => c.id), ["gone"]);
});

test("hash changes with the patch", () => {
  assert.equal(hash(patch), hash(patch));
  assert.notEqual(hash(patch), hash(patch + "+x\n"));
  assert.equal(findingKey({ file: "a.go", line: 3, summary: "s" }), "a.go:3:s");
  assert.equal(findingKey({ file: "a.go", summary: "s" }), "a.go:0:s");
});

test("viewed keys are per scope; unshown new files are listed", () => {
  assert.notEqual(viewedKey("since", "/r", "a.go"), viewedKey("uncommitted", "/r", "a.go"));
  assert.deepEqual(unshownNew(["big.lock", "small.go"], ["small.go", "a.go"]), ["big.lock"]);
  assert.deepEqual(unshownNew(null, []), []);
});
