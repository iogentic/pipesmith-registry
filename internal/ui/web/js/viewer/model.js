// The model of the viewer: the lines of a text and the window of rows the view draws. It holds
// numbers only and touches no element.

// The lines of a text without their line ends. A text that ends with a line end has no empty last
// line. "\r\n" counts as one line end.
export function splitLines(text) {
  const lines = String(text).split("\n");
  if (lines.length > 1 && lines[lines.length - 1] === "") lines.pop();
  for (let i = 0; i < lines.length; i += 1) {
    if (lines[i].endsWith("\r")) lines[i] = lines[i].slice(0, -1);
  }
  return lines;
}

// The rows to draw for a scroll position: the visible rows and overscan rows on each side, as
// indexes into the list of visible rows, from first to last inclusive.
export function rowWindow(scrollTop, viewHeight, rowHeight, rowCount, overscan) {
  if (!rowCount || rowHeight <= 0) return { first: 0, last: -1 };
  const top = Math.max(0, Math.floor(scrollTop / rowHeight));
  const fit = Math.max(1, Math.ceil(viewHeight / rowHeight));
  const first = Math.max(0, Math.min(top, rowCount - 1) - overscan);
  const last = Math.min(rowCount - 1, top + fit - 1 + overscan);
  return { first, last };
}

// The columns a line is indented by: a tab goes to the next multiple of TAB.
export const TAB = 4;
export function indentOf(line) {
  let col = 0;
  for (const ch of line) {
    if (ch === " ") col += 1;
    else if (ch === "\t") col += TAB - (col % TAB);
    else break;
  }
  return col;
}

// The folds of a text and which of them are collapsed. A fold [first, last] hides the lines after
// its first line up to its last line while it is collapsed. Folds that start on a hidden line are
// left alone. Several folds may start on one line; the arrow of a line acts on the largest of them.
export class Folds {
  constructor(lineCount, folds) {
    this.count = lineCount;
    this.list = (folds || []).filter(([a, b]) => a >= 1 && b > a && b <= lineCount).sort((x, y) => x[0] - y[0] || y[1] - x[1]);
    this.byLine = new Map(); // first line -> index of the largest fold that starts there
    this.list.forEach((f, i) => {
      if (!this.byLine.has(f[0])) this.byLine.set(f[0], i);
    });
    this.collapsed = new Set();
    this.inner = this.innermost();
    this.visible = null;
  }

  // For each line the index of the innermost fold with first < line < last, or -1; for each fold
  // the index of the fold around it, and of the nearest fold around it that starts on another line.
  innermost() {
    const inner = new Int32Array(this.count + 2).fill(-1);
    this.parent = new Int32Array(this.list.length).fill(-1);
    this.outer = new Int32Array(this.list.length).fill(-1);
    const stack = [];
    let next = 0;
    for (let line = 1; line <= this.count; line += 1) {
      while (stack.length && this.list[stack[stack.length - 1]][1] <= line) stack.pop();
      inner[line] = stack.length ? stack[stack.length - 1] : -1;
      while (next < this.list.length && this.list[next][0] === line) {
        const up = stack.length ? stack[stack.length - 1] : -1;
        this.parent[next] = up;
        this.outer[next] = up < 0 || this.list[up][0] !== this.list[next][0] ? up : this.outer[up];
        stack.push(next);
        next += 1;
      }
    }
    return inner;
  }

  // The folds whose guide stands on a line: of the folds with first < line < last, the innermost of
  // each opening line, innermost first, at most max of them. Folds of one opening line share the
  // column of their guide, so the others are left out.
  around(line, max = Infinity) {
    const out = [];
    for (let f = this.inner[line] ?? -1; f >= 0 && out.length < max; f = this.outer[f]) {
      if (this.list[f][0] < line && line < this.list[f][1]) out.push(f);
    }
    return out;
  }

