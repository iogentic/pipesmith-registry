// Modal dialogs on the native dialog element: it keeps the focus inside and closes on Escape.

import { h } from "../dom.js";
import { notice, errorText, problem } from "./notice.js";

export function openDialog({ title, body, actions, wide = false, onOpen }) {
  return new Promise((resolve) => {
    const titleId = `dialog-title-${Math.random().toString(36).slice(2, 8)}`;
    const foot = h("div", { class: "modal-foot" });
    const dialog = h(
      "dialog",
      { class: ["modal", wide && "wide"], "aria-labelledby": titleId },
      h("div", { class: "modal-body" }, h("h2", { id: titleId, text: title }), body),
      foot
    );
    let result = null;
    const finish = (value) => {
      result = value;
      dialog.close();
    };
    for (const action of actions) {
      const button = h("button", {
        type: "button",
        class: ["btn", action.kind],
        text: action.label,
        dataset: { action: action.value }
      });
      button.addEventListener("click", async () => {
        if (action.handler) {
          button.setAttribute("aria-busy", "true");
          button.disabled = true;
          try {
            const keep = await action.handler();
            if (keep === false) return;
            finish(action.value);
          } finally {
            button.removeAttribute("aria-busy");
            button.disabled = false;
          }
        } else {
          finish(action.value);
        }
      });
      foot.append(button);
    }
    dialog.addEventListener("close", () => {
      dialog.remove();
      resolve(result);
    });
    document.body.append(dialog);
    dialog.showModal();
    if (onOpen) onOpen(dialog);
  });
}

// Ask before a change that cannot be undone. With typeToConfirm the user has to type that text.
export async function confirm({ title, text, confirmLabel = "Confirm", danger = false, typeToConfirm = null }) {
  const parts = [h("p", { text })];
  let input = null;
  let confirmButton = null;
  if (typeToConfirm) {
    const id = `confirm-${Math.random().toString(36).slice(2, 8)}`;
    input = h("input", { class: "input", id, autocomplete: "off", spellcheck: "false" });
    parts.push(
      h(
        "div",
        { class: "field", dataset: { confirm: "" } },
        h("label", { class: "label", for: id }, "Type ", h("strong", { class: "mono", text: typeToConfirm }), " to confirm"),
        input
      )
    );
  }
  const value = await openDialog({
    title,
    body: h("div", { class: "stack" }, parts),
    actions: [
      { label: "Cancel", value: "cancel", kind: "" },
      { label: confirmLabel, value: "confirm", kind: danger ? "danger" : "primary" }
    ],
    onOpen: (dialog) => {
      confirmButton = dialog.querySelector('[data-action="confirm"]');
      if (input) {
        confirmButton.disabled = true;
        input.addEventListener("input", () => {
          confirmButton.disabled = input.value !== typeToConfirm;
        });
        input.focus();
      } else {
        dialog.querySelector('[data-action="cancel"]').focus();
      }
    }
  });
  return value === "confirm";
}

// A dialog with a form. submit gets the values and may throw an ApiError; its message is shown in
// the dialog and the dialog stays open.
export function formDialog({ title, form, submitLabel = "Save", submit, wide = false }) {
  const errors = h("div");
  return openDialog({
    title,
    wide,
    body: h("div", { class: "stack" }, errors, form.el),
    actions: [
      { label: "Cancel", value: null, kind: "" },
      {
        label: submitLabel,
        value: "submitted",
        kind: "primary",
        handler: async () => {
          errors.replaceChildren();
          if (!form.validate()) return false;
          try {
            await submit(form.values());
            return true;
          } catch (error) {
            if (!form.showServerErrors(error)) errors.append(problem(error, "Not saved"));
            return false;
          }
        }
      }
    ],
    onOpen: () => form.focusFirst()
  });
}
