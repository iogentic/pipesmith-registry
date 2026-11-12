// S20 Privilege detail: read only, with the roles that apply the privilege directly or through a
// contained role.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { formatName } from "../../format.js";
import { pageHead, failure, facts, emptyState, loading } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { notice, errorText, problem } from "../../ui/notice.js";
import { permissionOf, privilegeType } from "./common.js";

function actionsText(actions) {
  if (!actions || !actions.length) return "Every action";
  return actions.map((a) => (String(a).toUpperCase() === "ALL" ? "Every action" : String(a).toLowerCase())).join(", ");
}

function typeFacts(p) {
  const every = (value, word) => (!value || value === "*" ? `Every ${word}` : value);
  switch (p.type) {
    case "application":
      return [["Domain", h("span", { class: "mono", text: p.domain || "*" })], ["Actions", actionsText(p.actions)]];
    case "repository-view":
    case "repository-admin":
    case "repository-content-selector":
      return [
        p.type === "repository-content-selector" ? ["Content selector", p.contentSelector] : null,
        ["Format", !p.format || p.format === "*" ? "Every format" : formatName(p.format)],
        ["Repository", every(p.repository, "repository")],
        ["Actions", actionsText(p.actions)]
      ];
    case "wildcard":
      return [["Pattern", h("span", { class: "mono break", text: p.pattern || "" })]];
    case "script":
      return [["Script", p.scriptName], ["Actions", actionsText(p.actions)]];
    default:
      return [];
  }
}

// For each role: how it applies the privilege, directly or through which contained role.
function applying(roles, name) {
  const byId = new Map(roles.map((r) => [r.id, r]));
  const out = [];
  for (const role of roles) {
    if ((role.privileges || []).includes(name)) {
      out.push({ role, how: "Directly" });
      continue;
    }
    const seen = new Set([role.id]);
    const queue = (role.roles || []).map((id) => [id, id]);
    while (queue.length) {
      const [id, first] = queue.shift();
      if (seen.has(id)) continue;
      seen.add(id);
      const child = byId.get(id);
      if (!child) continue;
      if ((child.privileges || []).includes(name)) {
        out.push({ role, how: `Through ${byId.get(first) ? byId.get(first).name : first}` });
        break;
      }
      for (const next of child.roles || []) queue.push([next, first]);
    }
  }
  return out;
}

export async function render(ctx) {
  const name = ctx.params.name;
  let privilege;
  try {
    privilege = await api.privileges.get(name);
  } catch (error) {
    failure(ctx.main, error, { what: "privilege", retry: () => ctx.navigate(`/admin/privileges/${encodeURIComponent(name)}`, { replace: true }) });
    return;
  }
  ctx.setTitle(privilege.name);
  ctx.main.append(
    pageHead({
      title: privilege.name,
      sub: `${privilegeType(privilege.type)} privilege`,
      crumbs: [{ label: "Privileges", href: "#/admin/privileges" }]
    }),
    h(
      "section",
      { class: "panel" },
      facts([
        ["Description", privilege.description],
        ["Type", privilegeType(privilege.type)],
        ["Permission", h("code", { class: "break", text: permissionOf(privilege) })],
        ...typeFacts(privilege),
        ["Changes", privilege.readOnly ? "Built in. It cannot change." : "Read only in this interface."]
      ])
    )
  );

  const body = h("div");
  ctx.main.append(
    h("section", { class: "section privilege-roles" }, h("h2", { text: "Roles" }), h("p", { class: "help section-help", text: "The roles that grant this privilege, directly or through a role they contain." }), body)
  );
  if (!ctx.can("nexus:roles:read")) {
    body.append(notice("info", null, "Your roles do not include reading roles, so the roles that apply this privilege are not shown."));
    return;
  }
  body.append(loading());
  let roles;
  try {
    roles = await api.roles.list();
  } catch (error) {
    body.replaceChildren(problem(error, "The roles could not be loaded"));
    return;
  }
  const rows = applying(roles, privilege.name);
  if (!rows.length) {
    body.replaceChildren(h("div", { class: "table-wrap" }, emptyState({ title: "No role applies it", text: "Add the privilege to a role on the role page to grant it." })));
    return;
  }
  const table = dataTable(
    [
      { key: "name", label: "Role", value: (r) => r.role.name },
      { key: "id", label: "Id", value: (r) => r.role.id, render: (r) => h("span", { class: "mono small break", text: r.role.id }) },
      { key: "how", label: "Applied" }
    ],
    {
      rows,
      filter: false,
      sort: { key: "name", dir: "asc" },
      rowHref: ctx.can("nexus:roles:read") ? (r) => `#/admin/roles/${encodeURIComponent(r.role.id)}` : null,
      caption: "Roles that apply the privilege"
    }
  );
  body.replaceChildren(table.el);
}
