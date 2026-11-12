// S01 Sign in.

import { h } from "../dom.js";
import { api } from "../api.js";
import { notice } from "../ui/notice.js";

export async function render(ctx) {
  const errors = h("div", { class: "stack", "aria-live": "polite" });
  const user = h("input", { class: "input", id: "signin-user", name: "username", autocomplete: "username", required: true, autocapitalize: "none", spellcheck: "false" });
  const pass = h("input", { class: "input", id: "signin-password", name: "password", type: "password", autocomplete: "current-password", required: true });
  const submit = h("button", { type: "submit", class: "btn primary", text: "Sign in" });
  const formEl = h(
    "form",
    { novalidate: true },
    errors,
    h("div", { class: "field" }, h("label", { class: "label", for: "signin-user", text: "User name" }), user),
    h("div", { class: "field" }, h("label", { class: "label", for: "signin-password", text: "Password" }), pass),
    submit
  );
  const page = h(
    "div",
    { class: "signin" },
    h(
      "main",
      { class: "panel", id: "main" },
      h("div", { class: "brand" }, h("img", { src: "assets/symbol-64.png", alt: "", width: "28", height: "28" }), h("span", { text: "Pipesmith Registry" })),
      h("h1", { tabindex: "-1", text: "Sign in" }),
      h("p", { class: "sub", text: "With your user name and password, or with your directory account." }),
      formEl
    )
  );
  ctx.main.append(page);

  formEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    errors.replaceChildren();
    const missing = [];
    if (!user.value.trim()) missing.push("Enter your user name.");
    if (!pass.value) missing.push("Enter your password.");
    user.toggleAttribute("aria-invalid", !user.value.trim());
    pass.toggleAttribute("aria-invalid", !pass.value);
    if (missing.length) {
      errors.append(notice("error", "Not signed in", missing.join(" ")));
      (user.value.trim() ? pass : user).focus();
      return;
    }
    submit.disabled = true;
    submit.setAttribute("aria-busy", "true");
    try {
      const session = await api.session.signIn(user.value.trim(), pass.value);
      await ctx.signedIn(session);
      ctx.navigate(ctx.query.next || "/overview", { replace: true });
    } catch (error) {
      let text = "The user name or the password is wrong.";
      if (error.status === 429) text = error.retryAfter ? `Too many attempts. Try again in ${error.retryAfter} seconds.` : "Too many attempts. Wait a moment and try again.";
      else if (error.status === 0) text = "The server cannot be reached.";
      else if (error.status !== 401) text = error.message;
      errors.append(notice("error", "Not signed in", text));
      pass.value = "";
      pass.focus();
    } finally {
      submit.disabled = false;
      submit.removeAttribute("aria-busy");
    }
  });

  user.focus();
}
