// S24 User editor: create a local user, change names, mail, state and roles, change the password
// through a dialog, delete, and revoke the tokens of the user. A directory user keeps names, mail
// and state from the directory; only its roles change here.

import { h, replace } from "../../dom.js";
import { api } from "../../api.js";
import { pageHead, failure, button, busy, notFound, loading, emptyState } from "../../ui/page.js";
import { form } from "../../ui/form.js";
import { notice, toast, errorText, problem, toastProblem } from "../../ui/notice.js";
import { confirm, formDialog } from "../../ui/dialog.js";
import { ID_PATTERN, REALM, STATUS, roleLabel, statusMark, tags, tokenTable } from "./common.js";

const MIN_PASSWORD = 8;

function fullName(u) {
  return [u.firstName, u.lastName].filter(Boolean).join(" ");
}

async function loadUser(source, userId) {
  const list = await api.users.list(source, userId);
  return (list || []).find((u) => u.userId === userId && u.source === source) || null;
}

function changePassword(userId) {
  const f = form({
    fields: [
      { name: "newPassword", label: "New password", type: "password", required: true, minLength: MIN_PASSWORD, autocomplete: "new-password", help: `At least ${MIN_PASSWORD} characters.` },
      {
        name: "confirmPassword",
        label: "New password again",
        type: "password",
        required: true,
        autocomplete: "new-password",
        validate: (value, values) => (value !== values.newPassword ? "The two passwords differ." : null)
      }
    ]
  });
  return formDialog({
    title: "Change password",
    form: f,
    submitLabel: "Change password",
    submit: (values) => api.users.changePassword(userId, values.newPassword)
  });
}

