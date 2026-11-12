// S15 Cleanup policy editor: format, notes, the two criteria, the repositories that apply the policy,
// and a preview of what the policy in the form would remove from one repository. docs/ui/API.md, S15.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { plural } from "../../format.js";
import { pageHead, failure, button, busy, loading, emptyState } from "../../ui/page.js";
import { form } from "../../ui/form.js";
import { notice, toast, errorText, problem } from "../../ui/notice.js";
import { confirm } from "../../ui/dialog.js";
import { dataTable } from "../../ui/table.js";
import { NAME, POLICY_FORMATS, repositoryList, sentence } from "./common.js";

const DEFAULT_DAYS = { lastBlobUpdatedDays: 30, lastDownloadedDays: 90 };

function toValues(policy) {
  const c = policy.criteria || {};
  return {
    name: policy.name || "",
    format: policy.format || "*",
    notes: policy.notes || "",
    useUpdated: c.lastBlobUpdatedDays !== null && c.lastBlobUpdatedDays !== undefined,
    useDownloaded: c.lastDownloadedDays !== null && c.lastDownloadedDays !== undefined,
    criteria: {
      lastBlobUpdatedDays: c.lastBlobUpdatedDays ?? DEFAULT_DAYS.lastBlobUpdatedDays,
      lastDownloadedDays: c.lastDownloadedDays ?? DEFAULT_DAYS.lastDownloadedDays
    }
  };
}

function toBody(values) {
  const c = values.criteria || {};
  return {
    name: values.name,
    format: values.format,
    notes: values.notes || "",
    criteria: {
      lastBlobUpdatedDays: values.useUpdated ? c.lastBlobUpdatedDays ?? null : null,
      lastDownloadedDays: values.useDownloaded ? c.lastDownloadedDays ?? null : null
    }
  };
}

function sections(creating) {
  return [
    {
      title: "Policy",
      grid: true,
      fields: [
        creating
          ? { name: "name", label: "Name", type: "text", required: true, pattern: NAME, mono: true, help: "Repositories name the policy by it. It cannot change later." }
          : { name: "name", label: "Name", type: "static", mono: true },
        { name: "format", label: "Format", type: "select", required: true, noBlank: true, options: POLICY_FORMATS, help: "Repositories of this format can apply the policy." },
        { name: "notes", label: "Notes", type: "textarea", rows: 3, wide: true }
      ]
    },
    {
      title: "Criteria",
      fields: [
        { name: "criteria", label: "Rule", type: "static", render: () => "The cleanup task removes a component when it meets every criterion that is set. Set at least one." },
        { name: "useUpdated", label: "Last changed more than a number of days ago", type: "checkbox" },
        { name: "criteria.lastBlobUpdatedDays", label: "Days since the last change", type: "number", required: true, min: 1, show: (v) => v.useUpdated },
        { name: "useDownloaded", label: "Last downloaded more than a number of days ago", type: "checkbox" },
        {
          name: "criteria.lastDownloadedDays",
          label: "Days since the last download",
          type: "number",
          required: true,
          min: 1,
          help: "A component that was never downloaded counts from the day it was stored.",
          show: (v) => v.useDownloaded
        }
      ]
    }
  ];
}

// The field errors of a preview answer that belong to the policy in the form, without the prefix.
function policyErrors(error) {
  return (error.fieldErrors || []).filter((e) => e.id.startsWith("policy.")).map((e) => ({ id: e.id.slice("policy.".length), message: e.message }));
}

