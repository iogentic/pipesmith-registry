// Page header, loading and error states shared by every screen.

import { h, replace } from "../dom.js";
import { problem } from "./notice.js";
import { isPreviewError, learnCurrent, PREVIEW_TITLE } from "../preview.js";

// crumbs: [{label, href}] ending before the current page. actions: buttons or links.
export function pageHead({ title, sub, crumbs = [], actions = [] }) {
  const nav = crumbs.length
    ? h(
        "nav",
        { class: "crumbs", "aria-label": "Breadcrumb" },
        crumbs.flatMap((c, i) => [i ? h("span", { "aria-hidden": "true", text: "/" }) : null, h("a", { href: c.href, text: c.label })])
      )
    : null;
  return h(
    "header",
    { class: "page-head" },
    h("div", { class: "titles" }, nav, h("h1", { tabindex: "-1", text: title }), sub ? h("p", { class: "sub", text: sub }) : null),
    actions.length ? h("div", { class: "actions" }, actions) : null
  );
}

export function loading(text = "Loading") {
  return h("p", { class: "loading", role: "status", text });
}

export function button(label, { kind = "", onClick, type = "button", disabled = false, small = false, attrs = {} } = {}) {
  const el = h("button", { type, class: ["btn", kind, small && "small"], text: label, disabled, ...attrs });
  if (onClick) el.addEventListener("click", onClick);
  return el;
}

export function linkButton(label, href, kind = "") {
  return h("a", { class: ["btn", kind], href, text: label });
}

// Runs an action of a button: busy state while it runs, a toast or a notice afterwards.
export async function busy(buttonEl, action) {
  buttonEl.setAttribute("aria-busy", "true");
  buttonEl.disabled = true;
  try {
    return await action();
  } finally {
    buttonEl.removeAttribute("aria-busy");
    buttonEl.disabled = false;
  }
}

export function stateView({ code, title, text, actions = [] }) {
  return h(
    "section",
    { class: "state-page" },
    code ? h("p", { class: "code", text: code }) : null,
    h("h1", { tabindex: "-1", text: title }),
    h("p", { text }),
    actions.length ? h("div", { class: "actions" }, actions) : null
  );
}

export function noAccess() {
  return stateView({
    code: "No access",
    title: "This page is not open to you",
    text: "Your roles do not include the permission this page needs. An administrator can add it to one of your roles.",
    actions: [linkButton("Go to the overview", "#/overview", "primary")]
  });
}

export function notFound(what = "page") {
  return stateView({
    code: "Not found",
    title: `This ${what} does not exist`,
    text: "The address may be old, or the object was deleted or renamed.",
    actions: [linkButton("Go to the overview", "#/overview", "primary")]
  });
}

export function previewView() {
  return stateView({
    code: "Preview",
    title: PREVIEW_TITLE,
    text: "This server does not offer this page yet. It comes with a later step of the server; the other pages work as usual.",
    actions: [linkButton("Go to the overview", "#/overview", "primary")]
  });
}

// A screen that only makes changes, while the server takes none.
export function readOnlyView(reason) {
  return stateView({
    code: "Read only",
    title: "The server takes no changes",
    text: `${reason} This page only makes changes; the other pages show what is stored.`,
    actions: [linkButton("Go to the overview", "#/overview", "primary")]
  });
}

// What a screen shows when its first request fails.
export function failure(main, error, { what = "page", retry } = {}) {
  if (isPreviewError(error)) {
    learnCurrent(error);
    return replace(main, previewView());
  }
  if (error && error.status === 403) return replace(main, noAccess());
  if (error && error.status === 404) return replace(main, notFound(what));
  const actions = retry ? [h("button", { type: "button", class: "btn", text: "Try again", on: { click: retry } })] : [];
  return replace(
    main,
    h("div", { class: "stack" }, problem(error, error && error.status === 501 ? "Not available" : "This page could not be loaded"), actions.length ? h("div", { class: "actions" }, actions) : null)
  );
}

export function emptyState({ title, text, actions = [] }) {
  return h("div", { class: "empty" }, h("h2", { text: title }), text ? h("p", { text }) : null, actions.length ? h("div", { class: "actions" }, actions) : null);
}

// A list of label and value. empty is the text of a missing value; a file detail says what is
// missing ("Not recorded") rather than "None".
export function facts(rows, { empty = "None" } = {}) {
  return h(
    "dl",
    { class: "facts" },
    rows.filter(Boolean).flatMap(([label, value]) => [h("dt", { text: label }), h("dd", {}, value === null || value === undefined || value === "" ? h("span", { class: "muted", text: empty }) : value)])
  );
}
