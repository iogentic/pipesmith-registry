// S17 Routing rule editor: mode, matchers as repeated rows, the repositories that use the rule, and a
// test of one path against the rule as it stands in the form. docs/ui/API.md, S17.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { plural } from "../../format.js";
import { pageHead, failure, button, busy } from "../../ui/page.js";
import { form } from "../../ui/form.js";
import { notice, toast, errorText, problem } from "../../ui/notice.js";
import { confirm } from "../../ui/dialog.js";
import { NAME, repositoryList, sentence } from "./common.js";

const MODES = [
  { value: "BLOCK", label: "Block: fetch every path except those that match" },
  { value: "ALLOW", label: "Allow: fetch only the paths that match" }
];

function sections(creating) {
  return [
    {
      title: "Rule",
      grid: true,
      fields: [
        creating
          ? { name: "name", label: "Name", type: "text", required: true, pattern: NAME, mono: true, help: "Proxy repositories name the rule by it. It cannot change later." }
          : { name: "name", label: "Name", type: "static", mono: true },
        { name: "description", label: "Description", type: "text" },
        { name: "mode", label: "Mode", type: "radio", required: true, wide: true, options: MODES },
        {
          name: "matchers",
          label: "Matchers",
          type: "list",
          required: true,
          mono: true,
          wide: true,
          addLabel: "Add a matcher",
          help: "One regular expression per row. Each one is tested against the whole path of a request, for example ^/com/example/.*"
        }
      ]
    }
  ];
}

function toBody(values) {
  return { name: values.name, description: values.description || "", mode: values.mode, matchers: values.matchers || [] };
}

// The verdict of a test as a notice, with the matcher that decided in code type.
function verdictNotice(answer, matchers) {
  const index = answer.matcher;
  const matched = index !== null && index !== undefined;
  const pattern = matched ? h("code", { class: "tag", text: matchers[index] || "" }) : null;
  let text;
  if (answer.allowed) {
    text = matched ? `Matcher ${index + 1} matches the path. The rule allows what matches, so the proxy may fetch it.` : "No matcher matches the path. The rule blocks only what matches, so the proxy may fetch it.";
  } else {
    text = matched ? `Matcher ${index + 1} matches the path. The rule blocks what matches, so the proxy does not ask its remote.` : "No matcher matches the path. The rule allows only what matches, so the proxy does not ask its remote.";
  }
  return notice(answer.allowed ? "success" : "warning", answer.allowed ? "Allowed" : "Blocked", text, pattern ? h("div", {}, pattern) : null);
}

