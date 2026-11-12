// S43 Metrics: requests and downloads of the last 7, 30 or 90 days, a table per day with a bar per
// row, the downloads per repository, and a summary in words. docs/ui/API.md, S43.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { bytes, number } from "../../format.js";
import { pageHead, loading, emptyState } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { notice, errorText, problem } from "../../ui/notice.js";
import { barCell } from "../storage/common.js";

const RANGES = [7, 30, 90];
const DAY_MS = 86400000;

function isoDay(ms) {
  return new Date(ms).toISOString().slice(0, 10);
}

// The last `days` days in UTC, today included.
function range(days) {
  const today = Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth(), new Date().getUTCDate());
  return { from: isoDay(today - (days - 1) * DAY_MS), to: isoDay(today) };
}

function weekday(day) {
  return new Date(`${day}T00:00:00Z`).toLocaleDateString("en-US", { weekday: "short", timeZone: "UTC" });
}

function stat(label, value, sub) {
  return h("div", { class: "card" }, h("div", { class: "stat-label", text: label }), h("div", { class: "stat", text: value }), sub ? h("p", { text: sub }) : null);
}

function dayTable(days) {
  const max = Math.max(1, ...days.map((d) => d.requests || 0));
  const rows = [...days].sort((a, b) => (a.day < b.day ? 1 : -1));
  return h(
    "div",
    { class: "table-wrap" },
    h(
      "table",
      { class: "day-table" },
      h("caption", { class: "visually-hidden", text: "Requests and bytes sent per day, newest first" }),
      h(
        "thead",
        {},
        h("tr", {}, h("th", { scope: "col", text: "Day" }), h("th", { scope: "col", text: "Requests" }), h("th", { scope: "col", class: "num", text: "Sent" }))
      ),
      h(
        "tbody",
        {},
        rows.map((d) =>
          h(
            "tr",
            {},
            h("th", { scope: "row", class: "day" }, h("span", { class: "muted small weekday", text: weekday(d.day) }), " ", h("span", { class: "num", text: d.day })),
            h("td", {}, barCell(number(d.requests), (d.requests || 0) / max)),
            h("td", { class: "num", text: bytes(d.bytesOut) })
          )
        )
      )
    )
  );
}

function summaryText(days, requests, downloads) {
  const total = requests.reduce((n, d) => n + (d.requests || 0), 0);
  const sent = requests.reduce((n, d) => n + (d.bytesOut || 0), 0);
  const busiest = requests.reduce((best, d) => (!best || d.requests > best.requests ? d : best), null);
  const parts = [`In the last ${days} days the server answered ${number(total)} requests and sent ${bytes(sent) || "0 B"}.`];
  if (busiest && busiest.requests) parts.push(`The busiest day was ${busiest.day} with ${number(busiest.requests)} requests.`);
  const all = downloads.reduce((n, r) => n + r.count, 0);
  if (all) {
    parts.push(`Components were downloaded ${number(all)} times, most often from ${downloads[0].repository} with ${number(downloads[0].count)}.`);
  } else {
    parts.push("No component was downloaded.");
  }
  return parts.join(" ");
}

export async function render(ctx) {
  ctx.main.classList.add("area-storage");
  let days = RANGES.includes(Number(ctx.query.days)) ? Number(ctx.query.days) : 7;
  ctx.main.append(pageHead({ title: "Metrics", sub: "Requests and downloads this server counted per day. Days are in UTC." }));

  const buttons = RANGES.map((n) => h("button", { type: "button", class: "btn small", dataset: { days: n }, "aria-pressed": String(n === days), text: `${n} days` }));
  const picker = h("div", { class: "range", role: "group", "aria-label": "Range" }, h("span", { class: "muted small", text: "Range" }), buttons);
  const summary = h("p", { id: "metrics-summary", class: "metrics-summary", "aria-live": "polite" });
  const body = h("div", { class: "stack" });
  ctx.main.append(picker, summary, body);

  let run = 0;
  async function load() {
    const mine = ++run;
    buttons.forEach((b) => b.setAttribute("aria-pressed", String(Number(b.dataset.days) === days)));
    const { from, to } = range(days);
    summary.textContent = "";
    body.replaceChildren(loading(`Loading the last ${days} days`));
    let requests;
    let downloads;
    try {
      [requests, downloads] = await Promise.all([api.metrics.requests(from, to), api.metrics.downloads(from, to)]);
    } catch (error) {
      if (mine !== run) return;
      const retry = h("button", { type: "button", class: "btn", text: "Try again", on: { click: load } });
      body.replaceChildren(problem(error, "Metrics could not be loaded"), h("div", { class: "actions" }, retry));
      return;
    }
    if (mine !== run) return;
    const perDay = requests.days || [];
    const byRepo = {};
    for (const row of downloads.rows || []) byRepo[row.repository] = (byRepo[row.repository] || 0) + row.count;
    const repoRows = Object.entries(byRepo)
      .map(([repository, count]) => ({ repository, count }))
      .sort((a, b) => b.count - a.count || a.repository.localeCompare(b.repository));
    const totalRequests = perDay.reduce((n, d) => n + (d.requests || 0), 0);
    const totalDownloads = repoRows.reduce((n, r) => n + r.count, 0);

    summary.textContent = summaryText(days, perDay, repoRows);
    if (!totalRequests && !totalDownloads) {
      body.replaceChildren(h("div", { class: "table-wrap" }, emptyState({ title: "Nothing counted", text: `The server counted no request in the last ${days} days.` })));
      return;
    }
    const sent = perDay.reduce((n, d) => n + (d.bytesOut || 0), 0);
    const maxRepo = Math.max(1, ...repoRows.map((r) => r.count));
    const repoTable = dataTable(
      [
        { key: "repository", label: "Repository", class: "break" },
        { key: "count", label: "Downloads", render: (r) => barCell(number(r.count), r.count / maxRepo) },
        { key: "share", label: "Share", num: true, value: (r) => r.count, render: (r) => `${((r.count / totalDownloads) * 100).toFixed(1)} %` }
      ],
      { rows: repoRows, filter: false, caption: `Downloads per repository in the last ${days} days`, empty: { title: "No download", text: "No component was downloaded in this range." } }
    );
    body.replaceChildren(
      h(
        "div",
        { class: "cards metric-cards" },
        stat("Requests", number(totalRequests), `${number(Math.round(totalRequests / Math.max(1, perDay.length)))} a day on average`),
        stat("Sent", bytes(sent) || "0 B", `${number(perDay.length)} days from ${requests.from || from} to ${requests.to || to}`),
        stat("Downloads", number(totalDownloads), `${number(repoRows.length)} repositories`)
      ),
      h("section", { class: "section", "aria-labelledby": "metrics-days-title" }, h("h2", { id: "metrics-days-title", text: "Requests per day" }), dayTable(perDay)),
      h("section", { class: "section", id: "downloads-table", "aria-labelledby": "metrics-repos-title" }, h("h2", { id: "metrics-repos-title", text: "Downloads per repository" }), repoTable.el)
    );
  }

  buttons.forEach((b) =>
    b.addEventListener("click", () => {
      const next = Number(b.dataset.days);
      if (next === days && b.getAttribute("aria-pressed") === "true" && !body.querySelector(".notice")) return;
      days = next;
      load();
    })
  );
  await load();
}

