// S28 Realms: which realms are active and in which order. An empty list is refused, and a list
// without the local realm warns that no local user could sign in.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { pageHead, failure, busy } from "../../ui/page.js";
import { form } from "../../ui/form.js";
import { notice, toast, errorText, problem } from "../../ui/notice.js";
import { confirm } from "../../ui/dialog.js";
import { REALM } from "./common.js";

export async function render(ctx) {
  const canSave = ctx.can("nexus:settings:update");
  ctx.main.append(
    pageHead({
      title: "Realms",
      sub: "A sign in tries the active realms from the top. Token realms let clients sign in with a token of their kind."
    })
  );
  let available;
  let active;
  try {
    [available, active] = await Promise.all([api.realms.available(), api.realms.active()]);
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/realms", { replace: true }) });
    return;
  }
  const options = available.map((r) => ({ value: r.id, label: r.name }));
  for (const id of active) if (!available.some((r) => r.id === id)) options.push({ value: id, label: "A realm this server does not list" });
  const localName = (available.find((r) => r.id === REALM.local) || { name: "Local users" }).name;

  const warning = h("div", { class: "page-messages" });
  const warn = (list) => {
    if ((list || []).includes(REALM.local)) {
      warning.replaceChildren();
      return;
    }
    if (warning.querySelector(".realm-warning")) return;
    const el = notice("warning", "Local users cannot sign in", `The realm ${localName} is not active. No local user can sign in, administrators included, while it stays off.`);
    el.classList.add("realm-warning");
    warning.replaceChildren(el);
  };

  let values = { active };
  const f = form({
    fields: [
      {
        name: "active",
        label: "Realms",
        type: "transfer",
        ordered: true,
        required: true,
        availableLabel: "Available",
        selectedLabel: "Active, in order",
        options,
        help: "Mark a realm and use Add, Remove, Up and Down. A double click moves a realm to the other side."
      }
    ],
    values,
    readOnly: !canSave,
    onChange: (v) => warn(v.active)
  });
  ctx.setDirty(() => f.isDirty());
  warn(active);

  const messages = h("div", { class: "stack page-messages" });
  if (!canSave) messages.append(notice("info", "Read only", ctx.readOnlyText("Your roles let you read the realms, not change them.")));
  const save = h("button", { type: "submit", class: "btn primary", text: "Save" });
  const discard = h("button", { type: "button", class: "btn", text: "Discard changes" });
  const formEl = h("form", { novalidate: true, class: "panel" }, f.el, canSave ? h("div", { class: "form-actions" }, save, discard) : null);
  ctx.main.append(messages, warning, formEl);

  discard.addEventListener("click", () => {
    f.set(values);
    f.clearErrors();
    warn(values.active);
  });

  formEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    messages.replaceChildren();
    if (!f.validate()) return;
    const ids = f.values().active || [];
    if (!ids.includes(REALM.local)) {
      const ok = await confirm({
        title: `Save without ${localName}?`,
        text: "No local user can sign in after this save, administrators included. Only directory users and tokens still work.",
        confirmLabel: "Save anyway",
        danger: true
      });
      if (!ok) return;
    }
    try {
      await busy(save, () => api.realms.setActive(ids));
    } catch (error) {
      if (!f.showServerErrors(error)) messages.append(problem(error, "Not saved"));
      return;
    }
    try {
      values = { active: await api.realms.active() };
    } catch (error) {
      values = { active: ids };
    }
    f.set(values);
    warn(values.active);
    toast("The realms are saved.");
  });
}
