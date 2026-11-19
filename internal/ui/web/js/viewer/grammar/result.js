// The result of a scan, built from offsets into the whole text (docs/plans/browse-tools.md, section
// Viewer Parts): tokens per line as [start, end, kind], folds as [first line, last line] and pairs as
// [line, start, end, line, start, end]. Lines count from 1 and follow splitLines of model.js; columns
// count from 0 in the units of a JavaScript string. A token that runs over several lines is cut at
// each line end, and a carriage return before a line end belongs to no token.

export class Result {
  constructor(text) {
    this.text = text;
    this.starts = [0];
    for (let i = text.indexOf("\n"); i >= 0; i = text.indexOf("\n", i + 1)) this.starts.push(i + 1);
    if (this.starts.length > 1 && this.starts[this.starts.length - 1] === text.length) this.starts.pop();
    this.tokens = this.starts.map(() => []);
    this.folds = [];
    this.pairs = [];
  }

  // The line index (from 0) of an offset.
  lineOf(offset) {
    let low = 0;
    let high = this.starts.length - 1;
    while (low < high) {
      const mid = (low + high + 1) >> 1;
      if (this.starts[mid] <= offset) low = mid;
      else high = mid - 1;
    }
    return low;
  }

  // The offset where the content of line index n ends, before its line end.
  lineEnd(n) {
    let end = n + 1 < this.starts.length ? this.starts[n + 1] - 1 : this.text.length;
    if (n + 1 === this.starts.length && end > this.starts[n] && this.text.charCodeAt(end - 1) === 10) end -= 1;
    if (end > this.starts[n] && this.text.charCodeAt(end - 1) === 13) end -= 1;
    return end;
  }

  token(start, end, kind) {
    if (end <= start) return;
    for (let n = this.lineOf(start); n < this.starts.length && this.starts[n] < end; n += 1) {
      const from = Math.max(start, this.starts[n]);
      const to = Math.min(end, this.lineEnd(n));
      if (to > from) {
        const list = this.tokens[n];
        const col = from - this.starts[n];
        const last = list[list.length - 1];
        if (!last || last[1] <= col) list.push([col, to - this.starts[n], kind]);
      }
    }
  }

  // A fold from the line of one offset to the line of another, when they differ.
  fold(startOffset, endOffset) {
    const first = this.lineOf(startOffset) + 1;
    const last = this.lineOf(Math.max(startOffset, endOffset)) + 1;
    if (last > first) this.folds.push([first, last]);
  }

  // A fold over lines given by number.
  foldLines(first, last) {
    if (last > first) this.folds.push([first, last]);
  }

  pair(aStart, aEnd, bStart, bEnd) {
    const a = this.lineOf(aStart);
    const b = this.lineOf(bStart);
    this.pairs.push([a + 1, aStart - this.starts[a], aEnd - this.starts[a], b + 1, bStart - this.starts[b], bEnd - this.starts[b]]);
  }

  done() {
    const seen = new Set();
    const folds = this.folds
      .sort((x, y) => x[0] - y[0] || y[1] - x[1])
      .filter((f) => {
        const key = `${f[0]}:${f[1]}`;
        if (seen.has(key)) return false;
        seen.add(key);
        return true;
      });
    const pairs = this.pairs.sort((x, y) => x[0] - y[0] || x[1] - y[1]);
    return { tokens: this.tokens, folds, pairs };
  }
}

// The open brackets or elements of a scan, with a count per key, so that a closer whose key has no
// open entry is refused without a walk of the stack. close(key) returns the nearest open entry of
// that key and drops it with every entry above it, or returns null and leaves the stack.
export class Opens {
  constructor() {
    this.stack = [];
    this.counts = new Map();
  }

  get length() {
    return this.stack.length;
  }

  top() {
    return this.stack[this.stack.length - 1];
  }

  push(key, entry) {
    this.stack.push({ key, entry });
    this.counts.set(key, (this.counts.get(key) || 0) + 1);
  }

  close(key) {
    if (!this.counts.get(key)) return null;
    let k = this.stack.length - 1;
    while (this.stack[k].key !== key) k -= 1;
    for (let j = k; j < this.stack.length; j += 1) {
      const dropped = this.stack[j].key;
      this.counts.set(dropped, this.counts.get(dropped) - 1);
    }
    const found = this.stack[k].entry;
    this.stack.length = k;
    return found;
  }
}
