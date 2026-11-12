// S35 Capabilities: the imported capabilities with their state, and whether this server acts on
// each type. Parts S36 shares live here too.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { pageHead, failure } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { mark } from "../../ui/notice.js";

// What the server says about a type; a type missing from the map counts as not supported.
export function supportOf(support, type) {
  if (!support) return { known: false, supported: false, reason: "The server did not say what it does with this type." };
  const entry = support[type];
  if (!entry) return { known: true, supported: false, reason: "This server does not know this type." };
  return { known: true, supported: Boolean(entry.supported), reason: entry.reason || "" };
}

export function supportMark(s) {
  if (!s.known) return mark("warn", "Unknown");
  return s.supported ? mark("ok", "Supported") : mark("off", "Not supported");
}

export function enabledMark(enabled) {
  return enabled ? mark("ok", "Enabled") : mark("off", "Disabled");
}

export async function render(ctx) {
  ctx.main.append(pageHead({ title: "Capabilities", sub: "Optional features imported with the settings of the earlier install, and whether this server acts on each type." }));
  let list;
  let support = null;
  try {
    const [caps, sup] = await Promise.allSettled([api.capabilities.list(), api.capabilities.support()]);
    if (caps.status === "rejected") throw caps.reason;
    list = caps.value || [];
    support = sup.status === "fulfilled" ? sup.value || {} : null;
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/capabilities", { replace: true }) });
    return;
  }
  const table = dataTable(
    [
      { key: "type", label: "Type", render: (c) => h("span", { class: "mono break", text: c.type }) },
      { key: "notes", label: "Notes", render: (c) => (c.notes ? h("span", { class: "break", text: c.notes }) : null) },
      { key: "enabled", label: "State", value: (c) => (c.enabled ? "Enabled" : "Disabled"), render: (c) => enabledMark(c.enabled) },
      {
        key: "support",
        label: "In this server",
        value: (c) => {
          const s = supportOf(support, c.type);
          return `${s.supported ? "Supported" : "Not supported"} ${s.reason}`;
        },
        render: (c) => {
          const s = supportOf(support, c.type);
          return h("div", { class: "cell-stack" }, supportMark(s), s.reason ? h("span", { class: "muted small", text: s.reason }) : null);
        }
      }
    ],
    {
      rows: list,
      sort: { key: "type", dir: "asc" },
      rowHref: (c) => `#/admin/capabilities/${encodeURIComponent(c.id)}`,
      caption: "Capabilities",
      filterLabel: "Filter the capabilities",
      empty: { title: "No capability", text: "No capability was imported." }
    }
  );
  table.el.classList.add("capabilities-table");
  ctx.main.append(table.el);
}
