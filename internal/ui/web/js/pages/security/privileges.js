// S19 Privileges: read only. The stored privileges and those the server generates per repository.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { pageHead, failure } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { permissionOf, privilegeType } from "./common.js";

export async function render(ctx) {
  ctx.main.append(
    pageHead({
      title: "Privileges",
      sub: "The single rights that roles are made of. The server ships them or generates them for each repository, so they are read only here."
    })
  );
  let rows;
  try {
    rows = await api.privileges.list();
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/privileges", { replace: true }) });
    return;
  }

  const types = [...new Set(rows.map((p) => p.type))].sort((a, b) => privilegeType(a).localeCompare(privilegeType(b)));
  const typeSelect = h(
    "select",
    { class: "select", id: "privilege-type" },
    h("option", { value: "", text: "Every type" }),
    types.map((t) => h("option", { value: t, text: privilegeType(t) }))
  );
  const table = dataTable(
    [
      { key: "name", label: "Name", class: "break" },
      { key: "type", label: "Type", value: (p) => privilegeType(p.type) },
      { key: "permission", label: "Permission", value: (p) => permissionOf(p), render: (p) => h("span", { class: "mono small break", text: permissionOf(p) }) },
      { key: "description", label: "Description" }
    ],
    {
      rows,
      sort: { key: "name", dir: "asc" },
      rowHref: (p) => `#/admin/privileges/${encodeURIComponent(p.name)}`,
      caption: "Privileges",
      filterLabel: "Filter the privileges",
      tools: [h("div", { class: "inline-field" }, h("label", { for: "privilege-type", text: "Type" }), typeSelect)],
      empty: { title: "No privilege", text: "The server lists no privilege." }
    }
  );
  typeSelect.addEventListener("change", () => {
    table.setRows(typeSelect.value ? rows.filter((p) => p.type === typeSelect.value) : rows);
  });
  ctx.main.append(table.el);
}
