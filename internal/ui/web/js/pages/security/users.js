// S23 Users: one source at a time, filtered by the server on the user id.

import { h, replace } from "../../dom.js";
import { api } from "../../api.js";
import { plural } from "../../format.js";
import { pageHead, linkButton, failure, loading } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { notice, errorText, problem } from "../../ui/notice.js";
import { statusMark, tags, tableOrEmpty, STATUS } from "./common.js";

function name(u) {
  return [u.firstName, u.lastName].filter(Boolean).join(" ");
}

export async function render(ctx) {
  const canCreate = ctx.can("nexus:users:create");
  const create = () => (canCreate ? [linkButton("Create user", "#/admin/users/new", "primary")] : []);
  ctx.main.append(
    pageHead({
      title: "Users",
      sub: "Local users sign in with a password kept by this server. Directory users sign in through an LDAP connection.",
      actions: create()
    })
  );
  let sources;
  try {
    sources = await api.userSources();
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/users", { replace: true }) });
    return;
  }
  let source = sources.some((s) => s.id === ctx.query.source) ? ctx.query.source : sources.some((s) => s.id === "default") ? "default" : (sources[0] || {}).id;
  let text = ctx.query.userId || "";
  const sourceName = (id) => (sources.find((s) => s.id === id) || { name: id }).name;

  const sourceSelect = h(
    "select",
    { class: "select", id: "user-source", name: "source" },
    sources.map((s) => h("option", { value: s.id, text: s.name, selected: s.id === source }))
  );
  const filterInput = h("input", { class: "input", id: "user-filter", name: "filter", type: "search", value: text, autocomplete: "off", spellcheck: "false" });
  const searchForm = h(
    "form",
    { class: "user-search", role: "search", "aria-label": "Users" },
    h("div", { class: "field" }, h("label", { class: "label", for: "user-source", text: "Source" }), sourceSelect),
    h("div", { class: "field" }, h("label", { class: "label", for: "user-filter", text: "User id contains" }), filterInput),
    h("button", { type: "submit", class: "btn", text: "Search" })
  );
  const results = h("div");
  const status = h("p", { class: "visually-hidden", "aria-live": "polite" });
  ctx.main.append(searchForm, results, status);

  const columns = () => {
    const list = [
      { key: "userId", label: "User id", class: "break" },
      { key: "name", label: "Name", value: name },
      { key: "emailAddress", label: "Mail", class: "break" },
      { key: "status", label: "State", value: (u) => (STATUS[u.status] || { label: u.status }).label, render: (u) => statusMark(u.status) },
      { key: "roles", label: "Roles", sort: false, value: (u) => (u.roles || []).join(" "), render: (u) => tags(u.roles) }
    ];
    if (source !== "default") list.push({ key: "externalRoles", label: "Directory groups", sort: false, value: (u) => (u.externalRoles || []).join(" "), render: (u) => tags(u.externalRoles) });
    return list;
  };

  let seq = 0;
  async function load() {
    const run = ++seq;
    replace(results, loading("Loading users"));
    let rows;
    try {
      rows = await api.users.list(source, text || undefined);
    } catch (error) {
      if (run === seq) replace(results, problem(error, "The users could not be loaded"));
      return;
    }
    if (run !== seq) return;
    const empty = text
      ? { title: "No user found", text: `No user of ${sourceName(source)} has an id that contains "${text}".` }
      : { title: "No user yet", text: source === "default" ? "Create a local user, or connect a directory." : `${sourceName(source)} lists no user.`, actions: source === "default" ? create() : [] };
    const table = tableOrEmpty(
      rows,
      () =>
        dataTable(columns(), {
          rows,
          filter: false,
          sort: { key: "userId", dir: "asc" },
          rowHref: (u) => `#/admin/users/${encodeURIComponent(u.source)}/${encodeURIComponent(u.userId)}`,
          caption: `Users of ${sourceName(source)}`,
          empty
        }),
      empty
    );
    replace(results, table.el);
    status.textContent = `${plural(rows.length, "user")} found.`;
  }

  const search = () => {
    source = sourceSelect.value;
    text = filterInput.value.trim();
    const q = new URLSearchParams();
    if (source !== "default") q.set("source", source);
    if (text) q.set("userId", text);
    const qs = q.toString();
    history.replaceState(null, "", `#/admin/users${qs ? `?${qs}` : ""}`);
    load();
  };
  searchForm.addEventListener("submit", (event) => {
    event.preventDefault();
    search();
  });
  sourceSelect.addEventListener("change", search);
  await load();
}
