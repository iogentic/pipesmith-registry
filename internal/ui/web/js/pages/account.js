// S41 Account: the own names, mail and password (docs/ui/API.md, section S41). A user of a directory
// keeps these in the directory, so the page shows them read only and offers no password change; so
// does every user while the server takes no changes.

import { h } from "../dom.js";
import { api } from "../api.js";
import { pageHead, facts, busy } from "../ui/page.js";
import { form } from "../ui/form.js";
import { notice, toast, errorText, problem } from "../ui/notice.js";

const MIN_PASSWORD = 8;

function sourceName(source) {
  return !source || source === "default" ? "Local user of this server" : `Directory (${source})`;
}

export async function render(ctx) {
  const user = ctx.session.user;
  const directory = Boolean(user.source) && user.source !== "default";
  const blocked = ctx.writeBlock();
  ctx.main.append(pageHead({ title: "Account", sub: `Signed in as ${user.userId}.` }));

  if (directory || blocked) {
    ctx.main.append(
      h(
        "div",
        { class: "stack" },
        directory
          ? notice("info", "Kept in the directory", "Your names, your mail address and your password come from the directory server. Change them there; this page shows them as the server knows them.")
          : notice("info", "Read only", ctx.readOnlyText(blocked)),
        h(
          "section",
          { class: "panel", id: "profile-form", "aria-labelledby": "profile-title" },
          h("h2", { id: "profile-title", class: "panel-title", text: "Profile" }),
          facts([
            ["User ID", h("span", { class: "mono", text: user.userId })],
            ["First name", user.firstName],
            ["Last name", user.lastName],
            ["Mail", user.emailAddress],
            ["Source", sourceName(user.source)]
          ])
        )
      )
    );
    return;
  }

  // Profile

  const profile = form({
    sections: [
      {
        title: "Profile",
        grid: true,
        fields: [
          { name: "userId", label: "User ID", type: "static", mono: true },
          { name: "source", label: "Source", type: "static", render: sourceName },
          { name: "firstName", label: "First name", type: "text", required: true, autocomplete: "given-name" },
          { name: "lastName", label: "Last name", type: "text", required: true, autocomplete: "family-name" },
          { name: "emailAddress", label: "Mail", type: "email", required: true, wide: true, autocomplete: "email" }
        ]
      }
    ],
    values: { userId: user.userId, source: user.source, firstName: user.firstName, lastName: user.lastName, emailAddress: user.emailAddress }
  });
  const profileMessages = h("div", { class: "stack" });
  const save = h("button", { type: "submit", class: "btn primary", text: "Save" });
  const profileEl = h("form", { novalidate: true, class: "panel", id: "profile-form", "aria-label": "Profile" }, profileMessages, profile.el, h("div", { class: "form-actions" }, save));

  profileEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    profileMessages.replaceChildren();
    if (!profile.validate()) return;
    const values = profile.values();
    const body = { firstName: values.firstName.trim(), lastName: values.lastName.trim(), emailAddress: values.emailAddress.trim() };
    try {
      await busy(save, () => api.account.update(body));
    } catch (error) {
      if (!profile.showServerErrors(error)) profileMessages.append(problem(error, "Not saved"));
      return;
    }
    Object.assign(ctx.session.user, body);
    profile.set({ userId: user.userId, source: user.source, ...body });
    toast("Your account is saved.");
  });

  // Password

  const password = form({
    sections: [
      {
        title: "Password",
        help: `The new password needs at least ${MIN_PASSWORD} characters. Your session stays signed in.`,
        fields: [
          { name: "currentPassword", label: "Current password", type: "password", required: true, autocomplete: "current-password" },
          { name: "newPassword", label: "New password", type: "password", required: true, minLength: MIN_PASSWORD, autocomplete: "new-password" },
          {
            name: "repeatPassword",
            label: "New password again",
            type: "password",
            required: true,
            autocomplete: "new-password",
            validate: (value, values) => (value !== values.newPassword ? "The two new passwords differ." : null)
          }
        ]
      }
    ]
  });
  const passwordMessages = h("div", { class: "stack" });
  const change = h("button", { type: "submit", class: "btn primary", text: "Change password" });
  // The user name beside the password fields lets a password manager file the new password.
  const username = h("input", { type: "text", name: "username", autocomplete: "username", value: user.userId, readonly: true, tabindex: "-1", "aria-hidden": "true", class: "visually-hidden" });
  const passwordEl = h("form", { novalidate: true, class: "panel", id: "password-form", "aria-label": "Password" }, username, passwordMessages, password.el, h("div", { class: "form-actions" }, change));

  passwordEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    passwordMessages.replaceChildren();
    if (!password.validate()) return;
    const values = password.values();
    try {
      await busy(change, () => api.account.changePassword(values.currentPassword, values.newPassword));
    } catch (error) {
      if (!password.showServerErrors(error)) passwordMessages.append(problem(error, "Not changed"));
      return;
    }
    password.set({});
    password.clearErrors();
    toast("Your password is changed.");
  });

  ctx.setDirty(() => profile.isDirty() || password.isDirty());
  ctx.main.append(h("div", { class: "account" }, profileEl, passwordEl));
}
