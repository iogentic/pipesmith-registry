// S36 Capability editor: summary, notes and property values of one imported capability; enable,
// disable and delete. Enable and disable save the stored entry with `enabled` changed, so edits in
// the form stay where they are until Save.

import { h } from "../../dom.js";
import { fieldLabel } from "../../format.js";
import { api } from "../../api.js";
import { pageHead, failure, button, busy, notFound, facts } from "../../ui/page.js";
import { form } from "../../ui/form.js";
import { notice, toast, errorText, problem, toastProblem } from "../../ui/notice.js";
import { confirm } from "../../ui/dialog.js";
import { supportOf, supportMark, enabledMark } from "./capabilities.js";

const SAFE_KEY = /^[A-Za-z0-9_-]+$/;

export async function render(ctx) {
  const id = ctx.params.id;
  let cap;
  let support = null;
  try {
    const [caps, sup] = await Promise.allSettled([api.capabilities.list(), api.capabilities.support()]);
    if (caps.status === "rejected") throw caps.reason;
    cap = (caps.value || []).find((c) => c.id === id);
    support = sup.status === "fulfilled" ? sup.value || {} : null;
  } catch (error) {
    failure(ctx.main, error, { what: "capability", retry: () => location.reload() });
    return;
  }
  if (!cap) {
    ctx.main.append(notFound("capability"));
    return;
  }

  const canEdit = ctx.can("nexus:capabilities:update");
  const canDelete = ctx.can("nexus:capabilities:delete");
  const s = supportOf(support, cap.type);
  ctx.setTitle(cap.type);

  // Property names become field names; a name with other characters gets a field by position.
  const keys = Object.keys(cap.properties || {});
  const fieldName = (key, i) => (SAFE_KEY.test(key) ? `properties.${key}` : `propertyByIndex.p${i}`);
  const toValues = (c) => {
    const v = { notes: c.notes || "", properties: {}, propertyByIndex: {} };
    keys.forEach((key, i) => {
      const value = (c.properties || {})[key];
      if (SAFE_KEY.test(key)) v.properties[key] = value ?? "";
      else v.propertyByIndex[`p${i}`] = value ?? "";
    });
    return v;
  };
  const fromValues = (v) => {
    const out = {};
    keys.forEach((key, i) => {
      const value = SAFE_KEY.test(key) ? (v.properties || {})[key] : (v.propertyByIndex || {})[`p${i}`];
      out[key] = value === null || value === undefined ? "" : String(value);
    });
    return out;
  };
  // Server error ids name the property; the form knows the field by its own name.
  const mapErrors = (error) => ({
    fieldErrors: (error.fieldErrors || []).map((e) => {
      const m = /^properties\.(.+)$/.exec(e.id);
      if (!m) return e;
      const i = keys.indexOf(m[1]);
      return i >= 0 ? { ...e, id: fieldName(m[1], i) } : e;
    })
  });

  const actionsEl = h("div", { class: "actions" });
  const head = pageHead({ title: cap.type, sub: `Imported capability ${cap.id}`, crumbs: [{ label: "Capabilities", href: "#/admin/capabilities" }] });
  head.append(actionsEl);
  const summaryBody = h("div");
  const summary = h("section", { class: "panel sys-panel", "aria-labelledby": "capability-summary" }, h("h2", { id: "capability-summary", text: "Summary" }), summaryBody);
  const messages = h("div", { class: "stack" });
  if (!s.supported) messages.append(notice("info", "No effect here", `${s.reason} The entry is kept as it was imported.`));
  if (!canEdit) messages.append(notice("info", "Read only", ctx.readOnlyText("Your roles let you read this capability, not change it.")));

  const fields = [
    { name: "notes", label: "Notes", type: "textarea", rows: 3, wide: true, help: "Free text for administrators. The server does not read it." },
    ...keys.map((key, i) => ({ name: fieldName(key, i), label: fieldLabel(key), help: `Stored under the key ${key}.`, type: "text", mono: true, wide: true, autocomplete: "off" }))
  ];
  let values = toValues(cap);
  const f = form({
    sections: [
      { fields: [fields[0]] },
      { title: "Properties", help: keys.length ? "The values as they were imported. Saving keeps the names; only the values change." : "This capability has no properties.", grid: true, fields: fields.slice(1) }
    ],
    values,
    readOnly: !canEdit
  });
  ctx.setDirty(() => f.isDirty());

  const save = h("button", { type: "submit", class: "btn primary", text: "Save" });
  const discard = h("button", { type: "button", class: "btn", text: "Discard changes" });
  const formEl = h("form", { novalidate: true, class: "panel" }, f.el, canEdit ? h("div", { class: "form-actions" }, save, discard) : null);
  ctx.main.append(head, h("div", { class: "stack" }, messages, summary, formEl));

  function draw() {
    summaryBody.replaceChildren(
      facts([
        ["State", enabledMark(cap.enabled)],
        ["In this server", h("span", { class: "cell-stack" }, supportMark(s), s.reason ? h("span", { class: "muted small", text: s.reason }) : null)],
        ["Type", h("span", { class: "mono", text: cap.type })],
        ["Id", h("span", { class: "mono", text: cap.id })]
      ])
    );
    actionsEl.replaceChildren();
    if (canEdit) {
      actionsEl.append(
        cap.enabled
          ? button("Disable", { attrs: { id: "capability-disable" }, onClick: (event) => setEnabled(event.currentTarget, false) })
          : button("Enable", { attrs: { id: "capability-enable" }, onClick: (event) => setEnabled(event.currentTarget, true) })
      );
    }
    if (canDelete) actionsEl.append(button("Delete", { kind: "danger", attrs: { id: "capability-delete" }, onClick: remove }));
  }

  async function reloadCap() {
    const list = await api.capabilities.list();
    const fresh = (list || []).find((c) => c.id === id);
    if (fresh) cap = fresh;
  }

  async function setEnabled(buttonEl, enabled) {
    const body = { id: cap.id, type: cap.type, notes: cap.notes || "", enabled, properties: { ...(cap.properties || {}) } };
    try {
      await busy(buttonEl, () => api.capabilities.update(cap.id, body));
    } catch (error) {
      toastProblem(error);
      return;
    }
    try {
      await reloadCap();
    } catch (error) {
      cap = { ...cap, enabled };
    }
    if (!f.isDirty()) {
      values = toValues(cap);
      f.set(values);
    }
    draw();
    const next = actionsEl.querySelector("button");
    if (next) next.focus();
    toast(`The capability ${cap.type} is ${enabled ? "enabled" : "disabled"}.`);
  }

  async function remove() {
    const ok = await confirm({
      title: `Delete ${cap.type}?`,
      text: "The imported entry and its properties are removed. This cannot be undone.",
      confirmLabel: "Delete capability",
      danger: true
    });
    if (!ok) return;
    try {
      await api.capabilities.remove(cap.id);
    } catch (error) {
      toastProblem(error);
      return;
    }
    ctx.setDirty(null);
    toast(`The capability ${cap.type} is deleted.`);
    ctx.navigate("/admin/capabilities");
  }

  discard.addEventListener("click", () => {
    f.set(values);
    f.clearErrors();
    messages.querySelectorAll(".notice.error").forEach((n) => n.remove());
  });

  formEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    messages.querySelectorAll(".notice.error").forEach((n) => n.remove());
    if (!f.validate()) return;
    const v = f.values();
    const body = { id: cap.id, type: cap.type, notes: v.notes || "", enabled: cap.enabled, properties: fromValues(v) };
    try {
      await busy(save, () => api.capabilities.update(cap.id, body));
    } catch (error) {
      if (!f.showServerErrors(mapErrors(error))) messages.append(problem(error, "Not saved"));
      return;
    }
    f.markClean();
    try {
      await reloadCap();
    } catch (error) {
      cap = { ...cap, notes: body.notes, properties: body.properties };
    }
    values = toValues(cap);
    f.set(values);
    draw();
    toast(`The capability ${cap.type} is saved.`);
  });

  draw();
}
