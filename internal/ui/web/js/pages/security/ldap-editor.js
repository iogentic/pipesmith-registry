// S27 LDAP editor: two tabs, Connection and Users and groups. Verify the connection, the user
// mapping and a sign in with the values of the form. The server never sends the bind password back,
// so a save asks for it when the field is empty (plan decision P9, interface decision U27).

import { h, replace } from "../../dom.js";
import { api } from "../../api.js";
import { pageHead, failure, button, busy, notFound } from "../../ui/page.js";
import { form } from "../../ui/form.js";
import { dataTable } from "../../ui/table.js";
import { notice, toast, errorText, problem, toastProblem } from "../../ui/notice.js";
import { confirm, openDialog } from "../../ui/dialog.js";
import { isPreviewError } from "../../preview.js";
import { ID_PATTERN, tags } from "./common.js";
import { SCHEMES, address } from "./ldap.js";

const DEFAULTS = {
  name: "",
  protocol: "ldap",
  useTrustStore: false,
  host: "",
  port: 389,
  searchBase: "",
  authScheme: "SIMPLE",
  authRealm: "",
  authUsername: "",
  authPassword: "",
  connectionTimeoutSeconds: 30,
  connectionRetryDelaySeconds: 300,
  maxIncidentsCount: 3,
  userBaseDn: "",
  userSubtree: false,
  userObjectClass: "inetOrgPerson",
  userLdapFilter: "",
  userIdAttribute: "uid",
  userRealNameAttribute: "cn",
  userEmailAddressAttribute: "mail",
  userPasswordAttribute: "",
  ldapGroupsAsRoles: false,
  groupType: "static",
  groupBaseDn: "",
  groupSubtree: false,
  groupObjectClass: "groupOfUniqueNames",
  groupIdAttribute: "cn",
  groupMemberAttribute: "uniqueMember",
  groupMemberFormat: "${dn}",
  userMemberOfAttribute: "memberOf"
};

const binds = (v) => v.authScheme && v.authScheme !== "NONE";
const staticGroups = (v) => v.ldapGroupsAsRoles && v.groupType !== "dynamic";

function connectionSections() {
  return [
    {
      title: "Server",
      grid: true,
      fields: [
        { name: "name", label: "Name", type: "text", required: true, mono: true, pattern: ID_PATTERN, autocomplete: "off", help: "Shown in lists and used in the address of this page." },
        {
          name: "protocol",
          label: "Protocol",
          type: "select",
          required: true,
          noBlank: true,
          options: [
            { value: "ldap", label: "ldap, usually port 389" },
            { value: "ldaps", label: "ldaps with TLS, usually port 636" }
          ]
        },
        { name: "host", label: "Host", type: "text", required: true, mono: true, autocomplete: "off", placeholder: "ldap.example.com" },
        { name: "port", label: "Port", type: "number", required: true, min: 1, max: 65535 },
        { name: "searchBase", label: "Search base", type: "text", required: true, mono: true, wide: true, placeholder: "dc=example,dc=com", help: "The base below which users and groups are looked up." }
      ]
    },
    {
      title: "Bind",
      help: "How the server signs in to the directory to look up users.",
      grid: true,
      fields: [
        {
          name: "authScheme",
          label: "Method",
          type: "select",
          required: true,
          noBlank: true,
          options: [
            { value: "NONE", label: "No account, an anonymous bind" },
            { value: "SIMPLE", label: "Simple: account and password" },
            { value: "DIGEST_MD5", label: "DIGEST-MD5" },
            { value: "CRAM_MD5", label: "CRAM-MD5" }
          ]
        },
        { name: "authRealm", label: "SASL realm", type: "text", autocomplete: "off", show: (v) => v.authScheme === "DIGEST_MD5" || v.authScheme === "CRAM_MD5" },
        { name: "authUsername", label: "Account", type: "text", required: true, mono: true, wide: true, autocomplete: "off", placeholder: "uid=registry,ou=services,dc=example,dc=com", show: binds },
        {
          name: "authPassword",
          label: "Bind password",
          type: "password",
          wide: true,
          autocomplete: "new-password",
          help: "The server never sends the stored password back. Enter it to verify the connection; a save asks for it when this field is empty.",
          show: binds
        }
      ]
    },
    {
      title: "Timeouts",
      grid: true,
      fields: [
        { name: "connectionTimeoutSeconds", label: "Connection timeout (seconds)", type: "number", required: true, min: 1, max: 3600 },
        { name: "connectionRetryDelaySeconds", label: "Wait before a retry (seconds)", type: "number", required: true, min: 0, max: 86400 },
        { name: "maxIncidentsCount", label: "Failures before the connection is skipped", type: "number", required: true, min: 0, max: 100 }
      ]
    }
  ];
}

