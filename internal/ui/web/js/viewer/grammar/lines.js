// Folds of the line scanners: by indentation and by heading level.

// A line folds over the lines below it that are more indented, up to the last line that is not
// blank. indents holds the indentation of each line, -1 for a blank line.
export function indentFolds(r, indents) {
  const stack = []; // { indent, line }
  let last = -1;
  for (let n = 0; n < indents.length; n += 1) {
    const d = indents[n];
    if (d < 0) continue;
    while (stack.length && stack[stack.length - 1].indent >= d) {
      const open = stack.pop();
      if (last > open.line) r.foldLines(open.line + 1, last + 1);
    }
    stack.push({ indent: d, line: n });
    last = n;
  }
  while (stack.length) {
    const open = stack.pop();
    if (last > open.line) r.foldLines(open.line + 1, last + 1);
  }
}

// Folds of headings: a heading folds up to the last line that is not blank before the next heading
// of its level or a higher one.
export class HeadingFolds {
  constructor(r) {
    this.r = r;
    this.stack = []; // { level, line }
    this.last = -1;
  }

  content(n) {
    this.last = n;
  }

  heading(n, level) {
    while (this.stack.length && this.stack[this.stack.length - 1].level >= level) this.close();
    this.stack.push({ level, line: n });
    this.last = n;
  }

  close() {
    const open = this.stack.pop();
    if (this.last > open.line) this.r.foldLines(open.line + 1, this.last + 1);
  }

  end() {
    while (this.stack.length) this.close();
  }
}

const PLUS = /\+[^+\s][^+]*\+/y;

// Inline code in a line: a run of backticks up to the next run of exactly its length, and, where
// plus is set, text between two plus signs. A run without such a closer stays text. Each length
// keeps a cursor into its runs that only moves forward, so a line scans in linear time.
export function inlineCode(r, start, line, plus) {
  const runs = new Map(); // length to the offsets of its runs, ascending
  for (let i = line.indexOf("`"); i >= 0; ) {
    let j = i;
    while (line[j] === "`") j += 1;
    if (!runs.has(j - i)) runs.set(j - i, { at: [], next: 0 });
    runs.get(j - i).at.push(i);
    i = line.indexOf("`", j);
  }
  let i = 0;
  while (i < line.length) {
    const c = line[i];
    if (c === "`") {
      let j = i;
      while (line[j] === "`") j += 1;
      const same = runs.get(j - i);
      while (same.next < same.at.length && same.at[same.next] <= i) same.next += 1;
      if (same.next < same.at.length) {
        const stop = same.at[same.next] + (j - i);
        r.token(start + i, start + stop, "code");
        i = stop;
      } else {
        i = j;
      }
      continue;
    }
    if (plus && c === "+") {
      PLUS.lastIndex = i;
      const m = PLUS.exec(line);
      if (m) {
        r.token(start + i, start + i + m[0].length, "code");
        i += m[0].length;
        continue;
      }
    }
    i += 1;
  }
}
