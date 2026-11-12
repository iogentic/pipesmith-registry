// S31 Log files: the files of the log directory with size and time; each opens in S32.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { bytes, time } from "../../format.js";
import { pageHead, failure } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";

export function viewerHref(name) {
  return `#/admin/logs/${String(name).split("/").map(encodeURIComponent).join("/")}`;
}

export async function render(ctx) {
  ctx.main.append(pageHead({ title: "Log files", sub: "The files in the log directory of the server. Open one to read its end or to download it whole." }));
  let rows;
  try {
    rows = await api.logs.list();
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/logs", { replace: true }) });
    return;
  }
  const table = dataTable(
    [
      { key: "name", label: "Name", render: (r) => h("span", { class: "mono break", text: r.name }) },
      { key: "size", label: "Size", num: true, render: (r) => bytes(r.size) },
      { key: "lastModified", label: "Last written", render: (r) => h("span", { class: "nowrap", text: time(r.lastModified) }) }
    ],
    {
      rows,
      sort: { key: "name", dir: "asc" },
      rowHref: (r) => viewerHref(r.name),
      caption: "Log files",
      filterLabel: "Filter the log files",
      empty: { title: "No log file", text: "The server has written no log file yet." }
    }
  );
  table.el.classList.add("logs-table");
  ctx.main.append(table.el);
}
