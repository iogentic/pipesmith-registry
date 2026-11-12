// S09 Repositories.

import { h, copyText } from "../../dom.js";
import { api } from "../../api.js";
import { formatName, typeName } from "../../format.js";
import { pageHead, linkButton, failure } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { mark, toast } from "../../ui/notice.js";

export function copyButton(text, label = "Copy") {
  const button = h("button", { type: "button", class: "btn small quiet", text: label, "aria-label": `Copy ${text}` });
  button.addEventListener("click", async () => {
    toast((await copyText(text)) ? "Copied to the clipboard." : "The browser refused the clipboard. Select the text and copy it.", "info");
  });
  return button;
}

export function urlCell(url) {
  return h("span", { class: "url-cell" }, h("span", { class: "mono small", text: url }), copyButton(url));
}

export async function render(ctx) {
  const canAdd = ctx.can("nexus:repository-admin:*:*:add");
  ctx.main.append(
    pageHead({
      title: "Repositories",
      sub: "Hosted repositories keep what is deployed to them, proxies cache a remote, groups combine both under one address.",
      actions: canAdd ? [linkButton("Create repository", "#/admin/repositories/new", "primary")] : []
    })
  );
  let rows;
  try {
    rows = await api.repositories.settings();
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/repositories", { replace: true }) });
    return;
  }
  const table = dataTable(
    [
      { key: "name", label: "Name" },
      { key: "type", label: "Type", value: (r) => typeName(r.type) },
      { key: "format", label: "Format", value: (r) => formatName(r.format) },
      { key: "store", label: "Blob store", value: (r) => (r.storage || {}).blobStoreName },
      { key: "online", label: "State", value: (r) => (r.online ? "Online" : "Offline"), render: (r) => (r.online ? mark("ok", "Online") : mark("off", "Offline")) },
      { key: "url", label: "Address", sort: false, render: (r) => urlCell(r.url) }
    ],
    {
      rows,
      sort: { key: "name", dir: "asc" },
      rowHref: (r) => `#/admin/repositories/${encodeURIComponent(r.name)}`,
      caption: "Repositories",
      filterLabel: "Filter the repositories",
      empty: {
        title: "No repository yet",
        text: "Create a hosted repository to deploy to, or a proxy to cache a remote.",
        actions: canAdd ? [linkButton("Create repository", "#/admin/repositories/new", "primary")] : []
      }
    }
  );
  ctx.main.append(table.el);
}
