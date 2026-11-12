// S05 Repository tree and file detail. The tree loads one folder level per request, 200 entries at
// a time, with a button for the next 200 (docs/ui/API.md, section S05). The open file or folder is
// kept in the address as ?path=, so a search result or an upload can link to it.
//
// Keys in the tree: up and down move between entries, right opens a folder or enters it, left
// closes it or goes to the folder above, Home and End go to the first and the last entry.

import { h, replace } from "../../dom.js";
import { api } from "../../api.js";
import { bytes, number, time, formatName, typeName, fieldLabel } from "../../format.js";
import { pageHead, failure, notFound, emptyState, loading, linkButton, facts, button } from "../../ui/page.js";
import { notice, toast, errorText, problem, toastProblem } from "../../ui/notice.js";
import { confirm } from "../../ui/dialog.js";
import { copyButton } from "../repositories/list.js";

const SERVED = { maven: "Maven repository", npm: "npm registry", docker: "Docker registry", files: "Plain files" };
import { mayUpload, treeHref } from "./common.js";

const PAGE = 200;

// The objects maven, npm and docker of the file detail (docs/ui/API.md, S05). A field that is not
// listed here gets a label from its name; commands get a copy button.
const COORDINATES = {
  maven: { title: "Maven", labels: { group_id: "Group ID", artifact_id: "Artifact ID", version: "Version", classifier: "Classifier", extension: "Extension" } },
  npm: { title: "npm", labels: { kind: "Kind", name: "Package", version: "Version", registry: "Registry", install: "Install command" } },
  docker: { title: "Docker", labels: { kind: "Kind", image: "Image", tag: "Tag", digest: "Digest", port: "Port", pull: "Pull command" } }
};
const KINDS = { tarball: "Package file", document: "Package document", manifest: "Image manifest", blob: "Layer or config blob" };
const COMMANDS = new Set(["install", "pull"]);
// The text of a fact the server did not record (docs/ui/API.md, S05).
const UNRECORDED = "Not recorded";

function narrow() {
  return window.matchMedia("(max-width: 1000px)").matches;
}

function value(v, fallback = null) {
  return v === null || v === undefined || v === "" ? fallback : v;
}

function mono(text) {
  return h("span", { class: "mono break", text });
}

