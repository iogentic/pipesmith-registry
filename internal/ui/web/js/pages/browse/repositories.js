// S04 Browse: the repositories the session may browse, joined with the counts of the server by name.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { bytes, number, time, formatName, typeName } from "../../format.js";
import { pageHead, failure } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { notice } from "../../ui/notice.js";
import { urlCell } from "../repositories/list.js";

// The counts answer by name. A list of rows is the form of the mock; a map by name is taken too.
function countsByName(data) {
  const rows = Array.isArray(data)
    ? data
    : data && Array.isArray(data.repositories)
      ? data.repositories
      : data && typeof data === "object"
        ? Object.entries(data).map(([name, value]) => ({ name, ...(value || {}) }))
        : [];
  const out = new Map();
  for (const row of rows) {
    const name = row && (row.name || row.repo || row.repository);
    if (name) out.set(name, row);
  }
  return out;
}

function missing(repo) {
  return h("span", { class: "muted nowrap", text: repo.type === "group" ? "In members" : "Not counted" });
}

export async function render(ctx) {
  ctx.main.append(pageHead({ title: "Browse", sub: "The repositories you may browse, with the files each one holds. Open one to see its folders and files." }));
  const [list, counts] = await Promise.allSettled([api.repositories.list(), api.repositories.counts()]);
  if (list.status === "rejected") {
    failure(ctx.main, list.reason, { retry: () => location.reload() });
    return;
  }
  const known = counts.status === "fulfilled" ? countsByName(counts.value) : new Map();
  if (counts.status === "rejected") {
    ctx.main.append(h("div", { class: "stack browse-note" }, notice("warning", "No counts", "The server did not answer with the counts of files. The list below is complete; the counts are missing.")));
  }
  // A group holds no files of its own; the server counts zero for it, the page says where the files are.
  const rows = (list.value || []).map((r) => ({ ...r, counts: r.type === "group" ? null : known.get(r.name) || null }));
  const count = (key) => (r) => (r.counts && r.counts[key] !== undefined && r.counts[key] !== null ? Number(r.counts[key]) : null);

  const table = dataTable(
    [
      { key: "name", label: "Name", class: "name-cell" },
      { key: "type", label: "Type", value: (r) => typeName(r.type) },
      { key: "format", label: "Format", value: (r) => formatName(r.format) },
      { key: "files", label: "Files", num: true, value: count("files"), render: (r) => (r.counts ? number(count("files")(r) ?? 0) : missing(r)) },
      { key: "bytes", label: "Size", num: true, value: count("bytes"), render: (r) => (r.counts ? bytes(count("bytes")(r) ?? 0) : missing(r)) },
      {
        key: "newest",
        label: "Newest file",
        class: "nowrap",
        value: count("newest_ms"),
        render: (r) => (r.counts ? (count("newest_ms")(r) ? time(count("newest_ms")(r)) : h("span", { class: "muted", text: "No file yet" })) : missing(r))
      },
      { key: "url", label: "Address", sort: false, render: (r) => urlCell(r.url) }
    ],
    {
      rows,
      sort: { key: "name", dir: "asc" },
      rowHref: (r) => `#/browse/${encodeURIComponent(r.name)}`,
      caption: "Repositories to browse",
      filterLabel: "Filter the repositories",
      empty: { title: "Nothing to browse", text: "None of your roles lets you browse a repository. An administrator can add the browse right to one of them." }
    }
  );
  table.el.classList.add("browse-table");
  ctx.main.append(table.el);
}
