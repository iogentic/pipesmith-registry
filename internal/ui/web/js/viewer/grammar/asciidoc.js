// AsciiDoc line scanner of the viewer, for the source: section titles, listing and literal blocks,
// comment lines and blocks, attribute entries, block attributes, inline code and list markers. A
// delimited block is one token per line and one fold; a section folds up to the next section of its
// level or a higher one. The scanner never throws.

import { Result } from "./result.js";
import { HeadingFolds, inlineCode } from "./lines.js";

const DELIMITERS = { "-": "code", ".": "code", "/": "comment" };

// An attribute entry, ":name:" with an optional value after white space, read by index so that a
// long line costs one pass: the end of the key and the range of the value, or null for no entry.
function attributeEntry(line) {
  if (line[0] !== ":" || line.length < 3 || line[1] === ":" || /\s/.test(line[1])) return null;
  const close = line.indexOf(":", 2);
  if (close < 0) return null;
  const key = close + 1;
  const rest = line.slice(key);
  const lead = rest.length - rest.trimStart().length;
  const value = rest.trim();
  if (value && lead === 0) return null;
  return { key, value: value ? [key + lead, key + lead + value.length] : null };
}

export function scan(text) {
  const r = new Result(text);
  const headings = new HeadingFolds(r);
  let block = null; // { line, delimiter, kind }
  for (let n = 0; n < r.starts.length; n += 1) {
    const start = r.starts[n];
    const end = r.lineEnd(n);
    const line = text.slice(start, end);
    if (block) {
      if (line) r.token(start, end, block.kind);
      headings.content(n);
      if (line === block.delimiter) {
        r.foldLines(block.line + 1, n + 1);
        block = null;
      }
      continue;
    }
    if (!line.trim()) continue;
    const delimiter = /^([-./])\1{3,}$/.exec(line);
    if (delimiter) {
      const kind = DELIMITERS[delimiter[1]];
      r.token(start, end, kind);
      block = { line: n, delimiter: line, kind };
      headings.content(n);
      continue;
    }
    const title = /^(={1,6})\s+\S/.exec(line);
    if (title) {
      r.token(start, end, "heading");
      headings.heading(n, title[1].length);
      continue;
    }
    headings.content(n);
    if (/^\/\/(?!\/)/.test(line)) {
      r.token(start, end, "comment");
      continue;
    }
    const attribute = attributeEntry(line);
    if (attribute) {
      r.token(start, start + attribute.key, "key");
      if (attribute.value) r.token(start + attribute.value[0], start + attribute.value[1], "string");
      continue;
    }
    if (/^\[[^\]]*\]\s*$/.test(line)) {
      r.token(start, start + line.trimEnd().length, "meta");
      continue;
    }
    const marker = /^(\s*)(\*+|\.+|-)(?=\s)/.exec(line);
    if (marker) r.token(start + marker[1].length, start + marker[1].length + marker[2].length, "punct");
    inlineCode(r, start, line, true);
  }
  if (block) r.foldLines(block.line + 1, r.starts.length);
  headings.end();
  return r.done();
}
