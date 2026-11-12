// The frame: session, top bar, navigation, routes, the band for example data, the theme choice.

import { h, replace } from "./dom.js";
import { api, on, setCsrfToken, setRefused } from "./api.js";
import { ROUTES, SECTIONS, match } from "./routes.js";
import { permitted } from "./perm.js";
import { loading, noAccess, notFound, failure, previewView, readOnlyView } from "./ui/page.js";
import * as preview from "./preview.js";
import { toast } from "./ui/notice.js";
import { confirm, openDialog } from "./ui/dialog.js";

const app = document.getElementById("app");
preview.setRoutes(ROUTES);
const state = { session: null, exampleData: false, readOnly: null, writesRefused: false, page: null, dirty: null, lastHash: null, status: null };

// The actions of a permission that change something on the server.
const WRITE_ACTIONS = new Set(["add", "create", "edit", "update", "delete", "start", "stop"]);

// The text of a read only notice while the server takes no changes. The band of the frame gives the
// reason, so a notice on the page does not repeat it.
const SERVER_TAKES_NONE = "The server takes no changes at the moment, so this page shows what is stored.";

function isWrite(perm) {
  return [].concat(perm || []).some((p) => WRITE_ACTIONS.has(String(p).toLowerCase().split(":").pop()));
}

// Why the server takes no changes now, or null while it takes them: it reports itself read only, or
// it answered a change with 501 since this page was loaded.
function writeBlock() {
  if (state.readOnly && state.readOnly.frozen) return state.readOnly.summaryReason || "The server is read only until it is released.";
  if (state.writesRefused) return "This server takes no changes yet. They come with a later step of the server.";
  return null;
}

// The roles of the user allow this: the rule for navigation and for opening a screen.
function allowed(perm) {
  return Boolean(state.session) && permitted(state.session.permissions, perm);
}

// The roles allow it and the server takes it now: the rule for every control on a screen. A screen
// hides a change the server would refuse, as it hides a change the roles do not allow.
function can(perm) {
  return allowed(perm) && (!isWrite(perm) || !writeBlock());
}