export async function render(ctx) {
  ctx.main.classList.add("area-storage");
  const name = ctx.params.name;
  const creating = !name;
  const canEdit = ctx.can("nexus:settings:update");
  const canSeeRepos = ctx.can("nexus:repository-admin:*:*:read");

  let rule = { name: "", description: "", mode: "BLOCK", matchers: [] };
  let users = null;
  let usersFailed = false;
  try {
    const [one, settings] = await Promise.allSettled([
      creating ? Promise.resolve(null) : api.routingRules.get(name),
      !creating && canSeeRepos ? api.repositories.settings() : Promise.resolve(null)
    ]);
    if (one.status === "rejected") throw one.reason;
    if (one.value) rule = one.value;
    usersFailed = settings.status === "rejected";
    users = settings.status === "fulfilled" && settings.value ? settings.value.filter((r) => r.routingRuleName === name) : null;
  } catch (error) {
    failure(ctx.main, error, { what: "routing rule", retry: () => ctx.navigate(`/admin/routing-rules/${encodeURIComponent(name)}`, { replace: true }) });
    return;
  }

  const title = creating ? "New routing rule" : name;
  ctx.setTitle(title);
  const messages = h("div", { class: "stack page-messages" });

  const actions = [];
  if (!creating && canEdit) {
    actions.push(
      button("Delete", {
        kind: "danger",
        onClick: async () => {
          const count = users ? users.length : 0;
          const ok = await confirm({
            title: `Delete ${name}?`,
            text: count
              ? `${plural(count, "repository", "repositories")} ${count === 1 ? "uses" : "use"} this rule. The server refuses the delete while any repository uses it.`
              : "The rule is removed. Proxies without a rule may fetch every path from their remote.",
            confirmLabel: "Delete rule",
            danger: true
          });
          if (!ok) return;
          messages.replaceChildren();
          try {
            await api.routingRules.remove(name);
          } catch (error) {
            messages.append(problem(error, "Not deleted"));
            messages.scrollIntoView({ block: "nearest" });
            return;
          }
          ctx.setDirty(null);
          toast(`The routing rule ${name} is deleted.`);
          ctx.navigate("/admin/routing-rules");
        }
      })
    );
  }

  ctx.main.append(
    pageHead({
      title,
      sub: creating ? "Decides which paths a proxy repository may fetch from its remote." : "Routing rule",
      crumbs: [{ label: "Routing rules", href: "#/admin/routing-rules" }],
      actions
    }),
    messages
  );
  if (!canEdit) messages.append(notice("info", "Read only", ctx.readOnlyText("Your roles let you read and test this rule, not change it.")));

  const f = form({ sections: sections(creating), values: rule, readOnly: !canEdit });
  ctx.setDirty(() => f.isDirty());
  const save = h("button", { type: "submit", class: "btn primary", text: creating ? "Create rule" : "Save" });
  const discard = h("button", { type: "button", class: "btn", text: creating ? "Cancel" : "Discard changes" });
  const formEl = h("form", { novalidate: true, class: "panel storage-form" }, f.el, canEdit ? h("div", { class: "form-actions" }, save, discard) : null);
  ctx.main.append(formEl);

  // Test of one path against the rule in the form.
  const pathInput = h("input", {
    class: "input mono",
    id: "rule-test-path",
    type: "text",
    autocomplete: "off",
    spellcheck: "false",
    placeholder: "/com/example/tool/1.0/tool-1.0.jar",
    value: ctx.query.path || "",
    "aria-describedby": "rule-test-help rule-test-path-error"
  });
  const pathError = h("div", { class: "error-text", id: "rule-test-path-error" });
  const run = h("button", { type: "submit", class: "btn", id: "rule-test-run", text: "Test" });
  const result = h("div", { id: "rule-test-result", "aria-live": "polite" });
  let testMarked = false;
  const testForm = h(
    "form",
    { class: "panel", id: "rule-test", novalidate: true, "aria-labelledby": "rule-test-title" },
    h("h2", { id: "rule-test-title", text: "Path test" }),
    h("p", { class: "help", id: "rule-test-help", text: "Tests one path against the mode and matchers in the form, saved or not." }),
    h("div", { class: "inline-fields" }, h("div", { class: "field" }, h("label", { class: "label", for: "rule-test-path", text: "Path" }), pathInput, pathError), h("div", { class: "actions" }, run)),
    result
  );

  testForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    pathError.textContent = "";
    pathInput.removeAttribute("aria-invalid");
    result.replaceChildren();
    const path = pathInput.value.trim();
    if (!path) {
      pathError.textContent = "Enter a path to test.";
      pathInput.setAttribute("aria-invalid", "true");
      pathInput.focus();
      return;
    }
    const values = f.values();
    const matchers = values.matchers || [];
    let answer;
    try {
      answer = await busy(run, () => api.routingRules.test(values.mode, matchers, path));
    } catch (error) {
      const onPath = (error.fieldErrors || []).filter((e) => e.id === "path");
      const inForm = (error.fieldErrors || []).filter((e) => e.id !== "path");
      if (onPath.length) {
        pathError.textContent = onPath.map((e) => e.message).join(" ");
        pathInput.setAttribute("aria-invalid", "true");
      }
      if (inForm.length) {
        f.showServerErrors({ fieldErrors: inForm });
        testMarked = true;
        result.append(notice("error", "Not tested", "Change the fields marked in the form, then test again."));
      } else if (!onPath.length) {
        result.append(problem(error, "Not tested"));
      }
      return;
    }
    if (testMarked) f.clearErrors();
    testMarked = false;
    result.append(verdictNotice(answer, matchers));
  });

  const parts = [testForm];
  if (!creating) {
    let body;
    if (users) body = users.length ? repositoryList(ctx, users) : h("p", { class: "muted", text: "No repository uses this rule." });
    else if (usersFailed || !canSeeRepos) body = h("p", { class: "muted", text: "Your roles do not show which repositories use this rule." });
    parts.push(h("section", { class: "panel", id: "rule-usage", "aria-labelledby": "rule-usage-title" }, h("h2", { id: "rule-usage-title", text: "Used by" }), body));
  }
  ctx.main.append(...parts);

  discard.addEventListener("click", () => {
    if (creating) {
      ctx.setDirty(null);
      ctx.navigate("/admin/routing-rules");
    } else {
      f.set(rule);
      f.clearErrors();
    }
  });

  formEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    messages.replaceChildren();
    if (!canEdit || !f.validate()) return;
    const body = toBody(f.values());
    try {
      await busy(save, () => (creating ? api.routingRules.create(body) : api.routingRules.update(name, body)));
    } catch (error) {
      if (!f.showServerErrors(error)) messages.append(problem(error, creating ? "Not created" : "Not saved"));
      return;
    }
    f.markClean();
    if (creating) {
      ctx.setDirty(null);
      toast(`The routing rule ${body.name} is created.`);
      ctx.navigate(`/admin/routing-rules/${encodeURIComponent(body.name)}`);
      return;
    }
    try {
      rule = await api.routingRules.get(name);
      f.set(rule);
    } catch (error) {
      /* the save went through; the form keeps what was sent */
    }
    toast(`The routing rule ${name} is saved.`);
  });
}
