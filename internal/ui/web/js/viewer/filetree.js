// The tree of the viewer (call B2): the requests, keys and classes of the tree of S05 in
// js/pages/browse/tree.js, as a module of its own. One folder level per request, 200 entries at a
// time, with a button for the next 200.
//
// Keys: up and down move between entries, right opens a folder or enters it, left closes it or goes
// to the folder above, Home and End go to the first and the last entry.

import { h } from "../dom.js";
import { api } from "../api.js";
import { bytes } from "../format.js";
import { toastProblem } from "../ui/notice.js";

const PAGE = 200;

// repository: the name. onFile(node) and onFolder(node) are called when an entry is activated.
export function createFileTree({ repository, onFile, onFolder }) {
  const list = h("ul", { class: "tree vtree", "aria-label": `Folders and files of ${repository}` });
  const nodes = new Map();
  const root = { path: "", name: repository, isDir: true, parent: null, list, loaded: false, expanded: true, more: false, offset: 0, moreRow: null, button: null };
  let selected = null;

  function makeNode(entry, parent) {
    const node = { path: entry.path, name: entry.name, isDir: Boolean(entry.is_dir), size: entry.size, parent, loaded: false, expanded: false, more: false, offset: 0, moreRow: null };
    const button = h(
      "button",
      { type: "button", class: "node", title: entry.name, dataset: { path: entry.path }, "aria-expanded": node.isDir ? "false" : null },
      h("span", { class: ["kind", node.isDir && "dir"], "aria-hidden": "true" }),
      h("span", { class: "node-name", text: entry.name }),
      !node.isDir && entry.size !== null && entry.size !== undefined ? h("span", { class: "meta", text: bytes(entry.size) }) : null
    );
    button.addEventListener("click", () => activate(node));
    node.button = button;
    node.li = h("li", { dataset: { path: entry.path } }, button);
    if (node.isDir) {
      node.list = h("ul", { class: "tree-level", hidden: true, "aria-label": `Entries of ${entry.name}` });
      node.li.append(node.list);
    }
    nodes.set(node.path, node);
    return node;
  }

  function markEmpty(node) {
    const has = node.list.querySelector(":scope > li > button.node");
    if (!has && !node.more && !node.list.querySelector(":scope > li.empty-row")) node.list.append(h("li", { class: "empty-row", text: "This folder is empty." }));
  }

  async function loadLevel(node) {
    const answer = await api.browse(repository, node.path, PAGE, node.offset);
    const children = (answer && answer.children) || [];
    node.offset += children.length;
    node.more = Boolean(answer && answer.more) && children.length > 0;
    if (node.moreRow) {
      node.moreRow.remove();
      node.moreRow = null;
    }
    for (const child of children) if (!nodes.has(child.path)) node.list.append(makeNode(child, node).li);
    if (node.more) {
      const more = h("button", { type: "button", class: "btn small", text: "Load more", "aria-label": `Load more entries of ${node.path || repository}` });
      more.addEventListener("click", () => loadMore(node, more));
      node.moreRow = h("li", { class: "more-row" }, more);
      node.list.append(node.moreRow);
    }
    node.loaded = true;
    markEmpty(node);
    return children;
  }

  async function loadMore(node, more) {
    more.disabled = true;
    const before = node.offset;
    try {
      await loadLevel(node);
    } catch (error) {
      toastProblem(error);
      more.disabled = false;
      return;
    }
    const first = [...node.list.querySelectorAll(":scope > li > button.node")][before];
    if (first) first.focus();
  }

  async function expand(node) {
    if (!node.isDir || node.expanded) return true;
    if (!node.loaded) {
      node.button.setAttribute("aria-busy", "true");
      try {
        await loadLevel(node);
      } catch (error) {
        toastProblem(error);
        return false;
      } finally {
        node.button.removeAttribute("aria-busy");
      }
    }
    node.list.hidden = false;
    node.expanded = true;
    node.button.setAttribute("aria-expanded", "true");
    return true;
  }

  function collapse(node) {
    if (!node.isDir || !node.expanded || node === root) return;
    node.list.hidden = true;
    node.expanded = false;
    node.button.setAttribute("aria-expanded", "false");
  }

  function select(node) {
    if (selected && selected.button) selected.button.removeAttribute("aria-current");
    selected = node;
    if (node.button) node.button.setAttribute("aria-current", "true");
  }

  async function activate(node) {
    if (node.isDir) {
      if (node.expanded) collapse(node);
      else if (!(await expand(node))) return;
      select(node);
      if (onFolder) onFolder(node);
      return;
    }
    select(node);
    if (onFile) onFile(node);
  }

  // Opens every folder on the way to a path; loads further pages of a folder while the next name is
  // not among the entries loaded so far. Returns the node, or null.
  async function reveal(path) {
    const segments = String(path || "").split("/").filter(Boolean);
    let node = root;
    for (let i = 0; i < segments.length; i += 1) {
      const target = segments.slice(0, i + 1).join("/");
      while (!nodes.has(target) && node.more) await loadLevel(node);
      const child = nodes.get(target);
      if (!child || (i < segments.length - 1 && !child.isDir)) return null;
      if (child.isDir && !(await expand(child))) return null;
      node = child;
    }
    return node;
  }

  function visibleButtons() {
    return [...list.querySelectorAll("button.node")].filter((b) => b.offsetParent !== null);
  }

  list.addEventListener("keydown", async (event) => {
    const current = event.target.closest("button.node");
    if (!current) return;
    const node = nodes.get(current.dataset.path);
    const buttons = visibleButtons();
    const i = buttons.indexOf(current);
    const focus = (b) => b && b.focus();
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        focus(buttons[i + 1]);
        break;
      case "ArrowUp":
        event.preventDefault();
        focus(buttons[i - 1]);
        break;
      case "Home":
        event.preventDefault();
        focus(buttons[0]);
        break;
      case "End":
        event.preventDefault();
        focus(buttons[buttons.length - 1]);
        break;
      case "ArrowRight":
        if (!node || !node.isDir) return;
        event.preventDefault();
        if (!node.expanded) await expand(node);
        else focus(node.list.querySelector(":scope > li > button.node"));
        break;
      case "ArrowLeft":
        if (!node) return;
        event.preventDefault();
        if (node.isDir && node.expanded) collapse(node);
        else if (node.parent && node.parent.button) focus(node.parent.button);
        break;
      default:
    }
  });

  return {
    element: list,
    root,
    loadRoot: () => loadLevel(root),
    reveal,
    select,
    isEmpty: () => nodes.size === 0
  };
}
