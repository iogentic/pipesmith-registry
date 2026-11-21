// A small Markdown renderer for release notes. It builds DOM nodes through h(), so every piece of
// the text goes in as a text node: raw HTML in the notes stays visible text and runs nothing. It
// knows headings, paragraphs, lists, block quotes, fenced and indented code, rules, inline code,
// strong, emphasis and links; a link is kept only with an http or https address and opens in a new
// tab without the opener.

import { h } from "./dom.js";

const SAFE_URL = /^https?:\/\/[^\s]+$/i;
// Bounds against notes built to stall the page: quotes nest at most this deep, and a code span has
// at most three backticks and 2000 characters.
const MAX_DEPTH = 8;

export function safeUrl(href) {
  return typeof href === "string" && SAFE_URL.test(href);
}

// Inline Markdown of one line into nodes: code spans first, then links, then strong and emphasis.
export function inline(text) {
  const out = [];
  let rest = String(text);
  const pattern = /(`{1,3})([^`]|[^`][\s\S]{0,2000}?[^`])\1(?!`)|\[([^\]\n]+)\]\(([^)\s]+)\)|\*\*([^*\n]+)\*\*|__([^_\n]+)__|\*([^*\n]+)\*|(?<![A-Za-z0-9])_([^_\n]+)_(?![A-Za-z0-9])|<(https?:\/\/[^>\s]+)>/;
  while (rest.length) {
    const m = pattern.exec(rest);
    if (!m) {
      out.push(rest);
      break;
    }
    if (m.index > 0) out.push(rest.slice(0, m.index));
    if (m[1]) out.push(h("code", { text: m[2].trim() }));
    else if (m[3] !== undefined) out.push(link(m[4], inline(m[3])));
    else if (m[5] !== undefined || m[6] !== undefined) out.push(h("strong", {}, inline(m[5] ?? m[6])));
    else if (m[7] !== undefined || m[8] !== undefined) out.push(h("em", {}, inline(m[7] ?? m[8])));
    else if (m[9] !== undefined) out.push(link(m[9], [m[9]]));
    rest = rest.slice(m.index + m[0].length);
  }
  return out;
}

function link(href, children) {
  if (!SAFE_URL.test(href)) return h("span", {}, children);
  return h("a", { href, target: "_blank", rel: "noopener noreferrer" }, children);
}

const HEADING = /^(#{1,6})\s+(.*?)\s*#*\s*$/;
const BULLET = /^(\s*)[-*+]\s+(.*)$/;
const NUMBERED = /^(\s*)\d{1,9}[.)]\s+(.*)$/;
const FENCE = /^\s*(```|~~~)/;
const RULE = /^\s*([-*_])(\s*\1){2,}\s*$/;

// render turns Markdown text into a list of block nodes. Headings start at h3 so they sit below the
// heading of the panel; level 1 and 2 of the notes become h3, the rest h4.
export function render(text, depth = 0) {
  const lines = String(text ?? "").replace(/\r\n?/g, "\n").split("\n");
  const blocks = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) {
      i++;
      continue;
    }
    if (FENCE.test(line)) {
      const fence = line.trim().slice(0, 3);
      const body = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith(fence)) body.push(lines[i++]);
      i++;
      blocks.push(h("pre", { class: "log" }, h("code", { text: body.join("\n") })));
      continue;
    }
    const heading = HEADING.exec(line);
    if (heading) {
      blocks.push(h(heading[1].length <= 2 ? "h3" : "h4", {}, inline(heading[2])));
      i++;
      continue;
    }
    if (RULE.test(line)) {
      blocks.push(h("hr"));
      i++;
      continue;
    }
    if (/^ {4}|\t/.test(line)) {
      const body = [];
      while (i < lines.length && (/^ {4}|\t/.test(lines[i]) || !lines[i].trim())) body.push(lines[i++].replace(/^( {4}|\t)/, ""));
      blocks.push(h("pre", { class: "log" }, h("code", { text: body.join("\n").replace(/\n+$/, "") })));
      continue;
    }
    if (/^\s*>/.test(line)) {
      const body = [];
      while (i < lines.length && /^\s*>/.test(lines[i])) body.push(lines[i++].replace(/^\s*>\s?/, ""));
      blocks.push(depth < MAX_DEPTH ? h("blockquote", {}, render(body.join("\n"), depth + 1)) : h("p", { text: body.join(" ") }));
      continue;
    }
    if (BULLET.test(line) || NUMBERED.test(line)) {
      const ordered = !BULLET.test(line);
      const pattern = ordered ? NUMBERED : BULLET;
      const items = [];
      while (i < lines.length) {
        const m = pattern.exec(lines[i]);
        if (m) {
          items.push([m[2]]);
          i++;
        } else if (items.length && lines[i].trim() && /^\s{2,}/.test(lines[i]) && !BULLET.test(lines[i]) && !NUMBERED.test(lines[i])) {
          items[items.length - 1].push(lines[i++].trim());
        } else if (items.length && lines[i].trim() && (BULLET.test(lines[i]) || NUMBERED.test(lines[i]))) {
          // a nested list item stays in the list as an item of its own
          const m2 = BULLET.exec(lines[i]) || NUMBERED.exec(lines[i]);
          items.push([m2[2]]);
          i++;
        } else break;
      }
      blocks.push(h(ordered ? "ol" : "ul", {}, items.map((parts) => h("li", {}, inline(parts.join(" "))))));
      continue;
    }
    const para = [];
    while (
      i < lines.length &&
      lines[i].trim() &&
      !HEADING.test(lines[i]) &&
      !FENCE.test(lines[i]) &&
      !BULLET.test(lines[i]) &&
      !NUMBERED.test(lines[i]) &&
      !/^\s*>/.test(lines[i]) &&
      !RULE.test(lines[i])
    ) {
      para.push(lines[i++].trim());
    }
    blocks.push(h("p", {}, inline(para.join(" "))));
  }
  return blocks;
}
