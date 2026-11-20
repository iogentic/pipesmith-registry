// The source view of the viewer (docs/plans/browse-tools.md, section Viewer Parts). Rows are drawn
// for the visible window only (call B10): the height of the scroll area, the offset of the drawn
// rows and the column of a guide are custom properties set through the element object, from numbers
// the view computes, never from file content. File content goes into the page as text nodes only.

import { h, clear, copyText, uid } from "../dom.js";
import { splitLines, rowWindow, indentOf, Folds, partners, rowOf, findAll } from "./model.js";

// Rows drawn above and below the visible ones.
export const OVERSCAN = 8;
// A line longer than this is drawn without colours, as one text node, cut only at the edges of
// its find matches (call B10, first value).
export const LONG_LINE = 20000;
// Wrap is offered up to this many lines, where every row is in the document (call B10, first value).
export const WRAP_LINES = 5000;
// Guides drawn on one row at most, the innermost ones; deeper nesting leaves the outer ones out, so a
// crafted file cannot make a row hold one element per level.
export const GUIDE_LEVELS = 64;
// Bidirectional control characters drawn as a mark of their own on one row at most. Each mark holds
// its character in an isolate and shows its name; above the bound a control is drawn as BIDI_REST.
export const BIDI_MARKS = 1000;
export const BIDI_REST = "\u25af";
// Find marks drawn on one row at most: around the current match when it is on the row, else the first
// ones. The count and the steps of the find still cover every match.
export const FIND_MARKS = 1000;
const BIDI = /[\u202a-\u202e\u2066-\u2069]/g;

const GRAMMARS = {
  xml: () => import("./grammar/xml.js"),
  json: () => import("./grammar/json.js"),
  toml: () => import("./grammar/toml.js"),
  keyvalue: () => import("./grammar/keyvalue.js"),
  yaml: () => import("./grammar/yaml.js"),
  markdown: () => import("./grammar/markdown.js"),
  asciidoc: () => import("./grammar/asciidoc.js")
};

// The scan of a text for its kind, or an empty result for plain text.
export async function scanFor(kind, text) {
  const load = GRAMMARS[kind];
  if (!load) return null;
  const grammar = await load();
  return grammar.scan(text);
}

function setNumber(el, name, value) {
  el.style.setProperty(name, String(value));
}

function key(line, start) {
  return `${line}:${start}`;
}

function isMac() {
  return /Mac|iPhone|iPad/.test(navigator.platform || "");
}