function coordinateSection(kind, data) {
  if (!data || typeof data !== "object") return null;
  const { title, labels } = COORDINATES[kind];
  const shown = (key, v) => {
    if (key === "kind") return KINDS[v] || fieldLabel(v);
    if (COMMANDS.has(key)) return h("span", { class: "url-cell" }, mono(String(v)), copyButton(String(v), "Copy"));
    return mono(typeof v === "object" ? JSON.stringify(v) : String(v));
  };
  const rows = Object.entries(data)
    .filter(([, v]) => v !== null && v !== undefined && v !== "")
    .map(([key, v]) => [labels[key] || fieldLabel(key), shown(key, v)]);
  if (!rows.length) return null;
  return h("section", { class: "detail-section" }, h("h3", { text: title }), facts(rows, { empty: UNRECORDED }));
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
  ctx.setTitle(name);

  const actions = [];
  if (mayUpload(ctx, repo)) actions.push(linkButton("Upload", `#/upload/${encodeURIComponent(name)}`, "primary"));
  if (ctx.can("nexus:search:read")) actions.push(linkButton("Search here", `#/search?repository=${encodeURIComponent(name)}`));
  const kind = `${formatName(repo.format)} ${typeName(repo.type).toLowerCase()} repository`;
  ctx.main.append(
    pageHead({
      title: name,
      sub: repo.type === "group" ? `${kind}. It shows the files of its members, the first member first.` : `${kind}.`,
      crumbs: [{ label: "Browse", href: "#/browse" }],
      actions
    })
  );

  const treeList = h("ul", { class: "tree", "aria-label": `Folders and files of ${name}` });
  const nodes = new Map();
  const root = { path: "", name, isDir: true, parent: null, list: treeList, loaded: false, expanded: true, more: false, offset: 0, moreRow: null, button: null };
  let selected = null;
  let detailRun = 0;

  const detail = h("section", { class: "panel detail-panel", id: "asset-detail", "aria-label": "Detail" });
  const placeholder = () => emptyState({ title: "No file open", text: "Open a folder in the tree and pick a file to see its size, checksums and history." });

  // Tree

  function makeNode(entry, parent) {
    const node = { path: entry.path, name: entry.name, isDir: Boolean(entry.is_dir), size: entry.size, parent, loaded: false, expanded: false, more: false, offset: 0, moreRow: null };
    const btn = h(
      "button",
      { type: "button", class: "node", dataset: { path: entry.path }, "aria-expanded": node.isDir ? "false" : null },
      h("span", { class: ["kind", node.isDir && "dir"], "aria-hidden": "true" }),
      h("span", { class: "node-name break", text: entry.name }),
      !node.isDir && entry.size !== null && entry.size !== undefined ? h("span", { class: "meta", text: bytes(entry.size) }) : null
    );
    btn.addEventListener("click", () => activate(node));
    node.button = btn;
    node.li = h("li", { dataset: { path: entry.path } }, btn);
    if (node.isDir) {
      node.list = h("ul", { class: "tree-level", hidden: true, "aria-label": `Entries of ${entry.name}` });
      node.li.append(node.list);
    }
    nodes.set(node.path, node);
    return node;
  }

  function markEmpty(node) {
    const has = node.list.querySelector(":scope > li > button.node");
    const note = node.list.querySelector(":scope > li.empty-row");
    if (!has && !note && !node.more) node.list.append(h("li", { class: "empty-row", text: "This folder is empty." }));
    if (has && note) note.remove();
  }

  async function loadLevel(node) {
    const answer = await api.browse(name, node.path, PAGE, node.offset);
    const children = (answer && answer.children) || [];
    node.offset += children.length;
    node.more = Boolean(answer && answer.more) && children.length > 0;
    if (node.moreRow) {
      node.moreRow.remove();
      node.moreRow = null;
    }
    for (const child of children) {
      if (!nodes.has(child.path)) node.list.append(makeNode(child, node).li);
    }
    if (node.more) {
      const more = h("button", { type: "button", class: "btn small", text: "Load more", "aria-label": `Load more entries of ${node.path || name}` });
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
    more.setAttribute("aria-busy", "true");
    const before = node.offset;
    try {
      await loadLevel(node);
    } catch (error) {
      toastProblem(error);
      more.disabled = false;
      more.removeAttribute("aria-busy");
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

  function remember(path) {
    const target = treeHref(name, path);
    if (location.hash !== target) history.replaceState(null, "", target);
  }

  function select(node) {
    if (selected && selected.button) selected.button.removeAttribute("aria-current");
    selected = node;
    if (node.button) node.button.setAttribute("aria-current", "true");
    remember(node.path);
    if (node.isDir) showFolder(node);
    else showFile(node);
  }

  async function activate(node) {
    if (node.isDir) {
      if (node.expanded) collapse(node);
      else if (!(await expand(node))) return;
    }
    select(node);
  }

  // Opens every folder on the way to a path; loads further pages of a folder while the next name
  // is not among the entries loaded so far.
  async function reveal(path) {
    const segments = path.split("/").filter(Boolean);
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
    return [...treeList.querySelectorAll("button.node")].filter((b) => b.offsetParent !== null);
  }

  treeList.addEventListener("keydown", async (event) => {
    const current = event.target.closest("button.node");
    if (!current) return;
    const node = nodes.get(current.dataset.path);
    const list = visibleButtons();
    const i = list.indexOf(current);
    const focus = (b) => b && b.focus();
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        focus(list[i + 1]);
        break;
      case "ArrowUp":
        event.preventDefault();
        focus(list[i - 1]);
        break;
      case "Home":
        event.preventDefault();
        focus(list[0]);
        break;
      case "End":
        event.preventDefault();
        focus(list[list.length - 1]);
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

  // Detail

  function showFolder(node) {
    detailRun += 1;
    const entries = node.list.querySelectorAll(":scope > li > button.node").length;
    const parts = [
      h("div", { class: "detail-head" }, h("h2", { class: "break", text: node.name }), h("p", { class: "muted small mono break", text: node.path })),
      h("div", { class: "actions detail-actions" }, copyButton(node.path, "Copy path")),
      h("section", { class: "detail-section" }, h("h3", { text: "Folder" }), facts([["Entries", node.more ? `${number(entries)} loaded, more in the tree` : number(entries)]]))
    ];
    replace(detail, h("div", { class: "folder-detail" }, parts));
  }

  async function showFile(node) {
    const run = ++detailRun;
    replace(detail, loading("Loading the file"));
    let info;
    try {
      info = await api.asset(name, node.path);
    } catch (error) {
      if (run !== detailRun) return;
      replace(detail, problem(error, "The file could not be loaded"));
      return;
    }
    let asset = null;
    let assetError = null;
    if (info && info.asset_id) {
      try {
        asset = await api.assets.get(info.asset_id);
      } catch (error) {
        assetError = error;
      }
    }
    if (run !== detailRun) return;
    replace(detail, fileDetail(node, info || {}, asset, assetError));
    if (narrow()) detail.scrollIntoView({ block: "start" });
  }

  function fileDetail(node, info, asset, assetError) {
    const fileName = info.name || node.name;
    const member = info.member && info.member !== name ? info.member : null;
    const actions = [];
    if (info.download_url) actions.push(h("a", { class: "btn small", href: info.download_url, download: fileName, text: "Download" }));
    actions.push(copyButton(info.path || node.path, "Copy path"));
    if (info.asset_id && ctx.can(`nexus:repository-view:${repo.format}:${member || name}:delete`)) {
      actions.push(button("Delete", { kind: "danger", small: true, onClick: () => remove(node, info, fileName) }));
    }
    const sums = (asset && asset.checksum) || {};
    const checksums = [
      ["SHA-1", sums.sha1 || info.sha1],
      ["MD5", sums.md5],
      ["SHA-256", sums.sha256],
      ["SHA-512", sums.sha512]
    ]
      .filter(([, v]) => v)
      .map(([label, v]) => [label, h("span", { class: "sum" }, mono(v), copyButton(v))]);
    const fileRows = [
      ["Size", info.size !== null && info.size !== undefined ? `${bytes(info.size)} (${number(info.size)} bytes)` : null],
      ["Content type", value(info.content_type || (asset && asset.contentType)) && mono(info.content_type || asset.contentType)],
      ["Created", time(info.created_ms)],
      member ? ["From member", h("a", { href: treeHref(member, info.path), text: member })] : null,
      ["Blob store", value(info.store || (asset && asset.blobStoreName))],
      typeof info.access === "string" ? ["Served as", SERVED[info.access] || info.access] : null
    ];
    const holder = member ? repos.find((r) => r.name === member) : repo;
    const cached = Boolean(holder && holder.type === "proxy");
    const history = asset
      ? h(
          "section",
          { class: "detail-section" },
          h("h3", { text: "History" }),
          facts(
            [
              ["Uploaded by", value(asset.uploader, cached ? h("span", { class: "muted", text: "Cached from a remote" }) : null)],
              ["Uploaded from", value(asset.uploaderIp) && mono(asset.uploaderIp)],
              ["Blob created", time(asset.blobCreated)],
              ["Last modified", time(asset.lastModified)],
              ["Last downloaded", time(asset.lastDownloaded)]
            ],
            { empty: UNRECORDED }
          )
        )
      : null;
    return h(
      "div",
      { class: "file-detail" },
      h("div", { class: "detail-head" }, h("h2", { class: "break", text: fileName }), h("p", { class: "muted small mono break", text: info.path || node.path })),
      h("div", { class: "actions detail-actions" }, actions),
      assetError ? h("div", { class: "detail-section" }, notice("warning", "Part of the detail is missing", `The checksums beyond SHA-1 and the history could not be loaded. ${errorText(assetError)}`)) : null,
      !info.asset_id ? h("div", { class: "detail-section" }, notice("info", "Detail of this server only", "The server names no asset id for this file, so its history and the delete are not offered.")) : null,
      h("section", { class: "detail-section" }, h("h3", { text: "File" }), facts(fileRows.filter(Boolean), { empty: UNRECORDED })),
      h(
        "section",
        { class: "detail-section checksums" },
        h("h3", { text: "Checksums" }),
        checksums.length ? facts(checksums) : h("p", { class: "muted", text: UNRECORDED })
      ),
      coordinateSection("maven", info.maven),
      coordinateSection("npm", info.npm),
      coordinateSection("docker", info.docker),
      history
    );
  }

  async function remove(node, info, fileName) {
    const ok = await confirm({
      title: `Delete ${fileName}?`,
      text: `The file ${info.path || node.path} is removed from ${info.member || name}. Clients that ask for it get a not found answer. Its blob stays in the blob store until a compact task removes it.`,
      confirmLabel: "Delete file",
      danger: true
    });
    if (!ok) return;
    try {
      await api.assets.remove(info.asset_id);
    } catch (error) {
      toastProblem(error);
      return;
    }
    const parent = node.parent || root;
    node.li.remove();
    nodes.delete(node.path);
    parent.offset = Math.max(0, parent.offset - 1);
    markEmpty(parent);
    selected = null;
    detailRun += 1;
    remember(parent.path);
    replace(detail, emptyState({ title: "File deleted", text: `The file ${fileName} is gone from the tree. Pick another file to see its detail.` }));
    toast(`The file ${fileName} is deleted.`);
    const next = parent.button || treeList.querySelector("button.node");
    if (next) next.focus();
  }

  // First load

  const body = h("div", { class: "stack" }, loading("Loading the folders"));
  ctx.main.append(body);
  try {
    await loadLevel(root);
  } catch (error) {
    failure(ctx.main, error, { what: "repository", retry: () => location.reload() });
    return;
  }

  if (!nodes.size) {
    const text =
      repo.type === "proxy"
        ? "A proxy keeps the files clients asked it for. No client has asked yet."
        : repo.type === "group"
          ? "None of the members of this group holds a file yet."
          : mayUpload(ctx, repo)
            ? "Deploy to it with a client, or upload a file with the button above."
            : "Nothing is deployed to it yet.";
    replace(body, h("div", { class: "panel" }, emptyState({ title: "No files yet", text })));
    return;
  }

  const treePanel = h("section", { class: "panel tree-panel", "aria-labelledby": "tree-title" }, h("h2", { id: "tree-title", class: "visually-hidden", text: "Folders and files" }), treeList);
  const messages = h("div", { class: "stack" });
  const browser = h("div", { class: "browser" }, treePanel, detail);
  replace(body, messages, browser);
  replace(detail, placeholder());

  // Beside a tree taller than the window, the detail stays in view and scrolls on its own.
  const pin = () => browser.classList.toggle("pinned", treePanel.offsetHeight > window.innerHeight);
  const observer = typeof ResizeObserver === "function" ? new ResizeObserver(pin) : null;
  if (observer) observer.observe(treePanel);
  window.addEventListener("resize", pin);
  const page = {
    dispose() {
      if (observer) observer.disconnect();
      window.removeEventListener("resize", pin);
    }
  };

  const wanted = String(ctx.query.path || "").replace(/^\/+|\/+$/g, "");
  if (wanted) {
    let found = null;
    try {
      found = await reveal(wanted);
    } catch (error) {
      messages.append(problem(error, "The folders could not be loaded"));
      return page;
    }
    if (found) {
      select(found);
      found.button.scrollIntoView({ block: "nearest" });
    } else {
      messages.append(notice("warning", "Path not found", `The path ${wanted} is not in this repository. It may have been deleted or moved.`));
      remember("");
    }
  }
  return page;
}
