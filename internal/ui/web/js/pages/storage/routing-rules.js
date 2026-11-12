// S16 Routing rules, with the count of repositories that use each rule.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { plural } from "../../format.js";
import { pageHead, linkButton, failure } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { mark } from "../../ui/notice.js";

export function modeMark(mode) {
  return mode === "BLOCK" ? mark("warn", "Block") : mark("info", "Allow");
}

function matcherCell(rule) {
  const list = rule.matchers || [];
  return h(
    "div",
    { class: "matcher-cell" },
    h("div", { text: plural(list.length, "matcher") }),
    h("div", { class: "matcher-tags" }, list.slice(0, 3).map((m) => h("code", { class: "tag", text: m })), list.length > 3 ? h("span", { class: "muted small", text: `and ${list.length - 3} more` }) : null)
  );
}

export async function render(ctx) {
  ctx.main.classList.add("area-storage");
  const canEdit = ctx.can("nexus:settings:update");
  const canSeeRepos = ctx.can("nexus:repository-admin:*:*:read");
  const actions = [linkButton("Routing preview", "#/admin/routing-preview")];
  if (canEdit) actions.push(linkButton("Create routing rule", "#/admin/routing-rules/new", "primary"));
  ctx.main.append(
    pageHead({
      title: "Routing rules",
      sub: "A rule decides which paths a proxy repository may fetch from its remote. Each proxy applies one rule or none.",
      actions
    })
  );
  let rows;
  let settings = null;
  try {
    const [rules, repos] = await Promise.allSettled([api.routingRules.list(), canSeeRepos ? api.repositories.settings() : Promise.resolve(null)]);
    if (rules.status === "rejected") throw rules.reason;
    rows = rules.value || [];
    settings = repos.status === "fulfilled" ? repos.value : null;
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/routing-rules", { replace: true }) });
    return;
  }

  const users = {};
  if (settings) {
    for (const repo of settings) {
      if (repo.routingRuleName) (users[repo.routingRuleName] = users[repo.routingRuleName] || []).push(repo.name);
    }
  } else {
    ctx.main.append(h("p", { class: "muted store-summary", text: "Your roles do not show which repositories use each rule." }));
  }

  const columns = [
    { key: "name", label: "Name" },
    { key: "mode", label: "Mode", value: (r) => (r.mode === "BLOCK" ? "Block" : "Allow"), render: (r) => modeMark(r.mode) },
    { key: "matchers", label: "Matchers", sort: false, value: (r) => (r.matchers || []).join(" "), render: matcherCell },
    { key: "description", label: "Description", class: "break" }
  ];
  if (settings) {
    columns.push({
      key: "usedBy",
      label: "Used by",
      class: "nowrap",
      value: (r) => (users[r.name] || []).length,
      render: (r) => (users[r.name] ? h("span", { title: users[r.name].join(", "), text: plural(users[r.name].length, "repository", "repositories") }) : null)
    });
  }
  const table = dataTable(columns, {
    rows,
    sort: { key: "name", dir: "asc" },
    rowHref: (r) => `#/admin/routing-rules/${encodeURIComponent(r.name)}`,
    caption: "Routing rules",
    filterLabel: "Filter the routing rules",
    empty: {
      title: "No routing rule yet",
      text: "Without a rule a proxy may fetch every path from its remote.",
      actions: canEdit ? [linkButton("Create routing rule", "#/admin/routing-rules/new", "primary")] : []
    }
  });
  ctx.main.append(table.el);
}