// Mounts the view of a text into host. doc: { text, name, scan }. Returns the controller.
export function mountViewer(host, doc) {
  const text = doc.text || "";
  const lines = splitLines(text);
  const scan = doc.scan || null;
  const folds = new Folds(lines.length, scan ? scan.folds : []);
  const pairOf = partners(scan ? scan.pairs : []);
  let marks = []; // [line, start, end] of the clicked token and its partner
  let matches = []; // [line, start, end] of the find
  let matchesByLine = new Map(); // line -> indexes into matches
  let current = -1;
  let wrap = false;
  let target = 0; // the line go to line went to
  let longest = 0;
  for (const line of lines) if (line.length > longest) longest = line.length;

  const button = (label, extra = {}) => h("button", { type: "button", class: ["btn", "small", "quiet", extra.class], text: label, ...extra.attrs });
  const findId = uid("vfind");
  const gotoId = uid("vgoto");
  const findInput = h("input", { type: "search", id: findId, class: "vfind", placeholder: "Find", "aria-label": "Find in the file", autocomplete: "off", spellcheck: "false" });
  const caseBox = h("input", { type: "checkbox", class: "vcase" });
  const caseLabel = h("label", { class: "vcase-label" }, caseBox, " Match case");
  const count = h("span", { class: "vcount muted small", "aria-live": "polite" });
  const prevButton = button("Previous");
  const nextButton = button("Next");
  const gotoInput = h("input", { type: "number", id: gotoId, class: "vgoto", min: "1", max: String(lines.length), placeholder: "Line", "aria-label": "Go to line" });
  const gotoButton = button("Go");
  const copyButton = button("Copy");
  const wrapButton = button("Wrap", { class: "vwrap", attrs: { "aria-pressed": "false", disabled: lines.length > WRAP_LINES } });
  const reasonId = uid("vreason");
  const reason = lines.length > WRAP_LINES ? h("span", { class: "vreason muted small", id: reasonId, text: `Wrap is off above ${WRAP_LINES.toLocaleString("en")} lines.` }) : null;
  if (reason) wrapButton.setAttribute("aria-describedby", reasonId);
  const collapseAll = button("Collapse all", { attrs: { disabled: !folds.list.length } });
  const expandAll = button("Expand all", { attrs: { disabled: !folds.list.length } });
  // Two parts: the find field with its steps and count, and the other tools. On a narrow screen each
  // part is one row and the second one scrolls sideways.
  const more = h(
    "div",
    { class: "vmore" },
    h("div", { class: "vgroup" }, caseLabel),
    h("div", { class: "vgroup" }, gotoInput, gotoButton),
    h("div", { class: "vgroup" }, collapseAll, expandAll, wrapButton, reason, copyButton)
  );
  // A control of the row that scrolls sideways comes fully into it when it gets the focus.
  more.addEventListener("focusin", (event) => event.target.scrollIntoView({ block: "nearest", inline: "nearest" }));
  const tools = h(
    "div",
    { class: "vtools", role: "toolbar", "aria-label": "Viewer tools" },
    h("div", { class: "vgroup vfindbar" }, findInput, prevButton, nextButton, count),
    more
  );
  const info = h("p", { class: "vinfo", role: "status" });
  const list = h("div", { class: "viewer-rows" });
  const space = h("div", { class: "viewer-space" }, list);
  const scroller = h("div", { class: "viewer-scroll", tabindex: "0", role: "region", "aria-label": `Source of ${doc.name || "the file"}` }, space);
  const root = h("div", { class: "viewer" }, tools, scroller, info);
  setNumber(root, "--digits", String(lines.length).length);
  setNumber(space, "--cols", longest);
  host.append(root);

  let rowHeight = 0;
  let drawnFirst = 0;
  let drawnLast = -1;

  function measure() {
    const probe = h("div", { class: "vrow" }, h("span", { class: "vnum", text: "0" }), h("span", { class: "vcode", text: "0" }));
    list.append(probe);
    rowHeight = probe.getBoundingClientRect().height || 20;
    probe.remove();
  }

  function isMarked(line, start) {
    return marks.some((m) => m[0] === line && m[1] === start);
  }

  // The indexes of the matches of a line that its row draws, at most FIND_MARKS of them. The indexes
  // of one line follow each other, so the current match sits at current minus the first index.
  function drawnMatches(number) {
    const all = matchesByLine.get(number) || [];
    if (all.length <= FIND_MARKS) return all;
    const at = current >= all[0] && current <= all[all.length - 1] ? current - all[0] : 0;
    const from = Math.max(0, Math.min(at - (FIND_MARKS >> 1), all.length - FIND_MARKS));
    return all.slice(from, from + FIND_MARKS);
  }

  // The text of a line in segments: cut at every token edge and every match edge, so that each
  // segment carries the class of its token and of a match. A line longer than LONG_LINE has no token
  // segments; it is one text node, or cut at the edges of its matches only.
  function code(number) {
    const textOf = lines[number - 1];
    const el = h("span", { class: "vcode" });
    const long = textOf.length > LONG_LINE;
    const found = drawnMatches(number).map((i) => [matches[i][1], matches[i][2], i === current]);
    const controls = [];
    for (const m of textOf.matchAll(BIDI)) controls.push(m.index);
    const marked = new Set(controls.slice(0, BIDI_MARKS));
    const shown = (a, b) => {
      const part = textOf.slice(a, b);
      return controls.length > BIDI_MARKS && !(b - a === 1 && marked.has(a)) ? part.replace(BIDI, BIDI_REST) : part;
    };
    const piece = (a, b, attrs) => {
      if (b - a === 1 && marked.has(a)) {
        const cls = attrs ? attrs.class : [];
        return h("span", { ...(attrs || {}), class: [...cls, "vbidi"], dataset: { ...(attrs ? attrs.dataset : {}), bidi: textOf.charCodeAt(a).toString(16) }, text: textOf[a] });
      }
      return attrs ? h("span", { ...attrs, text: shown(a, b) }) : shown(a, b);
    };
    if (long && !found.length && !marked.size) {
      el.append(shown(0, textOf.length));
      return el;
    }
    const tokens = !long && scan ? scan.tokens[number - 1] || [] : [];
    const edges = new Set([0, textOf.length]);
    for (const [a, b] of tokens) edges.add(a).add(b);
    for (const [a, b] of found) edges.add(a).add(b);
    for (const a of marked) edges.add(a).add(a + 1);
    const cuts = [...edges].sort((a, b) => a - b);
    let t = 0;
    let f = 0;
    for (let i = 0; i + 1 < cuts.length; i += 1) {
      const a = cuts[i];
      const b = cuts[i + 1];
      while (t < tokens.length && tokens[t][1] <= a) t += 1;
      while (f < found.length && found[f][1] <= a) f += 1;
      const token = t < tokens.length && tokens[t][0] <= a ? tokens[t] : null;
      const match = f < found.length && found[f][0] <= a ? found[f] : null;
      if (!token && !match) {
        el.append(piece(a, b, null));
        continue;
      }
      el.append(
        piece(a, b, {
          class: [token && `t-${token[2]}`, token && isMarked(number, token[0]) && "vmark", match && "vfound", match && match[2] && "vcurrent"],
          dataset: { c: a }
        })
      );
    }
    if (long) return el;
    for (const fi of folds.around(number, GUIDE_LEVELS)) {
      const [first, last] = folds.list[fi];
      const guide = h("span", { class: "vguide", "aria-hidden": "true", dataset: { from: first, to: last } });
      setNumber(guide, "--col", indentOf(lines[first - 1]));
      el.append(guide);
    }
    const fold = folds.at(number);
    if (fold && folds.isCollapsed(number)) {
      el.append(h("button", { type: "button", class: "vellipsis", dataset: { line: number }, "aria-label": `Expand lines ${fold[0]} to ${fold[1]}`, text: `${fold[1] - fold[0]} lines` }));
    }
    return el;
  }

  function row(number) {
    const fold = folds.at(number);
    const arrow = fold
      ? h("button", {
          type: "button",
          class: "vfold",
          dataset: { line: number },
          "aria-expanded": String(!folds.isCollapsed(number)),
          "aria-label": `${folds.isCollapsed(number) ? "Expand" : "Collapse"} lines ${fold[0]} to ${fold[1]}`
        })
      : h("span", { class: "vfold-space", "aria-hidden": "true" });
    return h("div", { class: ["vrow", number === target && "vtarget"], dataset: { line: number } }, h("span", { class: "vnum", "aria-hidden": "true", text: number }), arrow, code(number));
  }

  // Puts the focus back on the control of a row that a draw replaced: the same control of the same
  // line, else its fold arrow, else the source region when the row left the window.
  function refocus(line, ellipsis) {
    const at = `.vrow[data-line="${Number(line)}"]`;
    const el = (ellipsis && list.querySelector(`${at} .vellipsis`)) || list.querySelector(`${at} .vfold`);
    (el || scroller).focus({ preventScroll: true });
  }

  // Draws the rows of the window. A scroll keeps the row elements that stay in the window, so that a
  // selection or the focus inside them survives; force rebuilds every row.
  function draw(force = false) {
    if (!rowHeight) measure();
    const active = document.activeElement;
    const held = active && active !== list && list.contains(active) && active.closest(".vrow") ? active : null;
    const rows = folds.lines();
    const { first, last } = wrap ? { first: 0, last: rows.length - 1 } : rowWindow(scroller.scrollTop, scroller.clientHeight, rowHeight, rows.length, OVERSCAN);
    if (!force && first === drawnFirst && last === drawnLast) return;
    const keep = !force && drawnLast >= drawnFirst && first <= drawnLast && last >= drawnFirst;
    if (keep) {
      for (let i = drawnFirst; i < first; i += 1) list.firstElementChild.remove();
      for (let i = drawnLast; i > last; i -= 1) list.lastElementChild.remove();
      const above = [];
      for (let i = first; i < drawnFirst; i += 1) above.push(row(rows[i]));
      list.prepend(...above);
      for (let i = Math.max(drawnLast + 1, first); i <= last; i += 1) list.append(row(rows[i]));
    } else {
      clear(list);
      for (let i = first; i <= last; i += 1) list.append(row(rows[i]));
    }
    drawnFirst = first;
    drawnLast = last;
    setNumber(list, "--offset", first);
    if (held && !held.isConnected) refocus(held.closest(".vrow").dataset.line, held.classList.contains("vellipsis"));
  }

  // Sets the mark class on the drawn tokens of marks in place, so that a click keeps a selection. A
  // find match can cut a token into several segments; each of them carries the mark.
  function paintMarks() {
    for (const el of list.querySelectorAll(".vmark")) el.classList.remove("vmark");
    for (const [line, start, end] of marks) {
      for (const el of list.querySelectorAll(`.vrow[data-line="${Number(line)}"] .vcode > span[data-c]`)) {
        const c = Number(el.dataset.c);
        if (c >= start && c < end) el.classList.add("vmark");
      }
    }
  }

  function layout() {
    setNumber(space, "--rows", folds.lines().length);
    draw(true);
  }

  function status(textValue) {
    info.textContent = textValue;
  }

  // Puts a line at the top of the window, or as near as the scroll goes; opens folds that hide it.
  function bringToTop(number) {
    if (folds.reveal(number)) layout();
    const rows = folds.lines();
    const index = rowOf(rows, number);
    if (wrap) {
      draw(true);
      const el = list.querySelector(`.vrow[data-line="${rows[index]}"]`);
      if (el) scroller.scrollTop = el.offsetTop;
    } else {
      scroller.scrollTop = index * rowHeight;
    }
    draw(true);
  }

  // Shows a line in the window; scrolls only when it is outside.
  function bringIntoView(number) {
    if (folds.reveal(number)) layout();
    const rows = folds.lines();
    const index = rowOf(rows, number);
    if (wrap) {
      draw(true);
      const el = list.querySelector(`.vrow[data-line="${rows[index]}"]`);
      if (el && (el.offsetTop < scroller.scrollTop || el.offsetTop + el.offsetHeight > scroller.scrollTop + scroller.clientHeight)) scroller.scrollTop = el.offsetTop;
      return;
    }
    const top = index * rowHeight;
    if (top < scroller.scrollTop || top + rowHeight > scroller.scrollTop + scroller.clientHeight) {
      scroller.scrollTop = Math.max(0, top - Math.floor(scroller.clientHeight / 3));
    }
    draw(true);
  }

  function toggle(line) {
    if (folds.toggle(line)) layout();
  }

  // Marks the token at a position and its partner. The status line names the line of a partner
  // that is outside the drawn rows or hidden in a fold.
  function partnerAt(line, col) {
    const tokens = (scan && scan.tokens[line - 1]) || [];
    const token = tokens.find(([start, end]) => start <= col && col < end);
    const partner = token ? pairOf.get(key(line, token[0])) : null;
    marks = partner ? [[line, token[0], token[1]], partner] : [];
    if (partner) {
      const rows = folds.lines();
      const index = rowOf(rows, partner[0]);
      const shown = rows[index] === partner[0] && index >= drawnFirst && index <= drawnLast;
      status(shown ? `Line ${line}, its partner is on line ${partner[0]}.` : `Line ${line}, its partner is on line ${partner[0]}, outside the view.`);
    } else {
      status("");
    }
    paintMarks();
  }

  // Scrolls the source sideways when the current match lies left or right of the window. The line
  // numbers and arrows stay at the left edge, so the window of the code starts after them.
  function revealCurrent() {
    if (wrap) return;
    const el = list.querySelector(".vcurrent");
    if (!el) return;
    const rowBox = el.closest(".vrow").getBoundingClientRect();
    const gutter = el.closest(".vcode").getBoundingClientRect().left - rowBox.left;
    const box = el.getBoundingClientRect();
    const left = box.left - rowBox.left;
    const right = box.right - rowBox.left;
    if (left < scroller.scrollLeft + gutter || right > scroller.scrollLeft + scroller.clientWidth) {
      scroller.scrollLeft = Math.max(0, left - gutter - Math.floor((scroller.clientWidth - gutter) / 3));
    }
  }

  function showCount() {
    count.textContent = !findInput.value ? "" : matches.length ? `${(current + 1).toLocaleString("en")} of ${matches.length.toLocaleString("en")}` : "No match";
  }

  function goToMatch(index) {
    if (!matches.length) return;
    current = (index + matches.length) % matches.length;
    showCount();
    bringIntoView(matches[current][0]);
    draw(true);
    revealCurrent();
  }

  // Runs a find over the whole text, not over the drawn rows (call B12).
  function find(query, matchCase) {
    if (query !== undefined) findInput.value = query;
    if (matchCase !== undefined) caseBox.checked = Boolean(matchCase);
    matches = findAll(lines, findInput.value, caseBox.checked);
    matchesByLine = new Map();
    matches.forEach((m, i) => {
      const at = matchesByLine.get(m[0]);
      if (at) at.push(i);
      else matchesByLine.set(m[0], [i]);
    });
    current = matches.length ? 0 : -1;
    showCount();
    if (current >= 0) bringIntoView(matches[0][0]);
    draw(true);
    revealCurrent();
    return { count: matches.length };
  }

  function goTo(line) {
    const number = Math.min(Math.max(1, Math.floor(Number(line)) || 1), lines.length);
    target = number;
    bringToTop(number);
    status(`Line ${number} of ${lines.length}.`);
  }

  function setWrap(on) {
    wrap = on && lines.length <= WRAP_LINES;
    root.classList.toggle("wrap", wrap);
    wrapButton.setAttribute("aria-pressed", String(wrap));
    layout();
  }

  root.addEventListener("click", (event) => {
    const arrow = event.target.closest(".vfold, .vellipsis");
    if (arrow) {
      toggle(Number(arrow.dataset.line));
      return;
    }
    const span = event.target.closest(".vcode > span[data-c]");
    const rowEl = event.target.closest(".vrow");
    if (rowEl && list.contains(rowEl)) partnerAt(Number(rowEl.dataset.line), span ? Number(span.dataset.c) : -1);
  });
  // Ctrl+F, or Cmd+F on a Mac, inside the viewer opens its own find (call B12); Escape gives the
  // focus back to where it was.
  let returnFocus = null;
  root.addEventListener("keydown", (event) => {
    if (event.key.toLowerCase() === "f" && (isMac() ? event.metaKey : event.ctrlKey) && !event.altKey) {
      event.preventDefault();
      if (document.activeElement !== findInput) returnFocus = document.activeElement;
      findInput.focus();
      findInput.select();
    }
  });
  findInput.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      goToMatch(current + (event.shiftKey ? -1 : 1));
    } else if (event.key === "Escape") {
      event.preventDefault();
      (returnFocus && returnFocus.isConnected ? returnFocus : scroller).focus();
      returnFocus = null;
    }
  });
  findInput.addEventListener("input", () => find());
  caseBox.addEventListener("change", () => find());
  nextButton.addEventListener("click", () => goToMatch(current + 1));
  prevButton.addEventListener("click", () => goToMatch(current - 1));
  gotoInput.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      if (gotoInput.value) goTo(gotoInput.value);
    }
  });
  gotoButton.addEventListener("click", () => {
    if (gotoInput.value) goTo(gotoInput.value);
  });
  copyButton.addEventListener("click", async () => {
    const ok = await copyText(text);
    status(ok ? `Copied ${lines.length.toLocaleString("en")} lines.` : "The browser did not allow the copy.");
  });
  wrapButton.addEventListener("click", () => setWrap(!wrap));
  collapseAll.addEventListener("click", () => {
    folds.collapseAll();
    layout();
  });
  expandAll.addEventListener("click", () => {
    folds.expandAll();
    layout();
  });

  const onScroll = () => {
    if (!wrap) draw();
  };
  scroller.addEventListener("scroll", onScroll, { passive: true });
  const observer = typeof ResizeObserver === "function" ? new ResizeObserver(() => draw(true)) : null;
  if (observer) observer.observe(scroller);
  layout();

  return {
    root,
    lines,
    folds,
    toggle,
    partnerAt,
    find,
    next: () => goToMatch(current + 1),
    prev: () => goToMatch(current - 1),
    findState: () => ({ count: matches.length, current: current >= 0 ? matches[current] : null }),
    goTo,
    setWrap,
    collapseAll() {
      folds.collapseAll();
      layout();
    },
    expandAll() {
      folds.expandAll();
      layout();
    },
    scrollToLine: goTo,
    drawn: () => [folds.lines()[drawnFirst], folds.lines()[drawnLast]],
    dispose() {
      scroller.removeEventListener("scroll", onScroll);
      if (observer) observer.disconnect();
      root.remove();
    }
  };
}
