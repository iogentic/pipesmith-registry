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

// Inline code in a line: a run of backticks up to the next run of the same length, and, where
// plus is set, text between two plus signs.
export function inlineCode(r, start, line, plus) {
  const pattern = plus ? /(`+)[^`]*?\1|\+[^+\s][^+]*\+/g : /(`+)[^`]*?\1/g;
  for (let m = pattern.exec(line); m; m = pattern.exec(line)) r.token(start + m.index, start + m.index + m[0].length, "code");
}