function userSections() {
  return [
    {
      title: "Users",
      grid: true,
      fields: [
        { name: "userBaseDn", label: "User base", type: "text", mono: true, placeholder: "ou=people", help: "Relative to the search base. Empty means the search base itself." },
        { name: "userObjectClass", label: "Object class", type: "text", mono: true, autocomplete: "off" },
        { name: "userSubtree", label: "Search below the user base", type: "checkbox", wide: true },
        { name: "userLdapFilter", label: "Extra filter", type: "text", mono: true, wide: true, placeholder: "(!(employeeType=external))", help: "Only users that match it can sign in." },
        { name: "userIdAttribute", label: "User id attribute", type: "text", mono: true, autocomplete: "off" },
        { name: "userRealNameAttribute", label: "Name attribute", type: "text", mono: true, autocomplete: "off" },
        { name: "userEmailAddressAttribute", label: "Mail attribute", type: "text", mono: true, autocomplete: "off" },
        { name: "userPasswordAttribute", label: "Password attribute", type: "text", mono: true, autocomplete: "off", help: "Leave it empty to check a password with a bind as the user." }
      ]
    },
    {
      title: "Groups",
      grid: true,
      fields: [
        { name: "ldapGroupsAsRoles", label: "Map directory groups to roles", type: "checkbox", wide: true, help: "A group whose name is the id of a role gives that role to its members." },
        {
          name: "groupType",
          label: "Group type",
          type: "radio",
          wide: true,
          options: [
            { value: "static", label: "Static: each group lists its members" },
            { value: "dynamic", label: "Dynamic: each user lists its groups" }
          ],
          show: (v) => v.ldapGroupsAsRoles
        },
        { name: "groupBaseDn", label: "Group base", type: "text", mono: true, placeholder: "ou=groups", show: staticGroups },
        { name: "groupObjectClass", label: "Group object class", type: "text", mono: true, show: staticGroups },
        { name: "groupSubtree", label: "Search below the group base", type: "checkbox", wide: true, show: staticGroups },
        { name: "groupIdAttribute", label: "Group id attribute", type: "text", mono: true, show: staticGroups },
        { name: "groupMemberAttribute", label: "Member attribute", type: "text", mono: true, show: staticGroups },
        {
          name: "groupMemberFormat",
          label: "Member format",
          type: "text",
          mono: true,
          wide: true,
          help: "How a member is written in the group. ${username} stands for the user id, ${dn} for the whole entry of the user.",
          show: staticGroups
        },
        { name: "userMemberOfAttribute", label: "Groups attribute of a user", type: "text", mono: true, required: true, placeholder: "memberOf", show: (v) => v.ldapGroupsAsRoles && v.groupType === "dynamic" }
      ]
    }
  ];
}

const names = (sections) => new Set(sections.flatMap((s) => s.fields.map((f) => f.name)));

function result(ok, title, message) {
  return notice(ok ? "success" : "error", title, message);
}

