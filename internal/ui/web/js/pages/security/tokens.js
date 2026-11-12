// S29 Tokens: the own tokens. Issue one of a kind whose realm is active, see its secret once with a
// copy button, revoke. The server keeps only a digest, so the secret lives in this page until it is
// left or dismissed.

import { h, replace, copyText } from "../../dom.js";
import { api } from "../../api.js";
import { day } from "../../format.js";
import { pageHead, failure, busy, loading, linkButton } from "../../ui/page.js";
import { form } from "../../ui/form.js";
import { notice, toast, errorText, problem } from "../../ui/notice.js";
import { TOKEN_KINDS, kindOf, tokenTable } from "./common.js";

// A name for a new token that no token of the list has yet.
function suggestion(kind, rows) {
  const base = `${kindOf(kind).label} ${day(Date.now())}`;
  const taken = new Set((rows || []).map((t) => t.name));
  let name = base;
  for (let n = 2; taken.has(name); n += 1) name = `${base} (${n})`;
  return name;
}

function secretPanel(token, onDone) {
  const copy = h("button", { type: "button", class: "btn small", text: "Copy", "aria-label": "Copy the token" });
  copy.addEventListener("click", async () => {
    toast((await copyText(token.secret)) ? "The token is copied to the clipboard." : "The browser refused the clipboard. Select the token and copy it.", "info");
  });
  const done = h("button", { type: "button", class: "btn small quiet", text: "Done" });
  done.addEventListener("click", onDone);
  const kind = kindOf(token.kind);
  return h(
    "section",
    { class: "panel token-secret", "aria-labelledby": "token-secret-title" },
    h("h2", { id: "token-secret-title", tabindex: "-1", text: "New token" }),
    notice("warning", "Copy the token now", "It is shown this once. The server keeps only a digest of it and cannot show it again."),
    h("div", { class: "secret-row" }, h("code", { class: "secret", text: token.secret }), h("div", { class: "actions" }, copy, done)),
    h("p", { class: "help", text: kind.use })
  );
}

export async function render(ctx) {
  ctx.main.append(
    pageHead({
      title: "Tokens",
      sub: "Tokens let clients such as npm, Docker and build tools sign in without your password. Revoke a token when a client no longer needs it."
    })
  );
  let rows;
  let active;
  try {
    [rows, active] = await Promise.all([api.tokens.list(), api.realms.active()]);
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/tokens", { replace: true }) });
    return;
  }
  const offered = TOKEN_KINDS.filter((k) => active.includes(k.realm));

  const secretHolder = h("div");
  const listHolder = h("div");
  const issueHolder = h("section", { class: "panel token-issue", "aria-labelledby": "token-issue-title" }, h("h2", { id: "token-issue-title", text: "Issue a token" }));
  ctx.main.append(secretHolder, issueHolder, h("section", { class: "section" }, h("h2", { text: "Your tokens" }), listHolder));

  async function drawList() {
    replace(listHolder, tokenTable(rows, { active, onRevoke: reload, empty: { title: "No token yet", text: offered.length ? "Issue a token above for a client that should not use your password." : "No token realm is active." } }).el);
  }
  async function reload() {
    replace(listHolder, loading("Loading tokens"));
    try {
      rows = await api.tokens.list();
      drawList();
    } catch (error) {
      replace(listHolder, problem(error, "The tokens could not be loaded"));
    }
  }
  drawList();

  if (!offered.length) {
    issueHolder.append(
      notice("info", "No token realm is active", "An administrator can activate the npm, Docker or user token realm. Until then no token can be issued here."),
      ctx.can("nexus:settings:read") ? h("div", { class: "actions after-notice" }, linkButton("Open the realms", "#/admin/realms")) : null
    );
    return;
  }

  let lastSuggestion = suggestion(offered[0].kind, rows);
  const f = form({
    sections: [
      {
        grid: true,
        fields: [
          {
            name: "kind",
            label: "Kind",
            type: "select",
            required: true,
            noBlank: true,
            options: offered.map((k) => ({ value: k.kind, label: k.label })),
            help: TOKEN_KINDS.length > offered.length ? "Only kinds whose realm is active are offered." : null
          },
          { name: "name", label: "Name", type: "text", required: true, autocomplete: "off", help: "Where you use it, for example the name of the laptop or the build job." }
        ]
      }
    ],
    values: { kind: offered[0].kind, name: lastSuggestion },
    onChange: (v) => {
      if (v.name === lastSuggestion) {
        lastSuggestion = suggestion(v.kind, rows);
        f.set({ ...v, name: lastSuggestion });
      }
    }
  });
  const issue = h("button", { type: "submit", class: "btn primary", text: "Issue token" });
  const errors = h("div", { class: "stack" });
  const formEl = h("form", { novalidate: true }, errors, f.el, h("div", { class: "form-actions" }, issue));
  issueHolder.append(formEl);

  formEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    errors.replaceChildren();
    if (!f.validate()) return;
    const v = f.values();
    let token;
    try {
      token = await busy(issue, () => api.tokens.create(v.kind, v.name.trim()));
    } catch (error) {
      if (!f.showServerErrors(error)) errors.append(problem(error, "Not issued"));
      return;
    }
    replace(
      secretHolder,
      secretPanel(token, () => {
        replace(secretHolder);
        issue.focus();
      })
    );
    secretHolder.querySelector("h2").focus();
    await reload();
    lastSuggestion = suggestion(v.kind, rows);
    f.set({ kind: v.kind, name: lastSuggestion });
  });
}
