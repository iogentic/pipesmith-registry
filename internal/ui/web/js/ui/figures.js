// The figure cards of Overview (task 7.23 of the phase 7 plan): requests, data sent, CPU and memory
// as line charts of GET /api/figures with a range switch and the value under the pointer, and the
// most used repositories of GET /api/metrics/downloads as a bar list. The charts are drawn as SVG
// here; their colours are the tokens of css/tokens.css through css/areas/figures.css.

import { h } from "../dom.js";
import { api } from "../api.js";
import { bytes, number, time } from "../format.js";

// the SVG namespace, read from the XHTML namespace the HTML parser gives the page, so this file names
// no host and holds no markup sink
const SVG = document.documentElement.namespaceURI.replace("1999/xhtml", "2000/svg");
const RANGES = [
  ["24h", "24 hours"],
  ["7d", "7 days"],
  ["30d", "30 days"]
];
const DAY_MS = 86400000;
const WIDTH = 600;
const HEIGHT = 120;

function svg(tag, attrs) {
  const el = document.createElementNS(SVG, tag);
  for (const [key, value] of Object.entries(attrs || {})) el.setAttribute(key, String(value));
  return el;
}

// The series of each chart: its label, the value of a point and how a value reads.
function charts(stepSeconds) {
  const minutes = Math.round(stepSeconds / 60);
  const per = minutes >= 60 ? `${minutes / 60} hours` : `${minutes} minutes`;
  return [
    { key: "requests", label: "Requests", unit: `per ${per}`, value: (p) => p.requests, text: (v) => number(v) },
    { key: "sent", label: "Data sent", unit: `per ${per}`, value: (p) => p.bytesSent, text: (v) => bytes(v) },
    { key: "cpu", label: "CPU", unit: "percent of one core", value: (p) => (p.cpuSeconds / stepSeconds) * 100, text: (v) => `${v.toFixed(1)} %` },
    { key: "memory", label: "Memory", unit: "highest in each step", value: (p) => p.memoryBytes, text: (v) => bytes(v) }
  ];
}

// One line chart: a polyline over the span from to to, a marker and a readout under the pointer.
function lineChart(series, chart, from, to) {
  const start = Date.parse(from);
  const span = Math.max(Date.parse(to) - start, 1);
  const values = series.map(chart.value);
  const top = Math.max(1e-9, ...values);
  const xy = series.map((p, i) => [((Date.parse(p.time) - start) / span) * WIDTH, HEIGHT - 4 - (values[i] / top) * (HEIGHT - 8)]);
  const plot = svg("svg", { class: "figure-plot", viewBox: `0 0 ${WIDTH} ${HEIGHT}`, preserveAspectRatio: "none", role: "img", "aria-label": `${chart.label} ${chart.unit}, highest ${chart.text(top)}` });
  plot.append(svg("line", { class: "figure-base", x1: 0, y1: HEIGHT - 4, x2: WIDTH, y2: HEIGHT - 4, "vector-effect": "non-scaling-stroke" }));
  plot.append(svg("polyline", { class: "figure-line", points: xy.map(([x, y]) => `${x.toFixed(1)},${y.toFixed(1)}`).join(" "), "vector-effect": "non-scaling-stroke" }));
  // the newest point as a dot: a line of no length with round caps keeps its shape when the plot stretches
  const [lx, ly] = xy[xy.length - 1];
  plot.append(svg("line", { class: "figure-last", x1: lx.toFixed(1), y1: ly.toFixed(1), x2: lx.toFixed(1), y2: ly.toFixed(1), "vector-effect": "non-scaling-stroke" }));
  const cursor = svg("line", { class: "figure-cursor", x1: 0, y1: 0, x2: 0, y2: HEIGHT, "vector-effect": "non-scaling-stroke", visibility: "hidden" });
  plot.append(cursor);
  const readout = h("p", { class: "figure-readout muted small", "aria-live": "polite", text: `Highest ${chart.text(top)}` });
  plot.addEventListener("pointermove", (event) => {
    const box = plot.getBoundingClientRect();
    const x = ((event.clientX - box.left) / Math.max(box.width, 1)) * WIDTH;
    let best = 0;
    xy.forEach(([px], i) => {
      if (Math.abs(px - x) < Math.abs(xy[best][0] - x)) best = i;
    });
    cursor.setAttribute("x1", xy[best][0].toFixed(1));
    cursor.setAttribute("x2", xy[best][0].toFixed(1));
    cursor.setAttribute("visibility", "visible");
    readout.textContent = `${time(series[best].time)}: ${chart.text(values[best])}`;
  });
  plot.addEventListener("pointerleave", () => {
    cursor.setAttribute("visibility", "hidden");
    readout.textContent = `Highest ${chart.text(top)}`;
  });
  return { plot, readout };
}

