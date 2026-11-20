// S46 Viewer: the files of a repository read only, the tree on the left and the source on the right
// (docs/plans/browse-tools.md, leg V1). The open file or folder is kept in the address as ?path=.
// A binary file shows a short card with Download and a link to its detail in Browse; a file without
// a download address shows a card of its own.

import { h, replace } from "../../dom.js";
import { api } from "../../api.js";
import { bytes, number } from "../../format.js";
import { pageHead, failure, notFound, emptyState, loading, linkButton, facts, button } from "../../ui/page.js";
import { notice, problem } from "../../ui/notice.js";
import { treeHref } from "./common.js";
import { createFileTree } from "../../viewer/filetree.js";
import { loadFile, CAP_BYTES, CAP_LINES } from "../../viewer/load.js";
import { kindOf, kindLabel, viewHref } from "../../viewer/kinds.js";
import { mountViewer, scanFor } from "../../viewer/view.js";

function narrow() {
  return window.matchMedia("(max-width: 1000px)").matches;
}

function mono(text) {
  return h("span", { class: "mono break", text });
}

export async function render(ctx) {
  const name = ctx.params.repository;
  let repos;
  try {
    repos = await api.repositories.list();
  } catch (error) {
    failure(ctx.main, error, { what: "repository", retry: () => location.reload() });
    return;
  }
  const repo = repos.find((r) => r.name === name);
  if (!repo) {
    ctx.main.append(notFound("repository"));
    ctx.setTitle("Not found");
    return;
  }
  ctx.setTitle(`${name} viewer`);

  let current = String(ctx.query.path || "").replace(/^\/+|\/+$/g, "");
  const browseLink = linkButton("Open in Browse", treeHref(name, current));
  const filesButton = button("Files", { kind: "vfiles", attrs: { "aria-expanded": "false", "aria-controls": "viewer-tree" } });
  ctx.main.append(
    pageHead({
      title: name,
      sub: "The files of the repository, read only.",
      crumbs: [{ label: "Browse", href: "#/browse" }, { label: name, href: treeHref(name, "") }],
      actions: [filesButton, browseLink]
    })
  );

  const source = h("section", { class: "panel vsource-panel", "aria-label": "Source" });
  const messages = h("div", { class: "stack vmessages" });
  let viewer = null;
  let run = 0;

  const tree = createFileTree({ repository: name, onFile: (node) => openFile(node.path) });
  const treePanel = h("section", { class: "panel vtree-panel", id: "viewer-tree", "aria-labelledby": "viewer-tree-title" }, h("h2", { id: "viewer-tree-title", class: "visually-hidden", text: "Folders and files" }), tree.element);
  const screen = h("div", { class: "vscreen" }, treePanel, source);

  function showTree(open) {
    treePanel.classList.toggle("open", open);
    filesButton.setAttribute("aria-expanded", String(open));
  }
  filesButton.addEventListener("click", () => showTree(!treePanel.classList.contains("open")));

  function remember(path) {
    current = path;
    const target = viewHref(name, path);
    if (location.hash !== target) history.replaceState(null, "", target);
    browseLink.setAttribute("href", treeHref(name, path));
  }

  function unmount() {
    if (viewer) viewer.dispose();
    viewer = null;
  }

  function placeholder(title, text) {
    unmount();
    replace(source, emptyState({ title, text }));
  }

  function openFolder(path) {
    run += 1;
    remember(path);
    placeholder("No file open", "Pick a file in the tree to see its source.");
  }

  function head(info, extra = []) {
    const fileName = info.name || current.split("/").pop();
    const actions = [];
    if (info.download_url) actions.push(h("a", { class: "btn small", href: info.download_url, download: fileName, text: "Download" }));
    actions.push(linkButton("Detail in Browse", treeHref(name, info.path || current), "small"));
    return h(
      "div",
      { class: "vsource-head" },
      h("div", { class: "titles" }, h("h2", { class: "break", text: fileName }), h("p", { class: "muted small mono break", text: info.path || current })),
      h("div", { class: "actions" }, actions),
      ...extra
    );
  }

  // The card of a file the viewer does not show: a binary file, or one without a download address.
  function card(info, size, binary = true) {
    unmount();
    replace(
      source,
      h(
        "div",
        { class: "vcard" },
        head(info),
        binary
          ? notice("info", "Binary file", "The first bytes of this file hold a NUL byte, so the viewer does not show it as text. Download it, or see its detail in Browse.")
          : notice("info", "No download address", "The server gives no download address for this file, so the viewer cannot read it. See its detail in Browse."),
        facts(
          [
            ["Size", typeof size === "number" ? `${bytes(size)} (${number(size)} bytes)` : null],
            ["Content type", info.content_type ? mono(info.content_type) : null],
            ["SHA-1", info.sha1 ? mono(info.sha1) : null]
          ],
          { empty: "Not recorded" }
        )
      )
    );
  }

  async function openFile(path) {
    const mine = ++run;
    remember(path);
    unmount();
    replace(source, loading("Loading the file"));
    if (narrow()) {
      // The panel closes over the focused node; the focus goes to the button that opens it again.
      const inTree = treePanel.contains(document.activeElement);
      showTree(false);
      if (inTree) filesButton.focus();
    }
    let info;
    try {
      info = (await api.asset(name, path)) || {};
    } catch (error) {
      if (mine !== run) return;
      replace(source, head({ path }), problem(error, "The file could not be loaded"));
      return;
    }
    if (mine !== run) return;
    if (!info.download_url) {
      card(info, info.size, false);
      return;
    }
    let result;
    try {
      result = await loadFile(info);
    } catch (error) {
      result = { state: "refused", message: error.message };
    }
    if (mine !== run) return;
    if (result.state === "binary") {
      card(info, result.size ?? info.size);
      return;
    }
    if (result.state === "empty") {
      replace(source, head(info), emptyState({ title: "This file is empty", text: "It holds no bytes." }));
      return;
    }
    if (result.state === "refused") {
      replace(source, head(info), notice("error", "The file could not be loaded", result.message || "The server refused the file."));
      return;
    }
    const kind = kindOf(info.name || path, info.content_type);
    const notes = [];
    if (result.cut) {
      const upTo = result.partialLine ? "which hold no line end, up to the last whole character" : "up to the last whole line";
      notes.push(notice("warning", "First part only", `The file has ${bytes(result.size)}. The viewer shows its first ${bytes(CAP_BYTES)} ${upTo}. Download it for the rest.`));
    }
    if (result.linesCut) notes.push(notice("warning", "First lines only", `The viewer shows the first ${number(CAP_LINES)} lines. Download the file for the rest.`));
    const status = h("p", { class: "vstatus muted small", role: "status" });
    const host = h("div", { class: "vsource-body" });
    replace(source, head(info, [status]), ...notes, host);
    let scan = null;
    try {
      scan = await scanFor(kind, result.text);
    } catch (error) {
      scan = null; // the text stays plain
    }
    if (mine !== run) return;
    viewer = mountViewer(host, { text: result.text, name: info.name || path, scan });
    status.textContent = `${kindLabel(kind)}, ${number(viewer.lines.length)} lines, ${bytes(result.size)}`;
  }

  replace(source, loading("Loading the folders"));
  ctx.main.append(messages, screen);
  try {
    await tree.loadRoot();
  } catch (error) {
    failure(ctx.main, error, { what: "repository", retry: () => location.reload() });
    return;
  }
  if (tree.isEmpty()) {
    replace(source, emptyState({ title: "No files yet", text: "This repository holds no file." }));
  } else if (!current) {
    placeholder("No file open", "Pick a file in the tree to see its source.");
    showTree(true);
  } else {
    let found = null;
    try {
      found = await tree.reveal(current);
    } catch (error) {
      messages.append(problem(error, "The folders could not be loaded"));
    }
    if (found) {
      tree.select(found);
      found.button.scrollIntoView({ block: "nearest" });
      if (found.isDir) {
        openFolder(found.path);
        showTree(true);
      } else {
        openFile(found.path);
      }
    } else {
      messages.append(notice("warning", "Path not found", `The path ${current} is not in this repository. It may have been deleted or moved.`));
      remember("");
      placeholder("No file open", "Pick a file in the tree to see its source.");
      showTree(true);
    }
  }
  return {
    dispose() {
      run += 1;
      unmount();
    }
  };
}