export async function render(ctx) {
  ctx.main.classList.add("area-storage");
  const name = ctx.params.name;
  const creating = !name;
  const canEdit = ctx.can("nexus:settings:update");

  let policy = { name: "", format: "*", notes: "", criteria: { lastBlobUpdatedDays: null, lastDownloadedDays: null }, usedBy: [] };
  let repositories = [];
  try {
    const [one, repos] = await Promise.allSettled([creating ? Promise.resolve(null) : api.cleanupPolicies.get(name), api.repositories.list()]);
    if (one.status === "rejected") throw one.reason;
    if (one.value) policy = one.value;
    repositories = repos.status === "fulfilled" ? repos.value || [] : [];
  } catch (error) {
    failure(ctx.main, error, { what: "cleanup policy", retry: () => ctx.navigate(`/admin/cleanup-policies/${encodeURIComponent(name)}`, { replace: true }) });
    return;
  }
  const byName = Object.fromEntries(repositories.map((r) => [r.name, r]));

  const title = creating ? "New cleanup policy" : name;
  ctx.setTitle(title);
  const messages = h("div", { class: "stack page-messages" });

  const actions = [];
  if (!creating && canEdit) {
    actions.push(
      button("Delete", {
        kind: "danger",
        onClick: async () => {
          const users = policy.usedBy || [];
          const ok = await confirm({
            title: `Delete ${name}?`,
            text: users.length
              ? `${plural(users.length, "repository", "repositories")} ${users.length === 1 ? "applies" : "apply"} this policy. The server refuses the delete until their settings drop it.`
              : "The policy is removed. No repository applies it.",
            confirmLabel: "Delete policy",
            danger: true
          });
          if (!ok) return;
          messages.replaceChildren();
          try {
            await api.cleanupPolicies.remove(name);
          } catch (error) {
            messages.append(problem(error, "Not deleted"));
            messages.scrollIntoView({ block: "nearest" });
            return;
          }
          ctx.setDirty(null);
          toast(`The cleanup policy ${name} is deleted.`);
          ctx.navigate("/admin/cleanup-policies");
        }
      })
    );
  }

  ctx.main.append(
    pageHead({
      title,
      sub: creating ? "Removes components that nobody changed or downloaded for a number of days." : "Cleanup policy",
      crumbs: [{ label: "Cleanup policies", href: "#/admin/cleanup-policies" }],
      actions
    }),
    messages
  );
  if (!canEdit) messages.append(notice("info", "Read only", ctx.readOnlyText("Your roles let you read and preview this policy, not change it.")));

  // Preview: one repository, the policy as it stands in the form.
  const previewId = "preview-repository";
  const repoError = h("div", { class: "error-text", id: `${previewId}-error` });
  const repoSelect = h("select", { class: "select", id: previewId, "aria-describedby": `${previewId}-help ${previewId}-error` });
  const run = h("button", { type: "submit", class: "btn", id: "preview-run", text: "Preview" });
  const result = h("div", { id: "preview-result", "aria-live": "polite" });
  const noChoice = h("p", { class: "muted", hidden: true, text: "No repository of this format holds components to preview." });

  function fillRepositories(format) {
    const before = repoSelect.value;
    const fits = repositories.filter((r) => r.type !== "group" && (format === "*" || r.format === format)).sort((a, b) => a.name.localeCompare(b.name));
    repoSelect.replaceChildren(...fits.map((r) => h("option", { value: r.name, text: `${r.name} (${r.type})` })));
    const first = (policy.usedBy || []).find((n) => fits.some((r) => r.name === n));
    if (fits.some((r) => r.name === before)) repoSelect.value = before;
    else if (first) repoSelect.value = first;
    repoSelect.disabled = !fits.length;
    run.disabled = !fits.length;
    noChoice.hidden = Boolean(fits.length);
  }

  const f = form({
    sections: sections(creating),
    values: toValues(policy),
    readOnly: !canEdit,
    onChange: (values) => fillRepositories(values.format)
  });
  ctx.setDirty(() => f.isDirty());

  const save = h("button", { type: "submit", class: "btn primary", text: creating ? "Create policy" : "Save" });
  const discard = h("button", { type: "button", class: "btn", text: creating ? "Cancel" : "Discard changes" });
  const formEl = h("form", { novalidate: true, class: "panel storage-form" }, f.el, canEdit ? h("div", { class: "form-actions" }, save, discard) : null);
  ctx.main.append(formEl);

  const usage = creating
    ? null
    : h(
        "section",
        { class: "panel", id: "policy-usage", "aria-labelledby": "policy-usage-title" },
        h("h2", { id: "policy-usage-title", text: "Applied by" }),
        (policy.usedBy || []).length
          ? repositoryList(
              ctx,
              policy.usedBy.map((n) => byName[n] || { name: n })
            )
          : h("p", { class: "muted", text: "No repository applies this policy." })
      );

  const previewForm = h(
    "form",
    { class: "panel", id: "preview-form", novalidate: true, "aria-labelledby": "preview-title" },
    h("h2", { id: "preview-title", text: "Preview" }),
    h("p", { class: "help", id: `${previewId}-help`, text: "Lists what the policy in the form would remove from one repository now. Nothing is removed." }),
    h(
      "div",
      { class: "inline-fields" },
      h("div", { class: "field" }, h("label", { class: "label", for: previewId, text: "Repository" }), repoSelect, repoError),
      h("div", { class: "actions" }, run)
    ),
    noChoice,
    result
  );
  ctx.main.append(...[usage, previewForm].filter(Boolean));
  fillRepositories(policy.format || "*");

  previewForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    repoError.textContent = "";
    repoSelect.removeAttribute("aria-invalid");
    const repository = repoSelect.value;
    if (!repository) return;
    const body = toBody(f.values());
    result.replaceChildren(loading("Loading the preview"));
    let answer;
    try {
      answer = await busy(run, () => api.cleanupPolicies.preview(body, repository));
    } catch (error) {
      result.replaceChildren();
      const inForm = policyErrors(error);
      const onRepo = (error.fieldErrors || []).filter((e) => e.id === "repository");
      if (inForm.length) f.showServerErrors({ fieldErrors: inForm });
      if (onRepo.length) {
        repoError.textContent = onRepo.map((e) => e.message).join(" ");
        repoSelect.setAttribute("aria-invalid", "true");
      }
      if (!inForm.length && !onRepo.length) result.append(problem(error, "No preview"));
      else if (inForm.length) result.append(notice("error", "No preview", "Change the fields marked in the form, then preview again."));
      return;
    }
    const items = answer.items || [];
    const total = answer.total ?? items.length;
    const summary = [
      total ? `${plural(total, "component")} would be removed from ${repository}.` : `No component would be removed from ${repository}.`,
      "Nothing is removed by this preview."
    ];
    if (answer.truncated) summary.push(`The first ${items.length} of ${total} are listed.`);
    const parts = [h("p", { id: "preview-summary", class: "preview-summary", text: summary.join(" ") })];
    if (items.length) {
      const table = dataTable(
        [
          { key: "group", label: "Group", class: "keep" },
          { key: "name", label: "Name", class: "keep" },
          { key: "version", label: "Version", class: "keep" },
          { key: "path", label: "Path", render: (r) => h("span", { class: "mono small break", text: r.path }) }
        ],
        { rows: items, caption: `Components the policy would remove from ${repository}`, filterLabel: "Filter the components" }
      );
      parts.push(table.el);
    } else {
      parts.push(emptyState({ title: "Nothing to remove", text: "No component of this repository meets the criteria today." }));
    }
    result.replaceChildren(...parts);
  });

  discard.addEventListener("click", () => {
    if (creating) {
      ctx.setDirty(null);
      ctx.navigate("/admin/cleanup-policies");
    } else {
      f.set(toValues(policy));
      f.clearErrors();
      fillRepositories(policy.format);
    }
  });

  formEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    messages.replaceChildren();
    if (!canEdit || !f.validate()) return;
    const body = toBody(f.values());
    try {
      await busy(save, () => (creating ? api.cleanupPolicies.create(body) : api.cleanupPolicies.update(name, body)));
    } catch (error) {
      if (!f.showServerErrors(error)) messages.append(problem(error, creating ? "Not created" : "Not saved"));
      return;
    }
    f.markClean();
    if (creating) {
      ctx.setDirty(null);
      toast(`The cleanup policy ${body.name} is created.`);
      ctx.navigate(`/admin/cleanup-policies/${encodeURIComponent(body.name)}`);
      return;
    }
    try {
      policy = await api.cleanupPolicies.get(name);
      f.set(toValues(policy));
    } catch (error) {
      /* the save went through; the form keeps what was sent */
    }
    toast(`The cleanup policy ${name} is saved.`);
  });
}

