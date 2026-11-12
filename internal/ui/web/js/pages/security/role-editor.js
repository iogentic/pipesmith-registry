// S22 Role editor: create a local role or map an external group, change and delete a role. A role
// with readOnly true opens read only. The request bodies follow docs/ui/API.md, section S22.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { pageHead, failure, button, busy, notFound } from "../../ui/page.js";
import { form } from "../../ui/form.js";
import { notice, toast, errorText, problem, toastProblem } from "../../ui/notice.js";
import { confirm } from "../../ui/dialog.js";
import { ID_PATTERN, roleLabel, renamed, tags } from "./common.js";

function settled(result, fallback) {
  return result.status === "fulfilled" ? result.value || fallback : fallback;
}

export async function render(ctx) {
  const creating = !ctx.params.id;
  const roleId = ctx.params.id;
  const canPrivileges = ctx.can("nexus:privileges:read");

  let role = null;
  let privileges = [];
  let roles = [];
  let sources = [];
  const groups = {};
  try {
    const loads = await Promise.allSettled([
      creating ? Promise.resolve(null) : api.roles.get(roleId),
      canPrivileges ? api.privileges.list() : Promise.resolve([]),
      api.roles.list(),
      api.userSources()
    ]);
    if (loads[0].status === "rejected") throw loads[0].reason;
    role = loads[0].value;
    privileges = settled(loads[1], []);
    roles = settled(loads[2], []);
    sources = settled(loads[3], []).filter((s) => s.id !== "default");
    const lists = await Promise.allSettled(sources.map((s) => api.roles.list(s.id)));
    sources.forEach((s, i) => {
      groups[s.id] = settled(lists[i], []).map((g) => g.id);
    });
  } catch (error) {
    failure(ctx.main, error, { what: "role", retry: () => location.reload() });
    return;
  }
  if (!creating && !role) {
    ctx.main.append(notFound("role"));
    return;
  }

  const mappedTo = role ? sources.find((s) => (groups[s.id] || []).includes(role.id)) : null;
  const readOnlyRole = Boolean(role && role.readOnly);
  const canEdit = creating || (ctx.can("nexus:roles:update") && !readOnlyRole);
  const canDelete = !creating && !readOnlyRole && ctx.can("nexus:roles:delete");
  const title = creating ? "New role" : role.name || role.id;
  ctx.setTitle(title);

  const actions = [];
  if (canDelete) {
    actions.push(
      button("Delete", {
        kind: "danger",
        onClick: async () => {
          const ok = await confirm({
            title: `Delete ${role.name || role.id}?`,
            text: "Users and roles that have this role lose its privileges. For a mapped role, the members of the directory group lose them too.",
            confirmLabel: "Delete role",
            danger: true
          });
          if (!ok) return;
          try {
            await api.roles.remove(role.id);
            ctx.setDirty(null);
            toast(`The role ${role.id} is deleted.`);
            ctx.navigate("/admin/roles");
          } catch (error) {
            toastProblem(error);
          }
        }
      })
    );
  }
  ctx.main.append(
    pageHead({
      title,
      sub: creating ? "A local role, or a role for the members of a directory group." : readOnlyRole ? "Built in role" : mappedTo ? "Mapped role" : "Local role",
      crumbs: [{ label: "Roles", href: "#/admin/roles" }],
      actions
    })
  );

  const messages = h("div", { class: "stack page-messages" });
  if (readOnlyRole) messages.append(notice("info", "Read only", "The server ships this role. It cannot change or be deleted."));
  else if (!canEdit) messages.append(notice("info", "Read only", ctx.readOnlyText("Your roles let you read this role, not change it.")));
  if (mappedTo) messages.append(notice("info", "Directory group", `Mapped to the group ${role.id} of ${mappedTo.name}. Every member of that group gets this role when signing in.`));

  const roleOptions = roles.filter((r) => !role || r.id !== role.id).map((r) => ({ value: r.id, label: roleLabel(r) }));
  const known = new Set(privileges.map((p) => p.name));
  const privilegeOptions = privileges.map((p) => ({ value: p.name, label: p.name }));
  if (role) for (const name of role.privileges || []) if (!known.has(name)) privilegeOptions.push({ value: name, label: name });

  const firstSource = sources[0] ? sources[0].id : null;
  let values = creating
    ? { kind: "local", source: firstSource, group: null, id: "", name: "", description: "", privileges: [], roles: [] }
    : { ...role, privileges: role.privileges || [], roles: role.roles || [] };

  const idFields = creating
    ? [
        {
          name: "kind",
          label: "Kind",
          type: "radio",
          wide: true,
          options: [
            { value: "local", label: "Local role" },
            { value: "mapped", label: "Mapped external group" }
          ],
          help: sources.length ? "A mapped role takes the name of a directory group as its id and applies to every member of that group." : "No external user source is set up, so only local roles can be created."
        },
        { name: "id", label: "Id", type: "text", required: true, mono: true, pattern: ID_PATTERN, help: "Users and other roles refer to this id. It cannot change later.", show: (v) => v.kind !== "mapped" },
        {
          name: "source",
          label: "Source",
          type: "select",
          required: true,
          noBlank: true,
          options: sources.map((s) => ({ value: s.id, label: s.name })),
          show: (v) => v.kind === "mapped"
        },
        {
          name: "group",
          label: "Group",
          type: "select",
          required: true,
          blank: "Choose a group",
          options: (v) => (groups[v.source] || []).filter((g) => !roles.some((r) => r.id === g)).map((g) => ({ value: g, label: g })),
          help: "Groups that already have a role are not offered.",
          show: (v) => v.kind === "mapped"
        }
      ]
    : [{ name: "id", label: "Id", type: "static", mono: true }];

  const sections = [
    {
      title: "Role",
      grid: true,
      fields: [
        ...idFields,
        { name: "name", label: "Name", type: "text", required: true, autocomplete: "off" },
        { name: "description", label: "Description", type: "textarea", rows: 2, wide: true }
      ]
    },
    {
      fields: [
        canPrivileges
          ? { name: "privileges", label: "Privileges", type: "transfer", availableLabel: "Available", selectedLabel: "Applied", help: "The rights this role grants. Filter the list by name, for example nx-repository-view." }
          : { name: "privileges", label: "Privileges", type: "static", render: (v) => tags(v) || h("span", { class: "muted", text: "None" }) }
      ]
    },
    {
      fields: [
        {
          name: "roles",
          label: "Contained roles",
          type: "transfer",
          availableLabel: "Available roles",
          selectedLabel: "Contained",
          options: roleOptions,
          help: "The role also grants every privilege of the roles it contains."
        }
      ]
    }
  ];
  if (canPrivileges) sections[1].fields[0].options = privilegeOptions;

  const f = form({ sections, values, readOnly: !canEdit });
  ctx.setDirty(() => f.isDirty());

  const save = h("button", { type: "submit", class: "btn primary", text: creating ? "Create role" : "Save" });
  const discard = h("button", { type: "button", class: "btn", text: creating ? "Cancel" : "Discard changes" });
  const formEl = h("form", { novalidate: true, class: "panel" }, f.el, canEdit ? h("div", { class: "form-actions" }, save, discard) : null);
  ctx.main.append(messages, formEl);

  discard.addEventListener("click", () => {
    if (creating) {
      ctx.setDirty(null);
      ctx.navigate("/admin/roles");
    } else {
      f.set(values);
      f.clearErrors();
    }
  });

  formEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    messages.replaceChildren();
    if (!f.validate()) return;
    const v = f.values();
    const mapped = creating && v.kind === "mapped";
    const id = creating ? (mapped ? v.group : v.id.trim()) : role.id;
    const body = { id, name: v.name.trim(), description: v.description || "", privileges: v.privileges || [], roles: v.roles || [] };
    try {
      await busy(save, () => (creating ? api.roles.create(body) : api.roles.update(id, body)));
    } catch (error) {
      const shown = f.showServerErrors(mapped ? renamed(error, { id: "group" }) : error);
      if (!shown) messages.append(problem(error, creating ? "Not created" : "Not saved"));
      return;
    }
    f.markClean();
    if (creating) {
      ctx.setDirty(null);
      toast(`The role ${id} is created.`);
      ctx.navigate(`/admin/roles/${encodeURIComponent(id)}`);
      return;
    }
    try {
      role = await api.roles.get(id);
      values = { ...role, privileges: role.privileges || [], roles: role.roles || [] };
      f.set(values);
    } catch (error) {
      /* the save went through; the form keeps what was sent */
    }
    toast(`The role ${id} is saved.`);
  });
}