function parseHash() {
  const raw = location.hash.replace(/^#/, "") || "/overview";
  const [path, q = ""] = raw.split("?");
  return { path: path || "/overview", query: Object.fromEntries(new URLSearchParams(q)), raw };
}

export function navigate(href, { replace: replaceEntry = false } = {}) {
  const target = href.startsWith("#") ? href : `#${href}`;
  if (replaceEntry) {
    history.replaceState(null, "", target);
    route();
  } else {
    location.hash = target.slice(1);
  }
}

// Theme

function themeToggle() {
  const button = h("button", { type: "button", class: "btn quiet small pill", id: "theme-toggle" });
  const label = () => {
    const other = window.pipesmithTheme.current() === "dark" ? "light" : "dark";
    button.textContent = other === "dark" ? "Dark" : "Light";
    button.setAttribute("aria-label", `Switch to the ${other} theme`);
  };
  button.addEventListener("click", () => {
    window.pipesmithTheme.pick(window.pipesmithTheme.current() === "dark" ? "light" : "dark");
    document.querySelectorAll("#theme-toggle").forEach((b) => b.dispatchEvent(new Event("relabel")));
    const chooser = document.getElementById("theme-chooser");
    if (chooser) chooser.remove();
    document.body.classList.remove("has-chooser");
  });
  button.addEventListener("relabel", label);
  label();
  return button;
}

function themeChooser() {
  if (window.pipesmithTheme.stored() || document.getElementById("theme-chooser")) return;
  const pick = (theme) => {
    window.pipesmithTheme.pick(theme);
    document.querySelectorAll("#theme-toggle").forEach((b) => b.dispatchEvent(new Event("relabel")));
    chooser.remove();
    document.body.classList.remove("has-chooser");
  };
  const light = h("button", { type: "button", class: "btn small pill", text: "Light", dataset: { pick: "light" } });
  const dark = h("button", { type: "button", class: "btn small pill", text: "Dark", dataset: { pick: "dark" } });
  light.addEventListener("click", () => pick("light"));
  dark.addEventListener("click", () => pick("dark"));
  const chooser = h("div", { class: "chooser", id: "theme-chooser", role: "region", "aria-label": "Theme" }, h("span", { text: "Pick a theme" }), light, dark);
  document.body.append(chooser);
  document.body.classList.add("has-chooser");
}

// Frame parts

function brand() {
  return h(
    "a",
    { class: "brand", href: "#/overview" },
    h("img", { src: "assets/symbol-64.png", alt: "", width: "28", height: "28" }),
    h("span", { class: "name", text: "Pipesmith" }),
    h("span", { class: "product", text: "Registry" })
  );
}

function navGroups(current) {
  const groups = [];
  for (const section of SECTIONS) {
    const items = ROUTES.filter((r) => r.section === section.id && r.nav && allowed(r.perm));
    if (!items.length) continue;
    const head = section.landing ? h("a", { class: "nav-head", href: `#${section.landing}`, text: section.title }) : h("span", { class: "nav-head", text: section.title });
    groups.push(
      h(
        "div",
        { class: "nav-group" },
        head,
        h(
          "ul",
          { class: "nav-list" },
          items.map((r) => {
            const active = current && (current.id === r.id || current.parent === r.id);
            const later = preview.unavailable(r.id);
            return h(
              "li",
              {},
              h("a", { href: `#${r.path}`, "aria-current": active ? "page" : null, class: later && "later" }, r.nav, later ? h("span", { class: "nav-note", text: "Not in preview" }) : null)
            );
          })
        )
      )
    );
  }
  return groups;
}

function openDrawer(current) {
  const close = h("button", { type: "button", class: "btn small", text: "Close" });
  const search = topSearch("drawer-search");
  const drawer = h(
    "dialog",
    { class: "drawer", "aria-label": "Navigation" },
    h("div", { class: "drawer-head" }, brand(), close),
    allowed("nexus:search:read") ? h("div", { class: "field" }, search) : null,
    h("nav", { "aria-label": "Main" }, navGroups(current))
  );
  close.addEventListener("click", () => drawer.close());
  drawer.addEventListener("click", (event) => {
    if (event.target.closest("a")) drawer.close();
  });
  drawer.addEventListener("close", () => drawer.remove());
  document.body.append(drawer);
  drawer.showModal();
}

function topSearch(id = "top-search") {
  const input = h("input", { class: "input", id, type: "search", name: "q", placeholder: "Search components", autocomplete: "off" });
  const formEl = h("form", { role: "search", class: id === "top-search" ? "top-search" : null }, h("label", { class: "visually-hidden", for: id, text: "Search components" }), input);
  formEl.addEventListener("submit", (event) => {
    event.preventDefault();
    const q = input.value.trim();
    navigate(`/search${q ? `?q=${encodeURIComponent(q)}` : ""}`);
    input.value = "";
  });
  return formEl;
}

function userMenu() {
  const user = state.session.user;
  const button = h("button", { type: "button", class: "btn quiet small", "aria-haspopup": "true", "aria-expanded": "false", id: "user-button" }, h("span", { class: "break", text: user.userId }));
  const menu = h(
    "div",
    { class: "menu", hidden: true, role: "menu" },
    h("div", { class: "who" }, h("div", { class: "id", text: [user.firstName, user.lastName].filter(Boolean).join(" ") || user.userId }), h("div", { class: "muted small break", text: user.emailAddress || user.userId })),
    h("a", { href: "#/account", role: "menuitem", text: "Account" }),
    allowed("nexus:apikey:*") ? h("a", { href: "#/tokens", role: "menuitem", text: "Tokens" }) : null,
    h("button", { type: "button", role: "menuitem", text: "About", dataset: { menu: "about" } }),
    h("button", { type: "button", role: "menuitem", text: "Sign out", dataset: { menu: "signout" } })
  );
  const wrap = h("div", { class: "menu-wrap" }, button, menu);
  const setOpen = (open) => {
    menu.hidden = !open;
    button.setAttribute("aria-expanded", String(open));
    if (open) menu.querySelector("a, button").focus();
  };
  button.addEventListener("click", () => setOpen(menu.hidden));
  menu.addEventListener("click", (event) => {
    const item = event.target.closest("[data-menu], a");
    if (!item) return;
    setOpen(false);
    if (item.dataset.menu === "about") about();
    if (item.dataset.menu === "signout") signOut();
  });
  menu.addEventListener("keydown", (event) => {
    const items = [...menu.querySelectorAll("a, button")];
    const i = items.indexOf(document.activeElement);
    if (event.key === "Escape") {
      setOpen(false);
      button.focus();
    } else if (event.key === "ArrowDown") {
      event.preventDefault();
      items[(i + 1) % items.length].focus();
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      items[(i - 1 + items.length) % items.length].focus();
    }
  });
  return wrap;
}

async function about() {
  let status = state.status;
  try {
    status = await api.status();
    state.status = status;
  } catch (e) {
    /* the dialog still shows what it has */
  }
  const name = (status && status.name) || "Pipesmith Registry";
  await openDialog({
    title: "About",
    body: h(
      "div",
      { class: "stack" },
      h("p", {}, h("strong", { text: name }), status && status.version ? ` version ${status.version}` : ""),
      h("p", { class: "muted", text: "An artifact repository server for Maven, npm, Docker and raw files. It reads existing blob stores in place and serves them under the same addresses." }),
      h("p", { class: "muted", text: "Licensed under the Apache License 2.0." })
    ),
    actions: [{ label: "Close", value: "close", kind: "primary" }]
  });
}

async function signOut() {
  try {
    await api.session.signOut();
  } catch (e) {
    /* the session is gone either way */
  }
  state.session = null;
  setCsrfToken(null);
  state.readOnly = null;
  setRefused([]);
  preview.reset();
  navigate("/sign-in", { replace: true });
  toast("You are signed out.", "info");
}

function bands() {
  const parts = [];
  // The node page shows the read only state with its reason and the release itself.
  const nodeShowsState = Boolean(state.route && state.route.id === "S37" && !preview.unavailable("S37"));
  if (state.exampleData) {
    parts.push(
      h(
        "div",
        { class: "band example", role: "note", id: "example-band" },
        h("strong", { text: "Example data" }),
        h("span", { text: "This interface runs against a mock. Every repository, user and number on these pages is invented." })
      )
    );
  }
  if (state.readOnly && state.readOnly.frozen) {
    if (!nodeShowsState) parts.push(h("div", { class: "band readonly", role: "status" }, h("strong", { text: "Read only" }), h("span", { text: state.readOnly.summaryReason || "The server accepts no changes until it is released." })));
  } else if (state.writesRefused) {
    parts.push(h("div", { class: "band readonly", role: "status", id: "no-changes-band" }, h("strong", { text: "No changes" }), h("span", { text: writeBlock() })));
  }
  return parts;
}

function frame(current) {
  const menuButton = h("button", { type: "button", class: "btn small menu-toggle", text: "Menu", "aria-label": "Open the navigation" });
  menuButton.addEventListener("click", () => openDrawer(current));
  const main = h("main", { id: "main", tabindex: "-1" });
  const skip = h("a", { class: "skip", href: "#main", text: "Skip to the content" });
  skip.addEventListener("click", (event) => {
    event.preventDefault();
    main.focus();
  });
  const top = h(
    "header",
    { class: "top" },
    menuButton,
    brand(),
    allowed("nexus:search:read") ? topSearch() : null,
    h("div", { class: "top-end" }, themeToggle(), userMenu())
  );
  const shell = h(
    "div",
    { class: "frame" },
    skip,
    top,
    h("div", { class: "bands" }, bands()),
    h("div", { class: "body" }, h("aside", { class: "side" }, h("nav", { "aria-label": "Main" }, navGroups(current))), main)
  );
  replace(app, shell);
  return main;
}

function redrawNav() {
  const nav = app.querySelector("aside.side nav");
  if (nav) replace(nav, navGroups(state.route));
}

preview.onChange(redrawNav);

async function runProbes() {
  if (state.session) await preview.probe(ROUTES, allowed);
}

function redrawBands() {
  const holder = app.querySelector(".bands");
  if (holder) replace(holder, bands());
}

// One listener for every menu: a click outside an open menu closes it.
document.addEventListener("click", (event) => {
  for (const wrap of document.querySelectorAll(".menu-wrap")) {
    const menu = wrap.querySelector(".menu");
    if (menu && !menu.hidden && !wrap.contains(event.target)) {
      menu.hidden = true;
      wrap.querySelector("[aria-expanded]").setAttribute("aria-expanded", "false");
    }
  }
});

on("writesRefused", () => {
  if (state.writesRefused) return;
  state.writesRefused = true;
  redrawBands();
});

on("exampleData", () => {
  if (state.exampleData) return;
  state.exampleData = true;
  redrawBands();
});

on("unauthorized", () => {
  if (!state.session) return;
  state.session = null;
  const { raw } = parseHash();
  navigate(`/sign-in?next=${encodeURIComponent(raw)}`, { replace: true });
  toast("Your session has ended. Sign in again.", "info");
});

async function loadSession() {
  try {
    const session = await api.session.get();
    state.session = session && session.user ? session : null;
    setCsrfToken(state.session ? session.csrfToken : null);
  } catch (e) {
    state.session = null;
  }
}

// What the server serves: GET /api/areas names the requests it refuses and its read only state for
// every signed in user (docs/ui/API.md, section Areas). Without the answer the probes of preview.js
// find the same out.
async function loadAreas() {
  setRefused([]);
  if (!state.session) return;
  let answer = null;
  try {
    answer = await api.areas();
  } catch (e) {
    return;
  }
  if (!answer || typeof answer !== "object") return;
  setRefused(answer.refused);
  const ro = answer.readOnly;
  if (ro && typeof ro === "object") {
    state.readOnly = { frozen: Boolean(ro.frozen), summaryReason: typeof ro.reason === "string" ? ro.reason : "" };
    redrawBands();
  }
}

async function loadReadOnly() {
  if (!allowed("nexus:settings:read")) return;
  try {
    state.readOnly = await api.readOnly.get();
    redrawBands();
  } catch (e) {
    // keep what the areas answer said, if anything
  }
}

function context(main, current, params, query) {
  return {
    main,
    route: current,
    params,
    query,
    session: state.session,
    can,
    allowed,
    writeBlock,
    // True while the server is known to be read only, from the areas answer or the read only state.
    frozen: () => Boolean(state.readOnly && state.readOnly.frozen),
    // The text of a read only notice: one line while the server takes no changes (the band gives the
    // reason), else the text about the roles.
    readOnlyText: (rolesText) => (writeBlock() ? SERVER_TAKES_NONE : rolesText),
    navigate,
    setDirty(fn) {
      state.dirty = fn;
    },
    setTitle(title) {
      document.title = title ? `${title} | Pipesmith Registry` : "Pipesmith Registry";
    },
    async signedIn(session) {
      state.session = session;
      setCsrfToken(session.csrfToken);
      await loadAreas();
      await loadReadOnly();
      await runProbes();
    },
    refreshReadOnly: loadReadOnly
  };
}

let routing = 0;

async function route() {
  const { path, query, raw } = parseHash();
  if (state.dirty && state.lastHash !== null && raw !== state.lastHash) {
    const isDirty = state.dirty();
    if (isDirty) {
      const leave = await confirm({ title: "Leave this page?", text: "Your changes on this page are not saved. They are lost when you leave.", confirmLabel: "Leave", danger: true });
      if (!leave) {
        history.replaceState(null, "", `#${state.lastHash}`);
        return;
      }
    }
  }
  state.dirty = null;
  state.lastHash = raw;
  const run = ++routing;
  if (state.page && state.page.dispose) state.page.dispose();
  state.page = null;

  const found = match(path);
  const current = found ? found.route : null;
  state.route = current;
  preview.setCurrent(current ? current.id : null);

  if (!state.session && !(current && current.public)) {
    navigate(`/sign-in?next=${encodeURIComponent(raw)}`, { replace: true });
    return;
  }
  if (current && current.public) {
    if (state.session && current.id === "S01") {
      navigate(query.next || "/overview", { replace: true });
      return;
    }
    const module = await import(current.module);
    if (run !== routing) return;
    replace(app);
    const holder = h("div");
    app.append(holder);
    const ctx = context(holder, current, found.params, query);
    ctx.setTitle(current.title);
    const page = (await module.render(ctx)) || null;
    if (run !== routing) {
      if (page && page.dispose) page.dispose();
      return;
    }
    state.page = page;
    themeChooser();
    return;
  }

  const main = frame(current);
  themeChooser();
  if (!found) {
    replace(main, notFound("page"));
    document.title = "Not found | Pipesmith Registry";
    main.querySelector("h1").focus();
    return;
  }
  if (!allowed(current.perm)) {
    replace(main, noAccess());
    document.title = "No access | Pipesmith Registry";
    main.querySelector("h1").focus();
    return;
  }
  if (preview.unavailable(current.id)) {
    replace(main, previewView());
    document.title = `${current.title} | Pipesmith Registry`;
    main.querySelector("h1").focus();
    return;
  }
  if (!can(current.perm)) {
    replace(main, readOnlyView(writeBlock()));
    document.title = `${current.title} | Pipesmith Registry`;
    main.querySelector("h1").focus();
    return;
  }
  replace(main, loading());
  const ctx = context(main, current, found.params, query);
  ctx.setTitle(current.title);
  try {
    const module = await import(current.module);
    if (run !== routing) return;
    const content = h("div", { class: "content" });
    replace(main, content);
    ctx.main = content;
    const page = (await module.render(ctx)) || null;
    if (run !== routing) {
      if (page && page.dispose) page.dispose();
      return;
    }
    state.page = page;
  } catch (error) {
    if (run !== routing) return;
    failure(main, error, { retry: () => route() });
  }
  if (run !== routing) return;
  const heading = main.querySelector("h1");
  if (heading) heading.focus({ preventScroll: true });
}

window.addEventListener("hashchange", route);

(async function start() {
  await loadSession();
  await loadAreas();
  await loadReadOnly();
  await runProbes();
  route();
})();
