// YAML line scanner of the viewer: comments, keys, list dashes, quoted strings, numbers, keywords,
// anchors, aliases and tags, flow punctuation and block scalars. Folds by indentation: a line folds
// over the lines below it that are more indented; a comment line counts as blank there. The scanner
// never throws.

import { Result } from "./result.js";
import { indentFolds } from "./lines.js";

const KEYWORDS = new Set(["true", "false", "True", "False", "TRUE", "FALSE", "yes", "no", "Yes", "No", "on", "off", "null", "Null", "NULL", "~"]);
const NUMBER = /^[-+]?(?:[0-9][0-9_]*(?:\.[0-9_]*)?|\.[0-9]+)(?:[eE][-+]?[0-9]+)?$|^0x[0-9A-Fa-f]+$|^0o[0-7]+$|^[-+]?\.(?:inf|Inf|INF)$|^\.(?:nan|NaN|NAN)$/;
const BLOCK = /^[|>](?:[-+]?[0-9]?|[0-9][-+]?)(?=\s|$)/;

export function scan(text) {
  const r = new Result(text);
  const count = r.starts.length;
  const indents = new Array(count).fill(-1); // -1 for a blank line
  let block = -1; // the indentation a block scalar must exceed, or -1

  for (let n = 0; n < count; n += 1) {
    const start = r.starts[n];
    const line = text.slice(start, r.lineEnd(n));
    let col = 0;
    while (col < line.length && line[col] === " ") col += 1;
    const empty = !line.trim();
    if (!empty) indents[n] = col;
    if (block >= 0) {
      if (empty || col > block) {
        if (!empty) r.token(start + col, start + line.length, "string");
        continue;
      }
      block = -1;
    }
    if (empty) continue;
    if (/^[ \t]*#/.test(line)) indents[n] = -1;
    if (col === 0 && /^(?:---|\.\.\.)(?=\s|$)/.test(line)) {
      r.token(start, start + 3, "meta");
      col = 3;
    }
    const at = (c) => start + c;
    const skip = () => {
      while (col < line.length && (line[col] === " " || line[col] === "\t")) col += 1;
    };
    skip();
    while (line[col] === "-" && (col + 1 === line.length || line[col + 1] === " ")) {
      r.token(at(col), at(col + 1), "punct");
      col += 1;
      skip();
    }
    // A key: quoted, or plain up to the first ": " or a colon at the line end.
    if (col < line.length && line[col] !== "#") {
      let end = -1;
      if (line[col] === '"' || line[col] === "'") {
        const close = line.indexOf(line[col], col + 1);
        if (close > 0) end = close + 1;
      } else if (!"[{&*!|>%@`".includes(line[col])) {
        const m = /:(?=\s|$)/.exec(line.slice(col));
        const comment = line.indexOf(" #", col);
        if (m && (comment < 0 || col + m.index < comment)) end = col + m.index;
      }
      if (end > col) {
        let colon = end;
        while (colon < line.length && line[colon] === " ") colon += 1;
        if (line[colon] === ":" && (colon + 1 === line.length || /\s/.test(line[colon + 1]))) {
          let keyEnd = end;
          while (keyEnd > col && line[keyEnd - 1] === " ") keyEnd -= 1;
          r.token(at(col), at(keyEnd), "key");
          r.token(at(colon), at(colon + 1), "punct");
          col = colon + 1;
          skip();
        }
      }
    }
    // The value.
    const m = BLOCK.exec(line.slice(col));
    if (m) {
      r.token(at(col), at(col + m[0].length), "punct");
      col += m[0].length;
      block = indents[n];
    }
    let flow = 0;
    while (col < line.length) {
      const c = line[col];
      if (c === " " || c === "\t") {
        col += 1;
        continue;
      }
      if (c === "#" && (col === 0 || line[col - 1] === " " || line[col - 1] === "\t")) {
        r.token(at(col), at(line.length), "comment");
        break;
      }
      if (c === '"' || c === "'") {
        let j = col + 1;
        while (j < line.length && line[j] !== c) j += c === '"' && line[j] === "\\" ? 2 : 1;
        const stop = Math.min(j + 1, line.length);
        r.token(at(col), at(stop), "string");
        col = stop;
        continue;
      }
      if ("[]{},".includes(c)) {
        r.token(at(col), at(col + 1), "punct");
        if (c === "[" || c === "{") flow += 1;
        if (c === "]" || c === "}") flow = Math.max(0, flow - 1);
        col += 1;
        continue;
      }
      if (c === "&" || c === "*" || c === "!") {
        let j = col + 1;
        while (j < line.length && !" \t,[]{}".includes(line[j])) j += 1;
        r.token(at(col), at(j), "meta");
        col = j;
        continue;
      }
      // A plain scalar: up to " #", or in a flow up to a flow character.
      let j = col;
      while (j < line.length && !(line[j] === " " && line[j + 1] === "#") && !(flow && ",[]{}".includes(line[j]))) j += 1;
      let end = j;
      while (end > col && (line[end - 1] === " " || line[end - 1] === "\t")) end -= 1;
      const word = line.slice(col, end);
      if (KEYWORDS.has(word)) r.token(at(col), at(end), "keyword");
      else if (NUMBER.test(word)) r.token(at(col), at(end), "number");
      col = j;
    }
  }
  indentFolds(r, indents);
  return r.done();
}