export async function render(ctx) {
  const creating = !ctx.params.id;
  const source = creating ? "default" : ctx.params.source;
  const userId = ctx.params.id;
  const canRoles = ctx.can("nexus:roles:read");

  let user = null;
  let roles = [];
  try {
    const loads = await Promise.allSettled([creating ? Promise.resolve(null) : loadUser(source, userId), canRoles ? api.roles.list() : Promise.resolve([])]);
    if (loads[0].status === "rejected") throw loads[0].reason;
    user = loads[0].value;
    roles = loads[1].status === "fulfilled" ? loads[1].value || [] : [];
  } catch (error) {
    failure(ctx.main, error, { what: "user", retry: () => location.reload() });
    return;
  }
  if (!creating && !user) {
    ctx.main.append(notFound("user"));
    return;
  }

  const directory = !creating && user.source !== "default";
  const self = !creating && ctx.session && ctx.session.user && ctx.session.user.userId === user.userId;
  const canEdit = creating || ctx.can("nexus:users:update");
  const canDelete = !creating && !directory && !self && ctx.can("nexus:users:delete");
  const canPassword = !creating && !directory && ctx.can("nexus:userschangepw:create");
  const canTokens = !creating && ctx.can("nexus:apikey:*") && ctx.can("nexus:users:update");
  const title = creating ? "New user" : user.userId;
  ctx.setTitle(title);

  const actions = [];
  if (canPassword) {
    actions.push(
      button("Change password", {
        onClick: async () => {
          if ((await changePassword(user.userId)) === "submitted") toast(`The password of ${user.userId} is changed.`);
        }
      })
    );
  }
  if (canDelete) {
    actions.push(
      button("Delete", {
        kind: "danger",
        onClick: async () => {
          const ok = await confirm({
            title: `Delete ${user.userId}?`,
            text: "The user can no longer sign in, and its tokens are revoked. This cannot be undone.",
            confirmLabel: "Delete user",
            danger: true
          });
          if (!ok) return;
          try {
            await api.users.remove(user.userId, REALM.local);
            ctx.setDirty(null);
            toast(`The user ${user.userId} is deleted.`);
            ctx.navigate("/admin/users");
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
      sub: creating ? "A local user who signs in with a password." : directory ? `Directory user, ${fullName(user) || user.userId}` : fullName(user) || "Local user",
      crumbs: [{ label: "Users", href: directory ? `#/admin/users?source=${encodeURIComponent(user.source)}` : "#/admin/users" }],
      actions
    })
  );

  const messages = h("div", { class: "stack page-messages" });
  if (directory) messages.append(notice("info", "Directory user", "This user comes from an LDAP directory. Names, mail and state come from the directory; only the roles change here."));
  if (!canEdit) messages.append(notice("info", "Read only", ctx.readOnlyText("Your roles let you read this user, not change it.")));
  if (self && canEdit) messages.append(notice("info", "Your own user", "Removing your own roles can take away the rights you use on this page."));

  const roleOptions = roles.map((r) => ({ value: r.id, label: roleLabel(r) }));
  const known = new Set(roles.map((r) => r.id));
  if (user) for (const id of user.roles || []) if (!known.has(id)) roleOptions.push({ value: id, label: id });
  const mapped = (group) => roles.find((r) => r.id === group);

  const statusOptions = Object.entries(STATUS).map(([value, s]) => ({ value, label: s.label }));
  const accountFields = directory
    ? [
        { name: "userId", label: "User id", type: "static", mono: true },
        { name: "fullName", label: "Name", type: "static" },
        { name: "emailAddress", label: "Mail", type: "static" },
        { name: "status", label: "State", type: "static", render: (v) => statusMark(v) },
        {
          name: "externalRoles",
          label: "Directory groups",
          type: "static",
          wide: true,
          render: (v) =>
            v && v.length
              ? h("span", { class: "tags" }, v.map((g) => h("span", { class: "tag", text: mapped(g) ? `${g} (role ${mapped(g).name})` : g })))
              : h("span", { class: "muted", text: "None" })
        }
      ]
    : [
        creating
          ? { name: "userId", label: "User id", type: "text", required: true, mono: true, pattern: ID_PATTERN, autocomplete: "off", help: "Used to sign in. It cannot change later." }
          : { name: "userId", label: "User id", type: "static", mono: true },
        { name: "status", label: "State", type: "select", required: true, noBlank: true, options: statusOptions, help: "Only an active user can sign in." },
        { name: "firstName", label: "First name", type: "text", required: true, autocomplete: "off" },
        { name: "lastName", label: "Last name", type: "text", required: true, autocomplete: "off" },
        { name: "emailAddress", label: "Mail", type: "email", required: true, wide: true, autocomplete: "off" }
      ];

  const sections = [{ title: "Account", grid: true, fields: accountFields }];
  if (creating) {
    sections.push({
      title: "Password",
      grid: true,
      fields: [
        { name: "password", label: "Password", type: "password", required: true, minLength: MIN_PASSWORD, autocomplete: "new-password", help: `At least ${MIN_PASSWORD} characters.` },
        {
          name: "passwordConfirm",
          label: "Password again",
          type: "password",
          required: true,
          autocomplete: "new-password",
          validate: (value, values) => (value !== values.password ? "The two passwords differ." : null)
        }
      ]
    });
  }
  const rolesHelp = directory ? "Roles granted here add to the roles the directory groups map to." : "The user gets every privilege of these roles and of the roles they contain.";
  sections.push({
    fields: [
      canRoles
        ? { name: "roles", label: "Roles", type: "transfer", required: !directory, availableLabel: "Available roles", selectedLabel: "Granted roles", options: roleOptions, help: rolesHelp }
        : { name: "roles", label: "Roles", type: "static", render: (v) => tags(v) || h("span", { class: "muted", text: "None" }), help: rolesHelp }
    ]
  });

  const initial = () =>
    creating
      ? { userId: "", firstName: "", lastName: "", emailAddress: "", status: "active", password: "", passwordConfirm: "", roles: [] }
      : { ...user, fullName: fullName(user), roles: user.roles || [] };
  let values = initial();
  const f = form({ sections, values, readOnly: !canEdit || (!canRoles && directory) });
  ctx.setDirty(() => f.isDirty());

  const save = h("button", { type: "submit", class: "btn primary", text: creating ? "Create user" : "Save" });
  const discard = h("button", { type: "button", class: "btn", text: creating ? "Cancel" : "Discard changes" });
  const editable = canEdit && (canRoles || !directory);
  const formEl = h("form", { novalidate: true, class: "panel" }, f.el, editable ? h("div", { class: "form-actions" }, save, discard) : null);
  ctx.main.append(messages, formEl);

  discard.addEventListener("click", () => {
    if (creating) {
      ctx.setDirty(null);
      ctx.navigate("/admin/users");
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
    let body;
    if (creating) {
      body = { userId: v.userId.trim(), firstName: v.firstName.trim(), lastName: v.lastName.trim(), emailAddress: v.emailAddress.trim(), password: v.password, status: v.status, roles: v.roles || [] };
    } else if (directory) {
      body = { ...user, roles: v.roles || [] };
    } else {
      body = { ...user, firstName: v.firstName.trim(), lastName: v.lastName.trim(), emailAddress: v.emailAddress.trim(), status: v.status, roles: v.roles || [] };
    }
    try {
      await busy(save, () => (creating ? api.users.create(body) : api.users.update(user.userId, body)));
    } catch (error) {
      if (!f.showServerErrors(error)) messages.append(problem(error, creating ? "Not created" : "Not saved"));
      return;
    }
    f.markClean();
    if (creating) {
      ctx.setDirty(null);
      toast(`The user ${body.userId} is created.`);
      ctx.navigate(`/admin/users/default/${encodeURIComponent(body.userId)}`);
      return;
    }
    try {
      user = (await loadUser(source, user.userId)) || user;
      values = initial();
      f.set(values);
    } catch (error) {
      /* the save went through; the form keeps what was sent */
    }
    toast(`The user ${user.userId} is saved.`);
  });

  if (!canTokens) return;
  const holder = h("div");
  ctx.main.append(
    h(
      "section",
      { class: "section user-tokens" },
      h("h2", { text: "Tokens" }),
      h("p", { class: "help section-help", text: "Tokens this user issued for clients such as npm and Docker. Revoke one when a client should no longer sign in with it." }),
      holder
    )
  );
  async function loadTokens() {
    replace(holder, loading("Loading tokens"));
    try {
      const rows = await api.tokens.list(user.userId);
      if (!rows.length) {
        replace(holder, h("div", { class: "table-wrap" }, emptyState({ title: "No token", text: "This user has issued no token." })));
        return;
      }
      replace(holder, tokenTable(rows, { onRevoke: loadTokens }).el);
    } catch (error) {
      replace(holder, problem(error, "The tokens could not be loaded"));
    }
  }
  await loadTokens();
}

