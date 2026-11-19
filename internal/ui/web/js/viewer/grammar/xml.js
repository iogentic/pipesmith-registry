// XML scanner of the viewer: tags, attributes, values, comments, entities, declarations, processing
// instructions, doctype and CDATA marks. Folds for elements and for comments, declarations and CDATA
// sections over several lines; pairs from the name of a start tag to the name of its end tag. An end
// tag without a start tag of its name stays unpaired, and the open elements above a matching one are
// left without fold and pair. The scanner never throws: what it cannot read stays plain text.

import { Result, Opens } from "./result.js";

const ENTITY = /&(#[0-9]+|#x[0-9a-fA-F]+|[A-Za-z_:][A-Za-z0-9_.:-]*);/y;

function nameStart(code) {
  return (code >= 65 && code <= 90) || (code >= 97 && code <= 122) || code === 95 || code === 58 || code >= 0xc0;
}

function nameChar(code) {
  return nameStart(code) || (code >= 48 && code <= 57) || code === 45 || code === 46 || code === 0xb7;
}

function space(code) {
  return code === 32 || code === 9 || code === 10 || code === 13;
}

export function scan(text) {
  const r = new Result(text);
  const n = text.length;
  const stack = new Opens(); // open elements by name: { start, nameStart, nameEnd }
  let i = 0;

  const nameEnd = (from) => {
    let j = from;
    while (j < n && nameChar(text.charCodeAt(j))) j += 1;
    return j;
  };
  const skipSpace = (from) => {
    let j = from;
    while (j < n && space(text.charCodeAt(j))) j += 1;
    return j;
  };

  while (i < n) {
    const code = text.charCodeAt(i);
    if (code === 38) {
      ENTITY.lastIndex = i;
      const m = ENTITY.exec(text);
      if (m) {
        r.token(i, i + m[0].length, "entity");
        i += m[0].length;
      } else {
        i += 1;
      }
      continue;
    }
    if (code !== 60) {
      i += 1;
      continue;
    }
    const next = text.charCodeAt(i + 1);
    if (text.startsWith("<!--", i)) {
      const end = text.indexOf("-->", i + 4);
      const stop = end < 0 ? n : end + 3;
      r.token(i, stop, "comment");
      r.fold(i, stop - 1);
      i = stop;
      continue;
    }
    if (text.startsWith("<![CDATA[", i)) {
      r.token(i, i + 9, "meta");
      const end = text.indexOf("]]>", i + 9);
      if (end < 0) {
        i = n;
        continue;
      }
      r.token(end, end + 3, "meta");
      r.fold(i, end);
      i = end + 3;
      continue;
    }
    if (next === 63) {
      const end = text.indexOf("?>", i + 2);
      const stop = end < 0 ? n : end + 2;
      r.token(i, stop, "meta");
      r.fold(i, stop - 1);
      i = stop;
      continue;
    }
    if (next === 33) {
      let j = i + 2;
      let depth = 0;
      while (j < n) {
        const c = text.charCodeAt(j);
        if (c === 34 || c === 39) {
          const close = text.indexOf(text[j], j + 1);
          j = close < 0 ? n : close + 1;
          continue;
        }
        if (c === 91) depth += 1;
        else if (c === 93) depth = Math.max(0, depth - 1);
        else if (c === 62 && depth === 0) break;
        j += 1;
      }
      const stop = j < n ? j + 1 : n;
      r.token(i, stop, "meta");
      r.fold(i, stop - 1);
      i = stop;
      continue;
    }
    if (next === 47 && nameStart(text.charCodeAt(i + 2))) {
      const end = nameEnd(i + 2);
      const name = text.slice(i + 2, end);
      r.token(i, i + 2, "punct");
      r.token(i + 2, end, "tag");
      let j = skipSpace(end);
      if (j < n && text.charCodeAt(j) === 62) {
        r.token(j, j + 1, "punct");
        j += 1;
      }
      const open = stack.close(name);
      if (open) {
        r.pair(open.nameStart, open.nameEnd, i + 2, end);
        r.fold(open.start, i);
      }
      i = j;
      continue;
    }
    if (!nameStart(next)) {
      i += 1;
      continue;
    }
    // A start tag or an empty element tag.
    const start = i;
    const end = nameEnd(i + 1);
    r.token(i, i + 1, "punct");
    r.token(i + 1, end, "tag");
    let j = end;
    let closed = false;
    let empty = false;
    while (j < n) {
      j = skipSpace(j);
      if (j >= n) break;
      const c = text.charCodeAt(j);
      if (c === 62) {
        r.token(j, j + 1, "punct");
        j += 1;
        closed = true;
        break;
      }
      if (c === 47 && text.charCodeAt(j + 1) === 62) {
        r.token(j, j + 2, "punct");
        j += 2;
        closed = true;
        empty = true;
        break;
      }
      if (c === 60) break; // a broken tag: the next one starts here
      if (nameChar(c)) {
        const attrEnd = nameEnd(j);
        r.token(j, attrEnd, "attr");
        j = skipSpace(attrEnd);
        if (j < n && text.charCodeAt(j) === 61) {
          r.token(j, j + 1, "punct");
          j = skipSpace(j + 1);
          const q = text.charCodeAt(j);
          if (q === 34 || q === 39) {
            const close = text.indexOf(text[j], j + 1);
            const stop = close < 0 ? n : close + 1;
            r.token(j, stop, "string");
            j = stop;
          } else {
            const valueStart = j;
            while (j < n) {
              const v = text.charCodeAt(j);
              if (space(v) || v === 62 || v === 60 || (v === 47 && text.charCodeAt(j + 1) === 62)) break;
              j += 1;
            }
            r.token(valueStart, j, "string");
          }
        }
        continue;
      }
      j += 1;
    }
    if (closed && !empty) stack.push(text.slice(i + 1, end), { start, nameStart: i + 1, nameEnd: end });
    i = j;
  }
  return r.done();
}
