// S18 Routing preview: the verdict of every repository in a scope for one path. docs/ui/API.md, S18.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { formatName, typeName } from "../../format.js";
import { pageHead, busy, loading, emptyState } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { mark, notice, errorText, problem } from "../../ui/notice.js";
import { sentence } from "./common.js";

const SCOPES = [
  { value: "all", label: "All repositories" },
  { value: "proxy", label: "Proxy repositories" },
  { value: "group", label: "Group repositories" }
];

export async function render(ctx) {
  ctx.main.classList.add("area-storage");
  ctx.main.append(
    pageHead({
      title: "Routing preview",
      sub: "Shows for one path which repositories would fetch it from their remote and which rule decides.",
      crumbs: [{ label: "Routing rules", href: "#/admin/routing-rules" }]
    })
  );

  const scope = h(
    "select",
    { class: "select", id: "preview-scope" },
    SCOPES.map((s) => h("option", { value: s.value, text: s.label }))
  );
  if (SCOPES.some((s) => s.value === ctx.query.scope)) scope.value = ctx.query.scope;
  const path = h("input", {
    class: "input mono",
    id: "preview-path",
    type: "text",
    autocomplete: "off",
    spellcheck: "false",
    placeholder: "/com/example/tool/1.0/tool-1.0.jar",
    value: ctx.query.path || "",
    "aria-describedby": "preview-path-help preview-path-error",
    "aria-required": "true"
  });
  const scopeError = h("div", { class: "error-text", id: "preview-scope-error" });
  const pathError = h("div", { class: "error-text", id: "preview-path-error" });
  const run = h("button", { type: "submit", class: "btn primary", id: "preview-run", text: "Preview" });
  const result = h("div", { id: "preview-result", "aria-live": "polite" });
  const formEl = h(
    "form",
    { class: "panel storage-form", id: "preview-form", novalidate: true, "aria-label": "Path and scope" },
    h(
      "div",
      { class: "form-grid preview-grid" },
      h("div", { class: "field" }, h("label", { class: "label", for: "preview-scope", text: "Repositories" }), scope, scopeError),
      h(
        "div",
        { class: "field" },
        h("label", { class: "label", for: "preview-path" }, "Path", h("span", { class: "req", "aria-hidden": "true", text: "*" })),
        path,
        h("div", { class: "help", id: "preview-path-help", text: "The path inside a repository, as a client asks for it." }),
        pathError
      )
    ),
    h("div", { class: "form-actions" }, run)
  );
  ctx.main.append(formEl, result);

  function mark400(error) {
    for (const e of error.fieldErrors || []) {
      const target = e.id === "scope" ? [scope, scopeError] : e.id === "path" ? [path, pathError] : null;
      if (!target) continue;
      target[0].setAttribute("aria-invalid", "true");
      target[1].textContent = e.message;
    }
    return (error.fieldErrors || []).some((e) => e.id === "scope" || e.id === "path");
  }

  async function preview() {
    for (const [input, error] of [[scope, scopeError], [path, pathError]]) {
      input.removeAttribute("aria-invalid");
      error.textContent = "";
    }
    const value = path.value.trim();
    if (!value) {
      path.setAttribute("aria-invalid", "true");
      pathError.textContent = "Enter a path.";
      path.focus();
      return;
    }
    result.replaceChildren(loading("Loading the preview"));
    let answer;
    try {
      answer = await busy(run, () => api.routingRules.preview(scope.value, value));
    } catch (error) {
      result.replaceChildren();
      if (!mark400(error)) result.append(problem(error, "No preview"));
      return;
    }
    const items = answer.items || [];
    if (!items.length) {
      result.replaceChildren(h("div", { class: "table-wrap" }, emptyState({ title: "No repository in this scope", text: "Pick another scope to see a verdict." })));
      return;
    }
    const blocked = items.filter((i) => !i.allowed).length;
    const ruleHref = (rule) => `#/admin/routing-rules/${encodeURIComponent(rule)}?path=${encodeURIComponent(value)}`;
    const table = dataTable(
      [
        {
          key: "repository",
          label: "Repository",
          render: (i) =>
            ctx.can(`nexus:repository-admin:${i.format}:${i.repository}:read`)
              ? h("a", { href: `#/admin/repositories/${encodeURIComponent(i.repository)}`, class: "break", text: i.repository })
              : h("span", { class: "break", text: i.repository })
        },
        { key: "type", label: "Type", value: (i) => typeName(i.type) },
        { key: "format", label: "Format", value: (i) => formatName(i.format) },
        {
          key: "rule",
          label: "Rule",
          value: (i) => i.rule || "No rule",
          render: (i) => (i.rule ? h("a", { href: ruleHref(i.rule), class: "break", "aria-label": `${i.rule}, test the path there`, text: i.rule }) : h("span", { class: "muted", text: "No rule" }))
        },
        { key: "allowed", label: "Verdict", value: (i) => (i.allowed ? "Allowed" : "Blocked"), render: (i) => (i.allowed ? mark("ok", "Allowed") : mark("error", "Blocked")) }
      ],
      { rows: [...items].sort((a, b) => Number(a.allowed) - Number(b.allowed) || a.repository.localeCompare(b.repository)), caption: `Verdict per repository for ${value}, blocked first`, filterLabel: "Filter the repositories" }
    );
    const parts = [h("p", { id: "preview-summary", class: "preview-summary", text: `${blocked} of ${items.length} repositories ${blocked === 1 ? "blocks" : "block"} this path.` })];
    if (items.some((i) => i.type === "group")) parts.push(h("p", { class: "muted small", text: "A group has no rule of its own. Each proxy among its members applies its own rule." }));
    parts.push(table.el);
    result.replaceChildren(...parts);
  }

  formEl.addEventListener("submit", (event) => {
    event.preventDefault();
    preview();
  });
  if (path.value.trim()) await preview();
}
