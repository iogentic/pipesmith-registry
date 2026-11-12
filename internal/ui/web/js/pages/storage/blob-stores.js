// S12 Blob stores.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { bytes, number, plural } from "../../format.js";
import { pageHead, linkButton, failure } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { mark, notice } from "../../ui/notice.js";
import { overQuota, quotaText } from "./common.js";

function state(store) {
  if (store.unavailable) return "Unavailable";
  if (overQuota(store)) return "Over the limit";
  return "Available";
}

function stateMark(store) {
  if (store.unavailable) return mark("error", "Unavailable");
  if (overQuota(store)) return mark("warn", "Over the limit");
  return mark("ok", "Available");
}

function unknownWhenUnavailable(store, text) {
  return store.unavailable ? h("span", { class: "muted", text: "Unknown" }) : text;
}

function names(list) {
  const all = list.map((s) => s.name);
  return all.length === 1 ? all[0] : `${all.slice(0, -1).join(", ")} and ${all[all.length - 1]}`;
}

export async function render(ctx) {
  ctx.main.classList.add("area-storage");
  const canCreate = ctx.can("nexus:blobstores:create");
  const createButton = () => (canCreate ? [linkButton("Create blob store", "#/admin/blob-stores/new", "primary")] : []);
  ctx.main.append(
    pageHead({
      title: "Blob stores",
      sub: "Each store is a folder on a disk of the server. Every repository keeps its files in one store.",
      actions: createButton()
    })
  );
  let rows;
  try {
    rows = (await api.blobStores.list()) || [];
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/blob-stores", { replace: true }) });
    return;
  }

  const messages = h("div", { class: "stack store-messages" });
  const unavailable = rows.filter((s) => s.unavailable);
  if (unavailable.length) {
    messages.append(
      notice(
        "warning",
        unavailable.length === 1 ? "One store is unavailable" : `${unavailable.length} stores are unavailable`,
        `The server cannot read the folder of ${unavailable.length === 1 ? "the store" : "the stores"} ${names(unavailable)}. Repositories that use ${unavailable.length === 1 ? "it" : "them"} answer with errors until the folder is back.`
      )
    );
  }
  const over = rows.filter(overQuota);
  if (over.length) {
    messages.append(
      notice(
        "warning",
        over.length === 1 ? "One store is over its soft quota" : `${over.length} stores are over their soft quota`,
        `${over.length === 1 ? "The store" : "The stores"} ${names(over)} reached the limit of ${over.length === 1 ? "its" : "their"} soft quota. A soft quota only warns; the server goes on writing.`
      )
    );
  }
  if (messages.childNodes.length) ctx.main.append(messages);

  if (rows.length) {
    const blobs = rows.reduce((n, s) => n + (s.blobCount || 0), 0);
    const used = rows.reduce((n, s) => n + (s.totalSizeInBytes || 0), 0);
    ctx.main.append(h("p", { class: "muted store-summary", text: `${plural(rows.length, "store")} hold ${plural(blobs, "blob")} in ${bytes(used)}.` }));
  }

  const table = dataTable(
    [
      { key: "name", label: "Name" },
      { key: "type", label: "Type" },
      { key: "state", label: "State", value: state, render: stateMark },
      { key: "blobCount", label: "Blobs", num: true, render: (s) => unknownWhenUnavailable(s, number(s.blobCount)) },
      { key: "totalSizeInBytes", label: "Used", num: true, render: (s) => unknownWhenUnavailable(s, bytes(s.totalSizeInBytes)) },
      { key: "availableSpaceInBytes", label: "Free on disk", num: true, render: (s) => unknownWhenUnavailable(s, bytes(s.availableSpaceInBytes)) },
      { key: "quota", label: "Soft quota", value: (s) => quotaText(s.softQuota) }
    ],
    {
      rows,
      sort: { key: "name", dir: "asc" },
      rowHref: (s) => `#/admin/blob-stores/${encodeURIComponent(s.name)}`,
      caption: "Blob stores",
      filterLabel: "Filter the blob stores",
      empty: {
        title: "No blob store yet",
        text: "Create a store before the first repository. Every repository needs one.",
        actions: createButton()
      }
    }
  );
  ctx.main.append(table.el);
}
