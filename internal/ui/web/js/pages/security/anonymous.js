// S25 Anonymous access: on or off, and the user and realm a request without a user acts as.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { pageHead, failure, busy } from "../../ui/page.js";
import { form } from "../../ui/form.js";
import { notice, toast, errorText, problem } from "../../ui/notice.js";

export async function render(ctx) {
  const canSave = ctx.can("nexus:settings:update");
  ctx.main.append(
    pageHead({
      title: "Anonymous access",
      sub: "What a request without a user name may do: it acts as the user below and gets the roles of that user."
    })
  );
  let values;
  let realms;
  try {
    [values, realms] = await Promise.all([api.anonymous.get(), api.realms.available()]);
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/anonymous", { replace: true }) });
    return;
  }
  const realmOptions = realms.map((r) => ({ value: r.id, label: r.name }));
  if (values.realmName && !realms.some((r) => r.id === values.realmName)) realmOptions.push({ value: values.realmName, label: "A realm this server does not list" });

  const f = form({
    sections: [
      {
        fields: [
          {
            name: "enabled",
            label: "Allow anonymous access",
            type: "checkbox",
            help: "Clients and browsers can then browse, search and download without signing in, within the roles of the user below. Docker repositories need it for anonymous pull."
          }
        ]
      },
      {
        title: "Acts as",
        grid: true,
        fields: [
          { name: "userId", label: "User id", type: "text", required: true, mono: true, autocomplete: "off", help: "A local user. Its roles decide what anonymous requests may do." },
          { name: "realmName", label: "Realm", type: "select", required: true, noBlank: true, options: realmOptions, help: "The realm that holds this user." }
        ]
      }
    ],
    values,
    readOnly: !canSave
  });
  ctx.setDirty(() => f.isDirty());

  const messages = h("div", { class: "stack page-messages" });
  if (!canSave) messages.append(notice("info", "Read only", ctx.readOnlyText("Your roles let you read this setting, not change it.")));
  const save = h("button", { type: "submit", class: "btn primary", text: "Save" });
  const discard = h("button", { type: "button", class: "btn", text: "Discard changes" });
  const formEl = h("form", { novalidate: true, class: "panel" }, f.el, canSave ? h("div", { class: "form-actions" }, save, discard) : null);
  const links = ctx.can("nexus:users:read")
    ? h("p", { class: "help after-panel" }, "The roles of the user: ", h("a", { href: `#/admin/users/default/${encodeURIComponent(values.userId)}`, text: values.userId }))
    : null;
  ctx.main.append(messages, formEl, links);

  discard.addEventListener("click", () => {
    f.set(values);
    f.clearErrors();
  });

  formEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    messages.replaceChildren();
    if (!f.validate()) return;
    const v = f.values();
    const body = { enabled: Boolean(v.enabled), userId: v.userId.trim(), realmName: v.realmName };
    let answer;
    try {
      answer = await busy(save, () => api.anonymous.update(body));
    } catch (error) {
      if (!f.showServerErrors(error)) messages.append(problem(error, "Not saved"));
      return;
    }
    values = answer && typeof answer === "object" ? answer : body;
    f.set(values);
    if (links) {
      const a = links.querySelector("a");
      a.textContent = values.userId;
      a.setAttribute("href", `#/admin/users/default/${encodeURIComponent(values.userId)}`);
    }
    toast(values.enabled ? "Anonymous access is on." : "Anonymous access is off.");
  });
}
