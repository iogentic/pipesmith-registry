// S02 Overview: counts of this server and links to the places an administrator needs most.

import { h } from "../dom.js";
import { api } from "../api.js";
import { bytes, number, formatName, typeName, time } from "../format.js";
import { pageHead, loading, linkButton } from "../ui/page.js";
import { mark, problem } from "../ui/notice.js";

function stat(label, value, sub) {
  return h("div", { class: "card" }, h("div", { class: "stat-label", text: label }), h("div", { class: "stat", text: value }), sub ? h("p", { text: sub }) : null);
}

// The blob stores of the status answer: files, size and the share of each in the whole.
function storesCard(status, canOpen) {
  const stores = [...status.stores].sort((a, b) => (b.bytes || 0) - (a.bytes || 0));
  const total = stores.reduce((sum, s) => sum + (s.bytes || 0), 0) || 1;
  // A store the server cannot read reports 0 files; the overview says Unknown, as the store list does.
  const unknown = () => h("span", { class: "muted", text: "Unknown" });
  const rows = stores.map((s) => {
    const off = s.available === false;
    const share = Math.round(((s.bytes || 0) / total) * 100);
    const fill = h("span");
    fill.style.width = `${off ? 0 : Math.max(share, 1)}%`;
    return h(
      "tr",
      {},
      h("td", { "data-label": "Store" }, canOpen ? h("a", { href: `#/admin/blob-stores/${encodeURIComponent(s.name)}`, text: s.name }) : s.name),
      h("td", { class: "num", "data-label": "Files" }, off ? unknown() : number(s.files)),
      h("td", { class: "num", "data-label": "Size" }, off ? unknown() : bytes(s.bytes)),
      h("td", { "data-label": "Share" }, h("div", { class: "bar", role: "img", "aria-label": off ? "No share known" : `${share} percent of the stored bytes` }, fill)),
      h("td", { "data-label": "State" }, s.available === false ? mark("error", "Unavailable") : mark("ok", "Available"))
    );
  });
  return h(
    "section",
    { class: "card wide-card", "aria-labelledby": "overview-stores" },
    h("h2", { class: "stat-label", id: "overview-stores", text: "Blob stores" }),
    h(
      "table",
      { class: "data compact" },
      h("thead", {}, h("tr", {}, ["Store", "Files", "Size", "Share", "State"].map((label, i) => h("th", { scope: "col", class: i === 1 || i === 2 ? "num" : null, text: label })))),
      h("tbody", {}, rows)
    )
  );
}

const RESULT = { OK: ["ok", "OK"], FAILED: ["error", "Failed"], CANCELED: ["warn", "Canceled"] };

// Failed and running tasks first, then the next runs; at most four rows.
function tasksCard(items) {
  const rank = (t) => (t.lastRunResult === "FAILED" ? 0 : t.currentState === "RUNNING" ? 1 : 2);
  const next = (t) => (t.nextRun ? Date.parse(t.nextRun) : Infinity);
  const shown = [...items].sort((a, b) => rank(a) - rank(b) || next(a) - next(b)).slice(0, 4);
  const rows = shown.map((t) => {
    const [kind, label] = t.currentState === "RUNNING" ? ["busy", "Running"] : RESULT[t.lastRunResult] || ["off", "Not run yet"];
    return h(
      "tr",
      {},
      h("td", { "data-label": "Task" }, h("a", { href: `#/admin/tasks/${encodeURIComponent(t.id)}`, text: t.name })),
      h("td", { "data-label": "Last result" }, mark(kind, label)),
      h("td", { "data-label": "Last run", class: "num", text: t.lastRun ? time(t.lastRun) : "Never" }),
      h("td", { "data-label": "Next run", class: "num", text: t.nextRun ? time(t.nextRun) : "Not scheduled" })
    );
  });
  return h(
    "section",
    { class: "card full-card", "aria-labelledby": "overview-tasks" },
    h("div", { class: "card-head" }, h("h2", { class: "stat-label", id: "overview-tasks", text: "Tasks" }), h("a", { href: "#/admin/tasks", class: "small", text: `All ${number(items.length)} tasks` })),
    h(
      "table",
      { class: "data compact" },
      h("thead", {}, h("tr", {}, ["Task", "Last result", "Last run", "Next run"].map((label, i) => h("th", { scope: "col", class: i > 1 ? "num" : null, text: label })))),
      h("tbody", {}, rows)
    )
  );
}

