// S21 Roles.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { number } from "../../format.js";
import { pageHead, linkButton, failure } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { mark } from "../../ui/notice.js";
import { tags } from "./common.js";

export async function render(ctx) {
  const canCreate = ctx.can("nexus:roles:create");
  const create = () => (canCreate ? [linkButton("Create role", "#/admin/roles/new", "primary")] : []);
  ctx.main.append(
    pageHead({
      title: "Roles",
      sub: "A role grants its privileges and those of the roles it contains. A role whose id is the name of a directory group applies to every member of that group.",
      actions: create()
    })
  );
  let rows;
  try {
    rows = await api.roles.list();
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/roles", { replace: true }) });
    return;
  }
  const table = dataTable(
    [
      { key: "name", label: "Name", class: "break" },
      { key: "id", label: "Id", render: (r) => h("span", { class: "mono small break", text: r.id }) },
      { key: "description", label: "Description" },
      { key: "privileges", label: "Privileges", num: true, value: (r) => (r.privileges || []).length, render: (r) => number((r.privileges || []).length) },
      { key: "roles", label: "Contained roles", sort: false, value: (r) => (r.roles || []).join(" "), render: (r) => tags(r.roles) },
      { key: "kind", label: "Kind", value: (r) => (r.readOnly ? "Built in" : "Custom"), render: (r) => (r.readOnly ? mark("info", "Built in") : mark("", "Custom")) }
    ],
    {
      rows,
      sort: { key: "name", dir: "asc" },
      rowHref: (r) => `#/admin/roles/${encodeURIComponent(r.id)}`,
      caption: "Roles",
      filterLabel: "Filter the roles",
      empty: { title: "No role yet", text: "Create a role to group privileges for users and directory groups.", actions: create() }
    }
  );
  ctx.main.append(table.el);
}
