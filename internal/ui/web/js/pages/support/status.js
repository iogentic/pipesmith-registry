// S33 Status: the health checks of the server, failing ones first, and whether it accepts writes.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { time } from "../../format.js";
import { pageHead, failure, button, busy, emptyState } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { mark, errorText } from "../../ui/notice.js";

function rowsOf(checks) {
  return Object.entries(checks || {})
    .map(([name, c]) => ({ name, ...(c || {}) }))
    .sort((a, b) => Number(Boolean(a.healthy)) - Number(Boolean(b.healthy)) || a.name.localeCompare(b.name, "en"));
}

function detailLines(check) {
  const lines = [];
  if (check.details && typeof check.details === "object") {
    for (const [key, value] of Object.entries(check.details)) lines.push(h("li", { class: "muted small", text: `${key}: ${typeof value === "object" ? JSON.stringify(value) : value}` }));
  }
  return lines.length ? h("ul", { class: "check-details" }, lines) : null;
}

export async function render(ctx) {
  const again = button("Check again", { attrs: { id: "status-refresh" }, onClick: (event) => busy(event.currentTarget, load) });
  ctx.main.append(pageHead({ title: "Status", sub: "Health checks of this server, failing ones first.", actions: [again] }));
  const body = h("div", { class: "stack" });
  ctx.main.append(body);

  const table = dataTable(
    [
      { key: "name", label: "Check", render: (c) => h("strong", { class: "break", text: c.name }) },
      { key: "healthy", label: "State", value: (c) => (c.healthy ? "Healthy" : "Failing"), render: (c) => (c.healthy ? mark("ok", "Healthy") : mark("error", "Failing")) },
      {
        key: "message",
        label: "Message",
        render: (c) => h("div", { class: "cell-stack" }, h("span", { class: "break", text: c.message || "" }), c.error ? h("span", { class: "error-text mono break", text: String(c.error) }) : null, detailLines(c))
      },
      { key: "timestamp", label: "Checked", value: (c) => c.timestamp || c.time || "", render: (c) => h("span", { class: "nowrap", text: time(c.timestamp || c.time) }) }
    ],
    { rows: [], filter: false, caption: "Health checks", empty: { title: "No health check", text: "The server reports no health check." } }
  );

  const writable = h("div", { id: "writable", class: "writable" });
  const counts = h("p", { class: "status-count" });
  const writes = h("section", { class: "panel sys-panel", "aria-labelledby": "writes-title" }, h("h2", { id: "writes-title", text: "Write access" }), writable);

  async function load() {
    // A server known to be read only answers 503 here; the interface does not ask it then.
    const [checks, write] = await Promise.allSettled([api.statusChecks(), ctx.frozen() ? Promise.reject({ status: 503 }) : api.writable()]);
    if (checks.status === "rejected") {
      failure(body, checks.reason, { retry: load });
      return;
    }
    const rows = rowsOf(checks.value);
    const failing = rows.filter((r) => !r.healthy).length;
    counts.replaceChildren(
      failing ? mark("error", `${failing} of ${rows.length} failing`) : mark("ok", `${rows.length} of ${rows.length} healthy`),
      h("span", { class: "muted small", text: ` Read at ${time(Date.now())}.` })
    );
    table.setRows(rows);
    if (write.status === "fulfilled") {
      writable.replaceChildren(mark("ok", "Writable"), h("span", { text: " The server accepts uploads and changes." }));
    } else if (write.reason && write.reason.status === 503) {
      const link = ctx.can("nexus:settings:read") ? [" ", h("a", { href: "#/admin/node", text: "Open the node" })] : [];
      writable.replaceChildren(mark("error", "Read only"), h("span", { text: " The server refuses uploads and changes until it is released." }), ...link);
    } else {
      writable.replaceChildren(mark("warn", "Unknown"), h("span", { text: ` ${errorText(write.reason)}` }));
    }
    if (!body.contains(table.el)) body.replaceChildren(writes, counts, table.el);
  }

  await load();
  if (!body.childNodes.length) body.append(emptyState({ title: "No status", text: "The server reported nothing." }));
}