// The repository list as two cards: the count with a line per type, and the formats.
function repositoryCards(repos) {
  const byType = {};
  for (const r of repos) byType[r.type] = (byType[r.type] || 0) + 1;
  const typeLine = Object.entries(byType)
    .sort()
    .map(([t, n]) => `${number(n)} ${typeName(t).toLowerCase()}`)
    .join(", ");
  const byFormat = {};
  for (const r of repos) byFormat[r.format] = (byFormat[r.format] || 0) + 1;
  const formatLine = Object.entries(byFormat)
    .sort()
    .map(([f, n]) => `${formatName(f)} ${number(n)}`)
    .join(", ");
  return [stat("Repositories you can see", number(repos.length), typeLine || "None yet"), formatLine ? stat("Formats", number(Object.keys(byFormat).length), formatLine) : null];
}

function scanCard(s) {
  return h(
    "div",
    { class: "card" },
    h("div", { class: "stat-label", text: "Index scan" }),
    h("div", { class: "stat" }, s.scan.running ? mark("busy", "Running") : mark("ok", "Idle")),
    h("p", { text: s.scan.running && s.scan.store ? `Scanning the store ${s.scan.store}` : `Version ${s.version || "unknown"}` })
  );
}

function checksCard(checks) {
  const results = Object.values(checks);
  const failing = results.filter((r) => !r.healthy).length;
  return h(
    "a",
    { class: "card", href: "#/admin/status" },
    h("div", { class: "stat-label", text: "Health checks" }),
    h("div", { class: "stat" }, failing ? mark("error", `${failing} failing`) : mark("ok", "All healthy")),
    h("p", { text: `${number(results.length)} checks` })
  );
}

// Puts a card for each label into the grid that says it is loading. When the request has
// answered, the card at the same index of what cards returns takes its place, and a place
// without a card is removed. A failed request removes its places and hands its error to failed.
function fill(grid, request, labels, cards, failed) {
  const places = labels.map(([label, kind]) =>
    grid.appendChild(h("div", { class: ["card", kind], "aria-busy": "true" }, h("div", { class: "stat-label", text: label }), loading()))
  );
  request.then(cards).then(
    (made) => places.forEach((place, i) => (made[i] ? place.replaceWith(made[i]) : place.remove())),
    (error) => {
      places.forEach((place) => place.remove());
      if (failed) failed(error);
    }
  );
}

export async function render(ctx) {
  const user = ctx.session.user;
  ctx.main.append(pageHead({ title: "Overview", sub: `Signed in as ${user.userId}.` }));

  const links = [];
  links.push(linkButton("Browse repositories", "#/browse", "primary"));
  if (ctx.can("nexus:search:read")) links.push(linkButton("Search", "#/search"));
  if (ctx.can("nexus:component:add")) links.push(linkButton("Upload", "#/upload"));
  if (ctx.can("nexus:repository-admin:*:*:read")) links.push(linkButton("Repositories", "#/admin/repositories"));
  if (ctx.can("nexus:tasks:read")) links.push(linkButton("Tasks", "#/admin/tasks"));
  const grid = h("div", { class: "cards overview-grid" });
  ctx.main.append(h("div", { class: "stack" }, grid, h("section", { class: "section" }, h("h2", { text: "Shortcuts" }), h("div", { class: "actions" }, links))));

  // Each card stands as soon as its own request has answered, in the order of the cards.
  // The read only state shows in the band of the frame on every screen, so the overview does not repeat it.
  const status = api.status();
  fill(grid, api.repositories.list(), [["Repositories you can see"], ["Formats"]], (repos) => repositoryCards(repos || []),
    (error) => grid.before(problem(error, "Repositories could not be loaded")));
  fill(grid, status, [["Files served"], ["Index scan"]], (s) => [
    s && s.totals ? stat("Files served", number(s.totals.files), `${bytes(s.totals.bytes)} in ${number(s.totals.stores)} blob stores`) : null,
    s && s.scan ? scanCard(s) : null
  ]);
  if (ctx.can("nexus:atlas:read")) fill(grid, api.statusChecks(), [["Health checks"]], (checks) => [checks ? checksCard(checks) : null]);
  fill(grid, status, [["Blob stores", "wide-card"]], (s) => [
    s && Array.isArray(s.stores) && s.stores.length ? storesCard(s, ctx.can("nexus:blobstores:read")) : null
  ]);
  if (ctx.can("nexus:tasks:read")) {
    fill(grid, api.tasks.list(), [["Tasks", "full-card"]], (tasks) => [
      tasks && Array.isArray(tasks.items) && tasks.items.length ? tasksCard(tasks.items) : null
    ]);
  }
}
