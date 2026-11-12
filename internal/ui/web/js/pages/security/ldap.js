// S26 LDAP connections: the list in the order the server asks them, a dialog to change the order,
// and a button that forgets cached directory sign ins.

import { h, replace } from "../../dom.js";
import { api } from "../../api.js";
import { pageHead, linkButton, failure, button, busy, loading } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { notice, toast, errorText, mark, problem, toastProblem } from "../../ui/notice.js";
import { openDialog } from "../../ui/dialog.js";
import { tableOrEmpty } from "./common.js";

export const SCHEMES = {
  NONE: "No account",
  SIMPLE: "Simple",
  DIGEST_MD5: "DIGEST-MD5",
  CRAM_MD5: "CRAM-MD5"
};

export function address(c) {
  return `${c.protocol}://${c.host}:${c.port}`;
}

function orderDialog(rows) {
  let names = rows.map((r) => r.name);
  const errors = h("div");
  const list = h("ol", { class: "order-list", "aria-label": "Connections in order" });
  const live = h("p", { class: "visually-hidden", "aria-live": "polite" });
  const draw = (focus) => {
    replace(
      list,
      names.map((name, i) => {
        const up = button("Up", { small: true, disabled: i === 0, attrs: { "aria-label": `Move ${name} up` }, onClick: () => move(i, -1) });
        const down = button("Down", { small: true, disabled: i === names.length - 1, attrs: { "aria-label": `Move ${name} down` }, onClick: () => move(i, 1) });
        return h(
          "li",
          { dataset: { name } },
          h("span", { class: "order-pos", "aria-hidden": "true", text: String(i + 1) }),
          h("span", { class: "order-name break", text: name }),
          h("span", { class: "order-moves" }, up, down)
        );
      })
    );
    if (focus) {
      const item = [...list.children].find((li) => li.dataset.name === focus.name);
      const buttons = item ? item.querySelectorAll("button") : [];
      const target = [...buttons].find((b) => b.textContent === focus.dir && !b.disabled) || [...buttons].find((b) => !b.disabled);
      if (target) target.focus();
    }
  };
  function move(i, step) {
    const j = i + step;
    if (j < 0 || j >= names.length) return;
    const name = names[i];
    [names[i], names[j]] = [names[j], names[i]];
    draw({ name, dir: step < 0 ? "Up" : "Down" });
    live.textContent = `${name} is now number ${j + 1}.`;
  }
  draw();
  return openDialog({
    title: "Order of connections",
    body: h("div", { class: "stack" }, h("p", { class: "muted", text: "A directory user who signs in is looked up in the connections from the top. The first connection that knows the user decides." }), errors, list, live),
    actions: [
      { label: "Cancel", value: "cancel", kind: "" },
      {
        label: "Save order",
        value: "save",
        kind: "primary",
        handler: async () => {
          errors.replaceChildren();
          try {
            await api.ldap.changeOrder(names);
            return true;
          } catch (error) {
            errors.append(problem(error, "Not saved"));
            return false;
          }
        }
      }
    ]
  });
}

export async function render(ctx) {
  const canCreate = ctx.can("nexus:ldap:create");
  const canUpdate = ctx.can("nexus:ldap:update");
  const holder = h("div");
  const orderButton = button("Change order", {
    onClick: async () => {
      if ((await orderDialog(rows)) === "save") {
        toast("The order of the connections is saved.");
        await load();
      }
    }
  });
  const cacheButton = button("Clear cache", {
    onClick: async (event) => {
      try {
        await busy(event.currentTarget, () => api.ldap.clearCache());
        toast("Cached directory sign ins are cleared. The next sign in asks the directory again.");
      } catch (error) {
        toastProblem(error);
      }
    }
  });
  const actions = [];
  if (canCreate) actions.push(linkButton("Create connection", "#/admin/ldap/new", "primary"));
  if (canUpdate) actions.push(orderButton, cacheButton);
  ctx.main.append(
    pageHead({
      title: "LDAP",
      sub: "Directory servers users sign in with. The server keeps a sign in for a while, so a change in the directory shows after the cache is cleared.",
      actions
    }),
    holder
  );

  let rows = [];
  async function load() {
    replace(holder, loading("Loading connections"));
    try {
      rows = await api.ldap.list();
    } catch (error) {
      failure(holder, error, { retry: load });
      return false;
    }
    orderButton.hidden = rows.length < 2;
    const empty = {
      title: "No connection yet",
      text: "Without a connection only local users can sign in.",
      actions: canCreate ? [linkButton("Create connection", "#/admin/ldap/new", "primary")] : []
    };
    const table = tableOrEmpty(rows, () => dataTable(
      [
        { key: "name", label: "Name", class: "break" },
        { key: "order", label: "Order", num: true },
        { key: "address", label: "Address", value: address, render: (c) => h("span", { class: "mono small break", text: address(c) }) },
        { key: "searchBase", label: "Search base", render: (c) => h("span", { class: "mono small break", text: c.searchBase }) },
        { key: "authScheme", label: "Bind", value: (c) => SCHEMES[c.authScheme] || c.authScheme },
        {
          key: "groups",
          label: "Groups as roles",
          value: (c) => (c.ldapGroupsAsRoles ? c.groupType : ""),
          render: (c) => (c.ldapGroupsAsRoles ? mark("ok", c.groupType === "dynamic" ? "Dynamic" : "Static") : mark("off", "Not mapped"))
        }
      ],
      {
        rows,
        filter: false,
        sort: { key: "order", dir: "asc" },
        rowHref: (c) => `#/admin/ldap/${encodeURIComponent(c.name)}`,
        caption: "LDAP connections",
        empty
      }
    ), empty);
    replace(holder, table.el);
    return true;
  }
  if (!(await load())) {
    orderButton.hidden = true;
  }
}
