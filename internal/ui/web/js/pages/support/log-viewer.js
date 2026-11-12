// S32 Log viewer: the end of one log file, 25, 50 or 100 KB, refreshed on a button or on a timer,
// and the whole file as a download. The timer stops when the page is left.

import { h, uid } from "../../dom.js";
import { api } from "../../api.js";
import { bytes } from "../../format.js";
import { pageHead, failure, button, emptyState } from "../../ui/page.js";
import { notice, errorText, problem } from "../../ui/notice.js";

const SIZES = [
  { value: 25600, label: "25 KB" },
  { value: 51200, label: "50 KB" },
  { value: 102400, label: "100 KB" }
];
const RATES = [
  { value: 0, label: "Off" },
  { value: 20, label: "Every 20 seconds" },
  { value: 60, label: "Every minute" },
  { value: 120, label: "Every 2 minutes" },
  { value: 300, label: "Every 5 minutes" }
];

const encoder = new TextEncoder();
const clock = (d) => [d.getHours(), d.getMinutes(), d.getSeconds()].map((n) => String(n).padStart(2, "0")).join(":");

export async function render(ctx) {
  const name = ctx.params.file;
  const base = name.split("/").pop();
  let size = SIZES.some((s) => String(s.value) === ctx.query.size) ? Number(ctx.query.size) : SIZES[0].value;
  let rate = 0;
  let timer = null;
  let stopped = false;
  ctx.setTitle(name);

  const head = pageHead({ title: name, sub: "The end of the file as the server has it now.", crumbs: [{ label: "Log files", href: "#/admin/logs" }] });
  ctx.main.append(head);

  let text;
  try {
    text = await api.logs.tail(name, size);
  } catch (error) {
    failure(ctx.main, error, { what: "log file", retry: () => ctx.navigate(`/admin/logs/${name}`, { replace: true }) });
    return null;
  }

  // Size: three radio buttons that look like one segmented control.
  const sizeGroup = h(
    "fieldset",
    { class: "fieldset log-size" },
    h("legend", { class: "label", text: "Size" }),
    h(
      "div",
      { class: "segmented" },
      SIZES.map((s) => {
        const id = uid("size");
        const input = h("input", { type: "radio", name: "log-size", id, value: String(s.value), checked: s.value === size, class: "visually-hidden" });
        input.addEventListener("change", () => {
          if (!input.checked) return;
          size = s.value;
          load();
        });
        return h("label", { for: id }, input, h("span", { text: s.label }));
      })
    )
  );

  const rateSelect = h("select", { class: "select", id: "log-refresh" }, RATES.map((r) => h("option", { value: String(r.value), text: r.label })));
  const rateField = h("div", { class: "field log-rate" }, h("label", { class: "label", for: "log-refresh", text: "Refresh" }), rateSelect);
  rateSelect.addEventListener("change", () => {
    rate = Number(rateSelect.value);
    schedule();
    status();
  });

  const reload = button("Refresh now", { attrs: { id: "log-reload" }, onClick: () => load() });
  const download = h("a", { class: "btn", id: "log-download", href: api.logs.downloadUrl(name), download: base, text: "Download", "aria-label": `Download ${name} whole` });

  const tools = h("div", { class: "log-tools" }, sizeGroup, rateField, h("div", { class: "actions" }, reload, download));
  const updated = h("p", { class: "muted small", id: "log-updated", "aria-live": "polite" });
  const problem = h("div");
  const pre = h("pre", { class: "log", id: "log-text", tabindex: "0", "aria-label": `End of ${name}` });
  const holder = h("div", {}, pre);
  ctx.main.append(h("div", { class: "panel log-panel" }, tools, problem, updated, holder));

  let readAt = null;
  let shownBytes = 0;
  let whole = false;
  let first = true;

  function status() {
    if (!readAt) return;
    const what = whole ? `The whole file, ${bytes(shownBytes)}` : `The last ${SIZES.find((s) => s.value === size).label}, from the first full line`;
    const every = rate ? ` Refreshes ${RATES.find((r) => r.value === rate).label.toLowerCase()}.` : "";
    updated.textContent = `${what}, read at ${clock(readAt)}.${every}`;
  }

  function show(raw) {
    let value = raw || "";
    const length = encoder.encode(value).length;
    whole = length < size;
    if (!whole) {
      const cut = value.indexOf("\n");
      if (cut >= 0 && cut < value.length - 1) value = value.slice(cut + 1);
    }
    shownBytes = encoder.encode(value).length;
    const atEnd = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
    const keep = pre.scrollTop;
    if (!value) {
      holder.replaceChildren(emptyState({ title: "The file is empty", text: "The server has written nothing to it yet." }));
    } else {
      // One block per line, so that a wrapped line hangs under its own start.
      pre.replaceChildren(...value.replace(/\n$/, "").split("\n").map((line) => h("span", { class: "log-line", text: line || " " })));
      if (!pre.isConnected) holder.replaceChildren(pre);
      pre.scrollTop = atEnd || first ? pre.scrollHeight : keep;
    }
    first = false;
    readAt = new Date();
    status();
  }

  let running = 0;
  async function load() {
    const call = ++running;
    reload.setAttribute("aria-busy", "true");
    try {
      const fresh = await api.logs.tail(name, size);
      if (stopped || call !== running) return;
      problem.replaceChildren();
      show(fresh);
    } catch (error) {
      if (stopped || call !== running) return;
      problem.replaceChildren(problem(error, "The file could not be read"));
    } finally {
      if (call === running) reload.removeAttribute("aria-busy");
    }
    schedule();
  }

  function schedule() {
    clearTimeout(timer);
    timer = null;
    if (stopped || !rate) return;
    timer = setTimeout(() => {
      timer = null;
      if (!ctx.main.isConnected) {
        stop();
        return;
      }
      load();
    }, rate * 1000);
  }

  function stop() {
    stopped = true;
    clearTimeout(timer);
    timer = null;
  }

  show(text);
  return { dispose: stop };
}
