// Notices in the page and short messages in a live region.

import { h } from "../dom.js";
import { isPreviewError, PREVIEW_TITLE, PREVIEW_TEXT } from "../preview.js";

export function notice(kind, title, text, ...extra) {
  return h(
    "div",
    { class: ["notice", kind], role: kind === "error" ? "alert" : "status" },
    title ? h("div", { class: "title", text: title }) : null,
    text ? h("div", { text }) : null,
    ...extra
  );
}

export function mark(kind, label) {
  return h("span", { class: ["mark", kind], text: label });
}

let region = null;

function toastRegion() {
  if (!region || !region.isConnected) {
    region = h("div", { class: "toasts", "aria-live": "polite", "aria-atomic": "false" });
    document.body.append(region);
  }
  return region;
}

export function toast(text, kind = "success") {
  const close = h("button", { class: "btn quiet small", type: "button", "aria-label": "Dismiss", text: "Close" });
  const el = h("div", { class: ["toast", kind], role: kind === "error" ? "alert" : "status" }, h("div", { text }), close);
  const remove = () => el.remove();
  close.addEventListener("click", remove);
  toastRegion().append(el);
  setTimeout(remove, kind === "error" ? 9000 : 5000);
  return el;
}

// The text of an error for a notice: the message of the server, or a fixed text per status.
export function errorText(error) {
  if (!error) return "Something went wrong.";
  if (isPreviewError(error)) return `${PREVIEW_TITLE}. ${PREVIEW_TEXT}`;
  return error.message || "Something went wrong.";
}

// A failed request as a notice: calm for a function that is not in this preview, an error otherwise.
export function problem(error, title = "Not done") {
  if (isPreviewError(error)) return notice("info", PREVIEW_TITLE, PREVIEW_TEXT);
  return notice("error", title, errorText(error));
}

// A failed action as a short message, calm for a function that is not in this preview.
export function toastProblem(error) {
  if (isPreviewError(error)) return toast(`${PREVIEW_TITLE}. ${PREVIEW_TEXT}`, "info");
  return toast(errorText(error), "error");
}
