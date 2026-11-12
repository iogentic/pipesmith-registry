// Parts the security screens share: the realm ids the server keeps as protocol values, labels of
// privilege types, user states and token kinds, the permission string of a privilege, the token table.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { time } from "../../format.js";
import { dataTable } from "../../ui/table.js";
import { emptyState } from "../../ui/page.js";
import { confirm } from "../../ui/dialog.js";
import { mark, toast, errorText, toastProblem } from "../../ui/notice.js";

// Protocol values of GET realms/active. The interface shows the names of GET realms/available.
export const REALM = {
  local: "NexusAuthenticatingRealm",
  ldap: "LdapRealm",
  npm: "NpmToken",
  docker: "DockerToken",
  user: "User-Token-Realm"
};

export const TOKEN_KINDS = [
  { kind: "npm", label: "npm", realm: REALM.npm, use: "Put it into .npmrc as the _authToken of the registry address." },
  { kind: "docker", label: "Docker", realm: REALM.docker, use: "Use it as the password of docker login, with your user name." },
  { kind: "user", label: "User token", realm: REALM.user, use: "Use it in place of your password in the settings of Maven, Gradle or another client." }
];

export function kindOf(kind) {
  return TOKEN_KINDS.find((k) => k.kind === kind) || { kind, label: kind || "Unknown", realm: null, use: "" };
}

export const ID_PATTERN = { regex: /^[A-Za-z0-9][A-Za-z0-9._-]*$/, message: "Use letters, digits, dots, hyphens and underscores, starting with a letter or a digit." };

const TYPES = {
  application: "Application",
  "repository-view": "Repository content",
  "repository-admin": "Repository settings",
  "repository-content-selector": "Content selector",
  wildcard: "Wildcard",
  script: "Script"
};

export function privilegeType(type) {
  return TYPES[type] || type || "";
}

function actionList(actions) {
  const list = actions && actions.length ? actions : ["*"];
  return list.map((a) => (String(a).toUpperCase() === "ALL" ? "*" : String(a).toLowerCase())).join(",");
}

// The permission string of a privilege, by the rules of docs/ui/API.md, section S19.
export function permissionOf(p) {
  if (!p) return "";
  const actions = actionList(p.actions);
  switch (p.type) {
    case "wildcard":
      return p.pattern || "";
    case "application":
      return `nexus:${p.domain || "*"}:${actions}`;
    case "repository-view":
    case "repository-admin":
      return `nexus:${p.type}:${p.format || "*"}:${p.repository || "*"}:${actions}`;
    case "repository-content-selector":
      return `nexus:repository-content-selector:${p.contentSelector || "*"}:${p.format || "*"}:${p.repository || "*"}:${actions}`;
    case "script":
      return `nexus:script:${p.scriptName || "*"}:${actions}`;
    default:
      return "";
  }
}

export const STATUS = {
  active: { label: "Active", kind: "ok" },
  locked: { label: "Locked", kind: "warn" },
  disabled: { label: "Disabled", kind: "off" },
  changepassword: { label: "Must change password", kind: "info" }
};

export function statusMark(status) {
  const s = STATUS[status] || { label: status || "Unknown", kind: "" };
  return mark(s.kind, s.label);
}

export function tags(list) {
  if (!list || !list.length) return null;
  return h("span", { class: "tags" }, list.map((item) => h("span", { class: "tag", text: item })));
}

export function roleLabel(role) {
  return role.name && role.name !== role.id ? `${role.name} (${role.id})` : role.id;
}

// A copy of an ApiError whose field ids are renamed, for forms whose field names differ from the body.
export function renamed(error, map) {
  if (!error || !error.fieldErrors) return error;
  return { fieldErrors: error.fieldErrors.map((e) => ({ id: map[e.id] || e.id, message: e.message })) };
}

// A table, or only its empty state when there is no row, so that no sort control stands above
// an empty list on a narrow screen.
export function tableOrEmpty(rows, build, empty) {
  if (rows && rows.length) return build();
  return { el: h("div", { class: "table-wrap" }, emptyState(empty)), setRows: () => {}, rows: () => [] };
}

// Tokens of one user. onRevoke(token) runs after a revoke went through.
export function tokenTable(rows, { onRevoke, active = null, empty } = {}) {
  if (!rows.length) return tableOrEmpty(rows, null, empty || { title: "No token", text: "No token was issued yet." });
  const accepted = (t) => !active || !kindOf(t.kind).realm || active.includes(kindOf(t.kind).realm);
  const columns = [
    { key: "name", label: "Name", class: "break" },
    {
      key: "kind",
      label: "Kind",
      value: (t) => kindOf(t.kind).label,
      render: (t) => (accepted(t) ? kindOf(t.kind).label : h("span", { class: "kind-cell" }, kindOf(t.kind).label, mark("off", "Realm not active")))
    },
    { key: "publicPart", label: "Public part", render: (t) => h("span", { class: "mono small break", text: t.publicPart || "" }) },
    { key: "created", label: "Issued", render: (t) => time(t.created) },
    { key: "lastUsed", label: "Last used", render: (t) => (t.lastUsed ? time(t.lastUsed) : h("span", { class: "muted", text: "Never" })) }
  ];
  if (onRevoke) {
    columns.push({
      key: "revoke",
      label: "Actions",
      sort: false,
      render: (t) => {
        const button = h("button", { type: "button", class: "btn small", text: "Revoke", "aria-label": `Revoke ${t.name}` });
        button.addEventListener("click", async () => {
          const ok = await confirm({
            title: `Revoke ${t.name}?`,
            text: "Clients that sign in with this token are refused from now on. A revoked token cannot come back; issue a new one instead.",
            confirmLabel: "Revoke token",
            danger: true
          });
          if (!ok) return;
          try {
            await api.tokens.remove(t.id);
            toast(`The token ${t.name} is revoked.`);
            onRevoke(t);
          } catch (error) {
            toastProblem(error);
          }
        });
        return button;
      }
    });
  }
  return dataTable(columns, {
    rows,
    filter: false,
    sort: { key: "created", dir: "desc" },
    caption: "Tokens",
    empty: empty || { title: "No token", text: "No token was issued yet." }
  });
}