  at(line) {
    const i = this.byLine.get(line);
    return i === undefined ? null : this.list[i];
  }

  isCollapsed(line) {
    return this.collapsed.has(line);
  }

  toggle(line) {
    if (!this.byLine.has(line)) return false;
    if (this.collapsed.has(line)) this.collapsed.delete(line);
    else this.collapsed.add(line);
    this.visible = null;
    return true;
  }

  collapseAll() {
    for (const line of this.byLine.keys()) this.collapsed.add(line);
    this.visible = null;
  }

  expandAll() {
    this.collapsed.clear();
    this.visible = null;
  }

  // Opens every collapsed fold that hides the line.
  reveal(line) {
    let changed = false;
    for (const first of [...this.collapsed]) {
      const f = this.at(first);
      if (f && f[0] < line && line <= f[1]) {
        this.collapsed.delete(first);
        changed = true;
      }
    }
    if (changed) this.visible = null;
    return changed;
  }

  // The visible lines, by number.
  lines() {
    if (this.visible) return this.visible;
    const out = new Int32Array(this.count);
    let size = 0;
    let hiddenUntil = 0;
    for (let line = 1; line <= this.count; line += 1) {
      if (line <= hiddenUntil) continue;
      out[size] = line;
      size += 1;
      if (this.collapsed.has(line)) hiddenUntil = Math.max(hiddenUntil, this.at(line)[1]);
    }
    this.visible = out.subarray(0, size);
    return this.visible;
  }
}

// The partner of each end of a pair, by "line:start".
export function partners(pairs) {
  const map = new Map();
  for (const [l1, s1, e1, l2, s2, e2] of pairs || []) {
    map.set(`${l1}:${s1}`, [l2, s2, e2]);
    map.set(`${l2}:${s2}`, [l1, s1, e1]);
  }
  return map;
}

// The index of the first visible line at or after a line number.
export function rowOf(visible, line) {
  let low = 0;
  let high = visible.length;
  while (low < high) {
    const mid = (low + high) >> 1;
    if (visible[mid] < line) low = mid + 1;
    else high = mid;
  }
  return low;
}

const NOT_ASCII = /[^\x00-\x7f]/;

// The lower case of a text, one code point at a time, and for each unit of it the start and end of
// the code point of the text it comes from. Lower case can change the length of a letter ("İ"
// gives two units), so offsets into the lower case copy are mapped back through from and to. An
// ASCII text keeps its length and gets no map.
export function lowerMap(text) {
  if (!NOT_ASCII.test(text)) return { text: text.toLowerCase(), from: null, to: null };
  const parts = [];
  const from = [];
  const to = [];
  for (let i = 0; i < text.length; ) {
    const n = text.codePointAt(i) > 0xffff ? 2 : 1;
    const low = text.slice(i, i + n).toLowerCase();
    parts.push(low);
    for (let k = 0; k < low.length; k += 1) {
      from.push(i);
      to.push(i + n);
    }
    i += n;
  }
  return { text: parts.join(""), from, to };
}

// Every match of a plain text query in the lines, as [line, start, end] in offsets of the line,
// line by line and left to right, without overlap. Without the case switch both sides are compared
// in lower case, one code point at a time.
export function findAll(lines, query, matchCase) {
  const out = [];
  if (!query) return out;
  const needle = matchCase ? query : lowerMap(query).text;
  for (let i = 0; i < lines.length; i += 1) {
    const hay = matchCase ? { text: lines[i], from: null, to: null } : lowerMap(lines[i]);
    let last = 0;
    for (let at = hay.text.indexOf(needle); at >= 0; at = hay.text.indexOf(needle, at + needle.length)) {
      const start = hay.from ? hay.from[at] : at;
      const end = hay.to ? hay.to[at + needle.length - 1] : at + needle.length;
      if (start < last) continue;
      out.push([i + 1, start, end]);
      last = end;
    }
  }
  return out;
}
