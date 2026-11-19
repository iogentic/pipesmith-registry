// Scanner of key value files: Java properties, cfg, ini and conf. A line that starts with "#", "!"
// or ";" is a comment. "[name]" at the start of a line is a section, which folds from its header to
// its last line that is not blank. Otherwise the key runs to the first "=", ":" or blank that no
// backslash escapes, and the value to the line end. A value line that ends with an odd number of
// backslashes goes on in the next line, and the lines fold. Unicode escapes are marked. The scanner
// never throws.

import { Result } from "./result.js";

const ESCAPE = /\\u[0-9A-Fa-f]{4}/g;

function blank(c) {
  return c === " " || c === "\t" || c === "\f";
}

export function scan(text) {
  const r = new Result(text);
  let section = -1; // line index of the current section header
  let lastContent = -1; // line index of the last line that is not blank

  // A span of key or value text with its unicode escapes as entity tokens.
  const piece = (start, end, kind) => {
    ESCAPE.lastIndex = 0;
    const part = text.slice(start, end);
    let at = 0;
    for (let m = ESCAPE.exec(part); m; m = ESCAPE.exec(part)) {
      r.token(start + at, start + m.index, kind);
      r.token(start + m.index, start + m.index + m[0].length, "entity");
      at = m.index + m[0].length;
    }
    r.token(start + at, end, kind);
  };

  const closeSection = () => {
    if (section >= 0 && lastContent > section) r.foldLines(section + 1, lastContent + 1);
  };

  let continued = -1; // line index where a value that goes on started
  for (let line = 0; line < r.starts.length; line += 1) {
    const start = r.starts[line];
    const end = r.lineEnd(line);
    let i = start;
    while (i < end && blank(text[i])) i += 1;
    if (continued >= 0) {
      if (i < end) piece(i, end, "string");
      if (i < end) lastContent = line;
      if (!oddBackslashes(text, start, end)) {
        r.foldLines(continued + 1, line + 1);
        continued = -1;
      }
      continue;
    }
    if (i >= end) continue;
    const c = text[i];
    const close = c === "[" ? text.indexOf("]", i + 1) : -1;
    if (close >= 0 && close < end) {
      closeSection();
      section = line;
      lastContent = line;
      r.token(i, i + 1, "punct");
      r.token(i + 1, close, "heading");
      r.token(close, close + 1, "punct");
      continue;
    }
    lastContent = line;
    if (c === "#" || c === "!" || c === ";") {
      r.token(i, end, "comment");
      continue;
    }
    let k = i;
    while (k < end && text[k] !== "=" && text[k] !== ":" && !blank(text[k])) k += text[k] === "\\" ? 2 : 1;
    k = Math.min(k, end);
    piece(i, k, "key");
    let v = k;
    while (v < end && blank(text[v])) v += 1;
    if (v < end && (text[v] === "=" || text[v] === ":")) {
      r.token(v, v + 1, "punct");
      v += 1;
      while (v < end && blank(text[v])) v += 1;
    }
    if (v < end) piece(v, end, "string");
    if (oddBackslashes(text, start, end)) continued = line;
  }
  if (continued >= 0) r.foldLines(continued + 1, r.starts.length);
  closeSection();
  return r.done();
}

function oddBackslashes(text, start, end) {
  let count = 0;
  for (let j = end - 1; j >= start && text[j] === "\\"; j -= 1) count += 1;
  return count % 2 === 1;
}