function chartCard(answer, chart) {
  const points = answer.points || [];
  const total = points.reduce((sum, p) => sum + chart.value(p), 0);
  const last = points.length ? chart.value(points[points.length - 1]) : 0;
  const shown = chart.key === "requests" || chart.key === "sent" ? chart.text(total) : chart.text(last);
  const sub = chart.key === "requests" || chart.key === "sent" ? "in the range" : "in the last step";
  const card = h("section", { class: "card figure-card", dataset: { figure: chart.key } }, h("div", { class: "stat-label", text: chart.label }), h("div", { class: "stat", text: shown }), h("p", { class: "muted small", text: `${sub}; ${chart.unit}` }));
  if (!points.length) {
    card.append(h("p", { class: "figure-empty muted", text: "No samples in this range yet. The server takes one each minute." }));
    return card;
  }
  const { plot, readout } = lineChart(points, chart, answer.from, answer.to);
  card.append(plot, readout);
  return card;
}

// The most used repositories over the days of the range, at least 7.
function repositoriesCard(rows, days) {
  const sums = {};
  for (const r of rows || []) sums[r.repository] = (sums[r.repository] || 0) + (r.count || 0);
  const top = Object.entries(sums)
    .sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : 1))
    .slice(0, 8);
  const most = top.length ? top[0][1] : 1;
  const list = top.map(([name, count]) => {
    const fill = h("span");
    fill.style.width = `${Math.max(Math.round((count / most) * 100), 1)}%`;
    return h("li", {}, h("span", { class: "figure-name", text: name }), h("span", { class: "num", text: number(count) }), h("div", { class: "bar", role: "img", "aria-label": `${number(count)} downloads` }, fill));
  });
  return h(
    "section",
    { class: "card wide-card figure-card", dataset: { figure: "repositories" } },
    h("div", { class: "stat-label", text: "Most used repositories" }),
    h("div", { class: "stat", text: top.length ? number(Object.values(sums).reduce((a, b) => a + b, 0)) : "None" }),
    h("p", { class: "muted small", text: `downloads in the last ${days} days` }),
    top.length ? h("ol", { class: "figure-bars" }, list) : h("p", { class: "figure-empty muted", text: "No downloads in these days." })
  );
}

function isoDay(ms) {
  return new Date(ms).toISOString().slice(0, 10);
}

// Draws the figure cards after the cards before them in the grid, and the range switch above the
// grid. A figure off (enabled false) shows one card that says so.
export function figureCards(grid) {
  const buttons = RANGES.map(([key, label]) => h("button", { type: "button", class: "btn small", dataset: { range: key }, "aria-pressed": String(key === "24h"), text: label }));
  const picker = h("div", { class: "range figure-range", role: "group", "aria-label": "Range of the figures" }, h("span", { class: "muted small", text: "Figures" }), buttons);
  const note = h("p", { class: "muted small figure-note", text: "Data sent and received count the requests of this server. CPU and memory are those of the server process." });
  const own = h("div", { class: "figure-cards" });
  grid.after(h("section", { class: "section figures", "aria-label": "Figures" }, picker, note, own));
  let run = 0;
  async function load(range) {
    const mine = ++run;
    buttons.forEach((b) => b.setAttribute("aria-pressed", String(b.dataset.range === range)));
    own.setAttribute("aria-busy", "true");
    const days = range === "30d" ? 30 : 7;
    const today = Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth(), new Date().getUTCDate());
    const [series, downloads] = await Promise.all([
      api.figures(range).catch(() => null),
      api.metrics.downloads(isoDay(today - (days - 1) * DAY_MS), isoDay(today)).catch(() => null)
    ]);
    if (mine !== run) return;
    own.replaceChildren();
    own.removeAttribute("aria-busy");
    if (series && series.enabled === false) {
      own.append(h("section", { class: "card figure-card", dataset: { figure: "off" } }, h("div", { class: "stat-label", text: "Figures" }), h("div", { class: "stat", text: "Off" }), h("p", { text: "The samples are off: the switch metrics is off." })));
    } else if (series) {
      for (const chart of charts(series.stepSeconds)) own.append(chartCard(series, chart));
    }
    if (downloads) own.append(repositoriesCard(downloads.rows, days));
  }
  buttons.forEach((b) => b.addEventListener("click", () => load(b.dataset.range)));
  return load("24h");
}