// The bind password for one save. Resolves to the password, or null when the user cancels.
function askPassword(account) {
  const input = h("input", { class: "input", id: "bind-password", name: "bindPassword", type: "password", autocomplete: "new-password", "aria-describedby": "bind-password-help bind-password-error" });
  const error = h("div", { class: "error-text", id: "bind-password-error" });
  let value = null;
  return openDialog({
    title: "Bind password",
    body: h(
      "div",
      { class: "stack" },
      h("p", { text: `The server does not send the stored bind password back, so each save needs it. Enter the password of ${account}.` }),
      h("div", { class: "field" }, h("label", { class: "label", for: "bind-password", text: "Bind password" }), input, h("div", { class: "help", id: "bind-password-help", text: "It is sent with this save and not kept in the browser." }), error)
    ),
    actions: [
      { label: "Cancel", value: "cancel", kind: "" },
      {
        label: "Save connection",
        value: "save",
        kind: "primary",
        handler: () => {
          if (!input.value) {
            error.textContent = "Enter the bind password.";
            input.setAttribute("aria-invalid", "true");
            input.focus();
            return false;
          }
          value = input.value;
          return true;
        }
      }
    ],
    onOpen: () => input.focus()
  }).then((choice) => (choice === "save" ? value : null));
}

export async function render(ctx) {
  const creating = !ctx.params.name;
  const name = ctx.params.name;
  let record;
  try {
    record = creating ? { ...DEFAULTS } : await api.ldap.get(name);
  } catch (error) {
    failure(ctx.main, error, { what: "connection", retry: () => location.reload() });
    return;
  }
  if (!record) {
    ctx.main.append(notFound("connection"));
    return;
  }
  const canEdit = creating || ctx.can("nexus:ldap:update");
  const canDelete = !creating && ctx.can("nexus:ldap:delete");
  // A verification changes nothing, so it stays while the server takes no changes.
  const canVerify = creating || ctx.allowed("nexus:ldap:update");
  const title = creating ? "New LDAP connection" : record.name;
  ctx.setTitle(title);

  const actions = [];
  if (canDelete) {
    actions.push(
      button("Delete", {
        kind: "danger",
        onClick: async () => {
          const ok = await confirm({
            title: `Delete ${record.name}?`,
            text: "Users of this directory can no longer sign in through it. Their roles granted here stay with the user ids.",
            confirmLabel: "Delete connection",
            danger: true
          });
          if (!ok) return;
          try {
            await api.ldap.remove(record.name);
            ctx.setDirty(null);
            toast(`The connection ${record.name} is deleted.`);
            ctx.navigate("/admin/ldap");
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
      sub: creating ? "A directory server users sign in with." : `${address(record)}, bind ${(SCHEMES[record.authScheme] || record.authScheme).toLowerCase()}`,
      crumbs: [{ label: "LDAP", href: "#/admin/ldap" }],
      actions
    })
  );

  const base = { ...DEFAULTS, ...record, authPassword: "" };
  const connSections = connectionSections();
  const userSecs = userSections();
  const connNames = names(connSections);
  const userNames = names(userSecs);
  const fConn = form({ sections: connSections, values: base, readOnly: !canEdit });
  const fUsers = form({ sections: userSecs, values: base, readOnly: !canEdit });
  ctx.setDirty(() => fConn.isDirty() || fUsers.isDirty());

  // Tabs
  const panels = {};
  const tabButtons = {};
  const tabs = [
    { id: "connection", label: "Connection" },
    { id: "users", label: "Users and groups" }
  ];
  const tablist = h("div", { class: "tabs", role: "tablist", "aria-label": "Parts of the connection" });
  function showTab(id, focus = false) {
    for (const t of tabs) {
      const selected = t.id === id;
      tabButtons[t.id].setAttribute("aria-selected", String(selected));
      tabButtons[t.id].tabIndex = selected ? 0 : -1;
      panels[t.id].hidden = !selected;
    }
    if (focus) tabButtons[id].focus();
  }
  for (const t of tabs) {
    const b = h("button", { type: "button", class: "tab", role: "tab", id: `ldap-tab-${t.id}`, "aria-controls": `ldap-panel-${t.id}`, text: t.label });
    b.addEventListener("click", () => showTab(t.id));
    tabButtons[t.id] = b;
    tablist.append(b);
  }
  tablist.addEventListener("keydown", (event) => {
    const order = tabs.map((t) => t.id);
    const current = order.findIndex((id) => tabButtons[id] === document.activeElement);
    if (current < 0) return;
    let next = null;
    if (event.key === "ArrowRight") next = order[(current + 1) % order.length];
    if (event.key === "ArrowLeft") next = order[(current - 1 + order.length) % order.length];
    if (event.key === "Home") next = order[0];
    if (event.key === "End") next = order[order.length - 1];
    if (next) {
      event.preventDefault();
      showTab(next, true);
    }
  });

  const connResult = h("div", { class: "verify-result", "aria-live": "polite" });
  const mappingResult = h("div", { class: "verify-result mapping-result", "aria-live": "polite" });
  const verifyConn = button("Verify connection", { onClick: (event) => verifyConnection(event.currentTarget) });
  const verifyMapping = button("Verify user mapping", { onClick: (event) => verifyUserMapping(event.currentTarget) });
  const verifyLogin = button("Verify sign in", { onClick: () => verifySignIn() });

  panels.connection = h(
    "div",
    { role: "tabpanel", id: "ldap-panel-connection", "aria-labelledby": "ldap-tab-connection", class: "tab-panel" },
    fConn.el,
    canVerify ? h("div", { class: "verify-row" }, verifyConn) : null,
    connResult
  );
  panels.users = h(
    "div",
    { role: "tabpanel", id: "ldap-panel-users", "aria-labelledby": "ldap-tab-users", class: "tab-panel" },
    fUsers.el,
    canVerify ? h("div", { class: "verify-row" }, verifyMapping, verifyLogin) : null,
    mappingResult
  );

  const messages = h("div", { class: "stack page-messages" });
  if (!canEdit) messages.append(notice("info", "Read only", ctx.readOnlyText("Your roles let you read this connection, not change it.")));
  const save = h("button", { type: "submit", class: "btn primary", text: creating ? "Create connection" : "Save" });
  const discard = h("button", { type: "button", class: "btn", text: creating ? "Cancel" : "Discard changes" });
  const formEl = h(
    "form",
    { novalidate: true, class: "panel" },
    tablist,
    panels.connection,
    panels.users,
    canEdit ? h("div", { class: "form-actions" }, save, discard) : null
  );
  ctx.main.append(messages, formEl);
  showTab("connection");

  // The connection object of the contract, from both tabs. Each form gives only its own fields;
  // a hidden field keeps its last value.
  function body() {
    const v = { ...base };
    const conn = fConn.values();
    const users = fUsers.values();
    for (const key of connNames) if (key in conn) v[key] = conn[key];
    for (const key of userNames) if (key in users) v[key] = users[key];
    delete v.order;
    if (creating) delete v.id;
    if (!binds(v)) {
      v.authUsername = "";
      v.authPassword = "";
      v.authRealm = "";
    }
    return v;
  }

  // Server errors beside their field, on the tab that holds the field.
  function showErrors(error) {
    if (!error || !error.fieldErrors || !error.fieldErrors.length) return false;
    const conn = [];
    const users = [];
    for (const e of error.fieldErrors) {
      const id = e.id.replace(/^connection\./, "");
      if (userNames.has(id)) users.push({ id, message: e.message });
      else conn.push({ id: connNames.has(id) ? id : "*", message: e.message });
    }
    fConn.clearErrors();
    fUsers.clearErrors();
    showTab(conn.length ? "connection" : "users");
    if (users.length) fUsers.showServerErrors({ fieldErrors: users });
    if (conn.length) fConn.showServerErrors({ fieldErrors: conn });
    return true;
  }

  function validConnection() {
    if (fConn.validate()) return true;
    showTab("connection");
    fConn.validate();
    return false;
  }

  function validAll() {
    if (!validConnection()) return false;
    if (fUsers.validate()) return true;
    showTab("users");
    fUsers.validate();
    return false;
  }

  async function verifyConnection(el) {
    connResult.replaceChildren();
    if (!validConnection()) return;
    try {
      const answer = await busy(el, () => api.ldap.verifyConnection(body()));
      connResult.replaceChildren(result(answer.ok, answer.ok ? "Connected" : "Not connected", answer.message));
    } catch (error) {
      if (!showErrors(error)) connResult.replaceChildren(isPreviewError(error) ? problem(error) : result(false, "Not verified", errorText(error)));
    }
  }

  async function verifyUserMapping(el) {
    mappingResult.replaceChildren();
    if (!validAll()) return;
    let answer;
    try {
      answer = await busy(el, () => api.ldap.verifyUserMapping(body()));
    } catch (error) {
      if (!showErrors(error)) mappingResult.replaceChildren(isPreviewError(error) ? problem(error) : result(false, "Not verified", errorText(error)));
      return;
    }
    const parts = [result(answer.ok, answer.ok ? "Mapping works" : "No user found", answer.message)];
    if (answer.users && answer.users.length) {
      parts.push(
        dataTable(
          [
            { key: "userId", label: "User id", class: "break" },
            { key: "name", label: "Name" },
            { key: "email", label: "Mail", class: "break" },
            { key: "groups", label: "Groups", sort: false, value: (u) => (u.groups || []).join(" "), render: (u) => tags(u.groups) }
          ],
          { rows: answer.users, filter: false, sort: { key: "userId", dir: "asc" }, caption: "Users the mapping finds" }
        ).el
      );
    }
    mappingResult.replaceChildren(...parts);
  }

  async function verifySignIn() {
    if (!validAll()) return;
    const f = form({
      fields: [
        { name: "username", label: "User name", type: "text", required: true, autocomplete: "off" },
        { name: "password", label: "Password", type: "password", required: true, autocomplete: "off" }
      ]
    });
    const out = h("div", { class: "verify-result", "aria-live": "polite" });
    await openDialog({
      title: "Verify sign in",
      body: h("div", { class: "stack" }, h("p", { class: "muted", text: "Signs in one directory user with the values of the form. Nothing is saved." }), f.el, out),
      actions: [
        { label: "Close", value: "close", kind: "" },
        {
          label: "Verify",
          value: "verify",
          kind: "primary",
          handler: async () => {
            out.replaceChildren();
            if (!f.validate()) return false;
            const v = f.values();
            try {
              const answer = await api.ldap.verifyLogin(body(), v.username.trim(), v.password);
              out.replaceChildren(result(answer.ok, answer.ok ? "Signed in" : "Not signed in", answer.message));
            } catch (error) {
              const own = error && error.fieldErrors ? error.fieldErrors.filter((e) => e.id === "username" || e.id === "password") : [];
              if (own.length) f.showServerErrors({ fieldErrors: own });
              else out.replaceChildren(isPreviewError(error) ? problem(error) : result(false, "Not verified", errorText(error)));
            }
            return false;
          }
        }
      ],
      onOpen: () => f.focusFirst()
    });
  }

  discard.addEventListener("click", () => {
    if (creating) {
      ctx.setDirty(null);
      ctx.navigate("/admin/ldap");
      return;
    }
    fConn.set(base);
    fUsers.set(base);
    fConn.clearErrors();
    fUsers.clearErrors();
    connResult.replaceChildren();
    mappingResult.replaceChildren();
  });

  formEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    messages.replaceChildren();
    if (!validAll()) return;
    const b = body();
    if (binds(b) && !b.authPassword) {
      const password = await askPassword(b.authUsername || "the bind account");
      if (password === null) return;
      b.authPassword = password;
    }
    try {
      await busy(save, () => (creating ? api.ldap.create(b) : api.ldap.update(record.name, b)));
    } catch (error) {
      if (!showErrors(error)) messages.append(problem(error, creating ? "Not created" : "Not saved"));
      return;
    }
    fConn.markClean();
    fUsers.markClean();
    ctx.setDirty(null);
    toast(creating ? `The connection ${b.name} is created.` : `The connection ${b.name} is saved.`);
    if (creating || b.name !== record.name) {
      ctx.navigate(`/admin/ldap/${encodeURIComponent(b.name)}`, { replace: !creating });
      return;
    }
    try {
      record = await api.ldap.get(b.name);
    } catch (error) {
      record = { ...b };
    }
    Object.assign(base, DEFAULTS, record, { authPassword: "" });
    fConn.set(base);
    fUsers.set(base);
    ctx.setDirty(() => fConn.isDirty() || fUsers.isDirty());
  });
}
