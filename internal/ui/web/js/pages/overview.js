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

export async function render(ctx) {
  const user = ctx.session.user;
  ctx.main.append(pageHead({ title: "Overview", sub: `Signed in as ${user.userId}.` }));
  const body = h("div", { class: "stack" }, loading());
  ctx.main.append(body);

  // The read only state shows in the band of the frame on every screen, so the overview does not repeat it.
  const [status, repositories, checks, tasks] = await Promise.allSettled([
    api.status(),
    api.repositories.list(),
    ctx.can("nexus:atlas:read") ? api.statusChecks() : Promise.resolve(null),
    ctx.can("nexus:tasks:read") ? api.tasks.list() : Promise.resolve(null)
  ]);

  const parts = [];

  const cards = [];
  if (repositories.status === "fulfilled") {
    const repos = repositories.value || [];
    const byType = {};
    for (const r of repos) byType[r.type] = (byType[r.type] || 0) + 1;
    const typeLine = Object.entries(byType)
      .sort()
      .map(([t, n]) => `${number(n)} ${typeName(t).toLowerCase()}`)
      .join(", ");
    cards.push(stat("Repositories you can see", number(repos.length), typeLine || "None yet"));
    const byFormat = {};
    for (const r of repos) byFormat[r.format] = (byFormat[r.format] || 0) + 1;
    const formatLine = Object.entries(byFormat)
      .sort()
      .map(([f, n]) => `${formatName(f)} ${number(n)}`)
      .join(", ");
    if (formatLine) cards.push(stat("Formats", number(Object.keys(byFormat).length), formatLine));
  } else {
    parts.push(problem(repositories.reason, "Repositories could not be loaded"));
  }
  if (status.status === "fulfilled" && status.value) {
    const s = status.value;
    if (s.totals) {
      cards.push(stat("Files served", number(s.totals.files), `${bytes(s.totals.bytes)} in ${number(s.totals.stores)} blob stores`));
    }
    if (s.scan) {
      cards.push(
        h(
          "div",
          { class: "card" },
          h("div", { class: "stat-label", text: "Index scan" }),
          h("div", { class: "stat" }, s.scan.running ? mark("busy", "Running") : mark("ok", "Idle")),
          h("p", { text: s.scan.running && s.scan.store ? `Scanning the store ${s.scan.store}` : `Version ${s.version || "unknown"}` })
        )
      );
    }
  }
  if (checks.status === "fulfilled" && checks.value) {
    const results = Object.values(checks.value);
    const failing = results.filter((r) => !r.healthy).length;
    cards.push(
      h(
        "a",
        { class: "card", href: "#/admin/status" },
        h("div", { class: "stat-label", text: "Health checks" }),
        h("div", { class: "stat" }, failing ? mark("error", `${failing} failing`) : mark("ok", "All healthy")),
        h("p", { text: `${number(results.length)} checks` })
      )
    );
  }
  if (status.status === "fulfilled" && status.value && Array.isArray(status.value.stores) && status.value.stores.length) {
    cards.push(storesCard(status.value, ctx.can("nexus:blobstores:read")));
  }
  if (tasks.status === "fulfilled" && tasks.value && Array.isArray(tasks.value.items) && tasks.value.items.length) {
    cards.push(tasksCard(tasks.value.items));
  }
  parts.push(h("div", { class: "cards overview-grid" }, cards));

  const links = [];
  links.push(linkButton("Browse repositories", "#/browse", "primary"));
  if (ctx.can("nexus:search:read")) links.push(linkButton("Search", "#/search"));
  if (ctx.can("nexus:component:add")) links.push(linkButton("Upload", "#/upload"));
  if (ctx.can("nexus:repository-admin:*:*:read")) links.push(linkButton("Repositories", "#/admin/repositories"));
  if (ctx.can("nexus:tasks:read")) links.push(linkButton("Tasks", "#/admin/tasks"));
  parts.push(h("section", { class: "section" }, h("h2", { text: "Shortcuts" }), h("div", { class: "actions" }, links)));

  body.replaceChildren(...parts);
}
