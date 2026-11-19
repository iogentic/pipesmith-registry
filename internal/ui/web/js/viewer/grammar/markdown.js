// Markdown line scanner of the viewer, for the source: headings, fenced code blocks, inline code,
// list and quote markers. A fenced block is one code token per line and one fold; a heading folds
// up to the next heading of its level or a higher one. The scanner never throws.

import { Result } from "./result.js";
import { HeadingFolds, inlineCode } from "./lines.js";

export function scan(text) {
  const r = new Result(text);
  const headings = new HeadingFolds(r);
  let fence = null; // { mark, line }
  for (let n = 0; n < r.starts.length; n += 1) {
    const start = r.starts[n];
    const end = r.lineEnd(n);
    const line = text.slice(start, end);
    if (fence) {
      if (line) r.token(start, end, "code");
      headings.content(n);
      if (new RegExp(`^ {0,3}${fence.mark[0] === "`" ? "`" : "~"}{${fence.mark.length},}\\s*$`).test(line)) {
        r.foldLines(fence.line + 1, n + 1);
        fence = null;
      }
      continue;
    }
    if (!line.trim()) continue;
    const open = /^ {0,3}(`{3,}|~{3,})/.exec(line);
    if (open && !(open[1][0] === "`" && line.slice(open[0].length).includes("`"))) {
      r.token(start, end, "code");
      fence = { mark: open[1], line: n };
      headings.content(n);
      continue;
    }
    const heading = /^ {0,3}(#{1,6})(?=\s|$)/.exec(line);
    if (heading) {
      r.token(start + line.indexOf("#"), end, "heading");
      headings.heading(n, heading[1].length);
      continue;
    }
    headings.content(n);
    const marker = /^(\s*)([-*+]|[0-9]+[.)]|>)(?=\s|$)/.exec(line);
    if (marker) r.token(start + marker[1].length, start + marker[1].length + marker[2].length, "punct");
    inlineCode(r, start, line, false);
  }
  if (fence) r.foldLines(fence.line + 1, r.starts.length);
  headings.end();
  return r.done();
}
