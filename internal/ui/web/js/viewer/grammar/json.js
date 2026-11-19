// JSON scanner of the viewer: keys, strings, numbers, true, false and null, and the punctuation.
// A string followed by a colon is a key. Brackets pair by a stack; a closing bracket that matches no
// open one stays unpaired. A string without its closing quote ends at its line end. Folds for a
// pair of brackets on different lines. The scanner never throws.

import { Result, Opens } from "./result.js";

const NUMBER = /-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/y;
const WORD = /[A-Za-z_][A-Za-z0-9_]*/y;
const KEYWORDS = new Set(["true", "false", "null"]);
const OPEN = { 123: 125, 91: 93 }; // { -> }, [ -> ]

export function scan(text) {
  const r = new Result(text);
  const n = text.length;
  const stack = new Opens(); // by the code of the closing bracket: { at }
  let i = 0;
  while (i < n) {
    const c = text.charCodeAt(i);
    if (c === 34) {
      let j = i + 1;
      while (j < n) {
        const d = text.charCodeAt(j);
        if (d === 92) j += 2;
        else if (d === 34 || d === 10 || d === 13) break;
        else j += 1;
      }
      const stop = j < n && text.charCodeAt(j) === 34 ? j + 1 : Math.min(j, n);
      let k = stop;
      while (k < n && (text.charCodeAt(k) === 32 || text.charCodeAt(k) === 9 || text.charCodeAt(k) === 10 || text.charCodeAt(k) === 13)) k += 1;
      r.token(i, stop, k < n && text.charCodeAt(k) === 58 ? "key" : "string");
      i = stop;
      continue;
    }
    if (c === 123 || c === 91) {
      r.token(i, i + 1, "punct");
      stack.push(OPEN[c], { at: i });
      i += 1;
      continue;
    }
    if (c === 125 || c === 93) {
      r.token(i, i + 1, "punct");
      const open = stack.close(c);
      if (open) {
        r.pair(open.at, open.at + 1, i, i + 1);
        r.fold(open.at, i);
      }
      i += 1;
      continue;
    }
    if (c === 44 || c === 58) {
      r.token(i, i + 1, "punct");
      i += 1;
      continue;
    }
    if (c === 45 || (c >= 48 && c <= 57)) {
      NUMBER.lastIndex = i;
      const m = NUMBER.exec(text);
      if (m) {
        r.token(i, i + m[0].length, "number");
        i += m[0].length;
        continue;
      }
    }
    if ((c >= 65 && c <= 90) || (c >= 97 && c <= 122) || c === 95) {
      WORD.lastIndex = i;
      const m = WORD.exec(text);
      if (KEYWORDS.has(m[0])) r.token(i, i + m[0].length, "keyword");
      i += m[0].length;
      continue;
    }
    i += 1;
  }
  return r.done();
}
