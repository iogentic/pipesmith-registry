// S14 Cleanup policies.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { plural } from "../../format.js";
import { pageHead, linkButton, failure } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { criteriaText, policyFormat } from "./common.js";

export async function render(ctx) {
  ctx.main.classList.add("area-storage");
  const canEdit = ctx.can("nexus:settings:update");
  const createButton = () => (canEdit ? [linkButton("Create cleanup policy", "#/admin/cleanup-policies/new", "primary")] : []);
  ctx.main.append(
    pageHead({
      title: "Cleanup policies",
      sub: "A policy names the components the cleanup task may remove. Repositories apply policies in their settings.",
      actions: createButton()
    })
  );
  let rows;
  try {
    rows = (await api.cleanupPolicies.list()) || [];
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/cleanup-policies", { replace: true }) });
    return;
  }
  const table = dataTable(
    [
      { key: "name", label: "Name" },
      { key: "format", label: "Format", value: (p) => policyFormat(p.format) },
      { key: "criteria", label: "Criteria", value: (p) => criteriaText(p.criteria) },
      {
        key: "usedBy",
        label: "Applied by",
        value: (p) => (p.usedBy || []).length,
        render: (p) => ((p.usedBy || []).length ? h("span", { title: p.usedBy.join(", "), text: plural(p.usedBy.length, "repository", "repositories") }) : null)
      },
      { key: "notes", label: "Notes", class: "break" }
    ],
    {
      rows,
      sort: { key: "name", dir: "asc" },
      rowHref: (p) => `#/admin/cleanup-policies/${encodeURIComponent(p.name)}`,
      caption: "Cleanup policies",
      filterLabel: "Filter the cleanup policies",
      empty: {
        title: "No cleanup policy yet",
        text: "A policy removes components that nobody changed or downloaded for a number of days, when the cleanup task runs.",
        actions: createButton()
      }
    }
  );
  ctx.main.append(table.el);
}
