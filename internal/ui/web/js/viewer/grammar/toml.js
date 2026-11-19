// TOML scanner of the viewer: table headers, keys, strings of all four kinds, numbers, dates and
// times (as numbers), booleans, comments, arrays and inline tables. Brackets and braces of values
// pair; a table folds from its header to its last line that is not blank, and arrays, inline tables
// and strings over several lines fold. The scanner never throws.

import { Result, Opens } from "./result.js";

const DATE = /[0-9]{4}-[0-9]{2}-[0-9]{2}(?:[Tt ][0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?)?(?:[Zz]|[+-][0-9]{2}:[0-9]{2})?|[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?/y;
const NUMBER = /[+-]?(?:0x[0-9A-Fa-f_]+|0o[0-7_]+|0b[01_]+|inf|nan|[0-9][0-9_]*(?:\.[0-9_]+)?(?:[eE][+-]?[0-9_]+)?)/y;
const BARE = /[A-Za-z0-9_-]+/y;
const WORD = /[A-Za-z]+/y;

export function scan(text) {
  const r = new Result(text);
  const n = text.length;
  const stack = new Opens(); // open "[" or "{" of values by that character: { at }
  let i = 0;
  let lineStart = true; // only blanks since the last line end
  let expectKey = true;
  let header = -1; // offset of the current table header
  let lastContent = -1; // offset of the last character that is no blank, for the fold of a table

  const closeTable = () => {
    if (header >= 0 && lastContent >= 0) r.fold(header, lastContent);
  };

  // A string from i: basic, literal, or one of their multi line forms. Returns its end.
  const string = (from) => {
    const q = text[from];
    if (text.startsWith(q.repeat(3), from)) {
      let close = text.indexOf(q.repeat(3), from + 3);
      if (q === '"') {
        while (close > 0 && text[close - 1] === "\\") close = text.indexOf(q.repeat(3), close + 1);
      }
      let stop = close < 0 ? n : close + 3;
      for (let extra = 0; close >= 0 && extra < 2 && stop < n && text[stop] === q; extra += 1) stop += 1; // up to two quotes end the content
      r.token(from, stop, "string");
      r.fold(from, stop - 1);
      return stop;
    }
    let j = from + 1;
    while (j < n && text[j] !== q && text[j] !== "\n") j += q === '"' && text[j] === "\\" ? 2 : 1;
    const stop = j < n && text[j] === q ? j + 1 : Math.min(j, n);
    r.token(from, stop, "string");
    return stop;
  };

  // Dotted key segments from i, as tokens of the given kind. Returns the end.
  const dotted = (from, kind) => {
    let j = from;
    while (j < n) {
      while (j < n && (text[j] === " " || text[j] === "\t")) j += 1;
      const c = text[j];
      if (c === '"' || c === "'") {
        const start = j;
        let k = j + 1;
        while (k < n && text[k] !== c && text[k] !== "\n") k += c === '"' && text[k] === "\\" ? 2 : 1;
        j = k < n && text[k] === c ? k + 1 : Math.min(k, n);
        r.token(start, j, kind);
      } else {
        BARE.lastIndex = j;
        const m = BARE.exec(text);
        if (!m) break;
        r.token(j, j + m[0].length, kind);
        j += m[0].length;
      }
      let k = j;
      while (k < n && (text[k] === " " || text[k] === "\t")) k += 1;
      if (text[k] === ".") {
        r.token(k, k + 1, "punct");
        j = k + 1;
      } else {
        j = k;
        break;
      }
    }
    return j;
  };

  while (i < n) {
    const c = text[i];
    if (c === "\n") {
      if (!stack.length) expectKey = true;
      lineStart = true;
      i += 1;
      continue;
    }
    if (c === " " || c === "\t" || c === "\r") {
      i += 1;
      continue;
    }
    if (c === "#") {
      let end = text.indexOf("\n", i);
      if (end < 0) end = n;
      r.token(i, end, "comment");
      lastContent = Math.max(lastContent, end - 1);
      i = end;
      continue;
    }
    if (lineStart && !stack.length && c === "[") {
      closeTable();
      header = i;
      lastContent = -1;
      const double = text[i + 1] === "[";
      r.token(i, i + (double ? 2 : 1), "punct");
      let j = dotted(i + (double ? 2 : 1), "heading");
      while (j < n && (text[j] === " " || text[j] === "\t")) j += 1;
      if (text.startsWith(double ? "]]" : "]", j)) {
        r.token(j, j + (double ? 2 : 1), "punct");
        j += double ? 2 : 1;
      }
      lastContent = j - 1;
      lineStart = false;
      expectKey = true;
      i = j;
      continue;
    }
    lineStart = false;
    lastContent = i;
    if (expectKey && c !== "}" && c !== "]") {
      const j = dotted(i, "key");
      if (j > i) {
        lastContent = j - 1;
        expectKey = false;
        i = j;
        continue;
      }
    }
    if (c === "=") {
      r.token(i, i + 1, "punct");
      expectKey = false;
      i += 1;
      continue;
    }
    if (c === '"' || c === "'") {
      i = string(i);
      lastContent = i - 1;
      continue;
    }
    if (c === "[" || c === "{") {
      r.token(i, i + 1, "punct");
      stack.push(c, { at: i });
      expectKey = c === "{";
      i += 1;
      continue;
    }
    if (c === "]" || c === "}") {
      r.token(i, i + 1, "punct");
      const want = c === "]" ? "[" : "{";
      const open = stack.close(want);
      if (open) {
        r.pair(open.at, open.at + 1, i, i + 1);
        r.fold(open.at, i);
      }
      expectKey = false;
      i += 1;
      continue;
    }
    if (c === ",") {
      r.token(i, i + 1, "punct");
      expectKey = stack.length > 0 && stack.top().key === "{";
      i += 1;
      continue;
    }
    DATE.lastIndex = i;
    const date = DATE.exec(text);
    if (date) {
      r.token(i, i + date[0].length, "number");
      i += date[0].length;
      lastContent = i - 1;
      continue;
    }
    NUMBER.lastIndex = i;
    const number = NUMBER.exec(text);
    if (number) {
      r.token(i, i + number[0].length, "number");
      i += number[0].length;
      lastContent = i - 1;
      continue;
    }
    WORD.lastIndex = i;
    const word = WORD.exec(text);
    if (word) {
      if (word[0] === "true" || word[0] === "false") r.token(i, i + word[0].length, "keyword");
      i += word[0].length;
      lastContent = i - 1;
      continue;
    }
    i += 1;
  }
  closeTable();
  return r.done();
}
