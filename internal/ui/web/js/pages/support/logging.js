// S30 Logging: the level of each logger. A level changes in place; a logger is added through a
// dialog; an added logger can be removed and a changed built in one reset; reset all puts every
// logger back to its default and removes the added ones.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { pageHead, failure, button, busy } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { form } from "../../ui/form.js";
import { toast, errorText, mark, toastProblem } from "../../ui/notice.js";
import { confirm, formDialog } from "../../ui/dialog.js";

const LEVELS = ["DEBUG", "INFO", "WARN", "ERROR"];

export async function render(ctx) {
  const canChange = ctx.can("nexus:logging:update");
  const actions = [];
  if (canChange) {
    actions.push(button("Add logger", { kind: "primary", attrs: { id: "add-logger" }, onClick: () => addLogger() }));
    actions.push(button("Reset all", { attrs: { id: "reset-loggers" }, onClick: () => resetAll() }));
  }
  ctx.main.append(
    pageHead({
      title: "Logging",
      sub: "Each logger writes the messages of its level and of the levels above it: DEBUG, INFO, WARN, ERROR.",
      actions
    })
  );

  let rows;
  try {
    rows = await api.logging.loggers();
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/logging", { replace: true }) });
    return;
  }

  // The actions cell of each row, kept so that a level change can show or hide Reset in place.
  const holders = new Map();

  function fillActions(holder, row) {
    holder.replaceChildren();
    if (!canChange) return;
    if (row.default === null || row.default === undefined) {
      holder.append(button("Remove", { small: true, attrs: { "data-remove": row.name, "aria-label": `Remove ${row.name}` }, onClick: (event) => removeLogger(event.currentTarget, row) }));
    } else if (row.level !== row.default) {
      holder.append(
        button("Reset", { small: true, attrs: { "data-reset": row.name, "aria-label": `Reset ${row.name} to ${row.default}` }, onClick: (event) => removeLogger(event.currentTarget, row) })
      );
    }
  }

  function levelCell(row) {
    if (!canChange) return h("span", { class: "mono", text: row.level });
    const select = h(
      "select",
      { class: "select level-select", "data-logger": row.name, "aria-label": `Level of ${row.name}` },
      LEVELS.map((level) => h("option", { value: level, text: level, selected: level === row.level }))
    );
    select.addEventListener("change", async () => {
      const level = select.value;
      const before = row.level;
      select.setAttribute("aria-busy", "true");
      try {
        await api.logging.set(row.name, level);
        row.level = level;
        toast(`The logger ${row.name} logs at ${level}.`);
        const holder = holders.get(row.name);
        if (holder) fillActions(holder, row);
      } catch (error) {
        select.value = before;
        toastProblem(error);
      } finally {
        select.removeAttribute("aria-busy");
      }
    });
    return select;
  }

  const columns = [
    { key: "name", label: "Logger", render: (r) => h("span", { class: "mono break", text: r.name }) },
    { key: "level", label: "Level", render: levelCell },
    { key: "default", label: "Default", value: (r) => r.default || "Added here", render: (r) => (r.default ? h("span", { class: "mono", text: r.default }) : mark("info", "Added here")) }
  ];
  if (canChange) {
    columns.push({
      key: "actions",
      label: "Actions",
      sort: false,
      value: () => "",
      render: (r) => {
        const holder = h("div", { class: "actions row-actions" });
        holders.set(r.name, holder);
        fillActions(holder, r);
        return holder;
      }
    });
  }

  const table = dataTable(columns, {
    rows,
    caption: "Loggers",
    filterLabel: "Filter the loggers",
    empty: { title: "No logger", text: "The server reports no logger." }
  });
  ctx.main.append(table.el);

  async function reload() {
    try {
      rows = await api.logging.loggers();
      table.setRows(rows);
    } catch (error) {
      toastProblem(error);
    }
  }

  async function removeLogger(buttonEl, row) {
    const added = row.default === null || row.default === undefined;
    try {
      await busy(buttonEl, () => api.logging.remove(row.name));
    } catch (error) {
      toastProblem(error);
      return;
    }
    toast(added ? `The logger ${row.name} is removed.` : `The logger ${row.name} is back at ${row.default}.`);
    await reload();
  }

  async function addLogger() {
    const f = form({
      fields: [
        { name: "name", label: "Logger name", type: "text", required: true, mono: true, placeholder: "pipesmith.http", help: "Parts separated by dots. A logger that exists gets the new level." },
        { name: "level", label: "Level", type: "select", required: true, noBlank: true, options: LEVELS.map((l) => ({ value: l, label: l })) }
      ],
      values: { name: "", level: "DEBUG" }
    });
    let done = null;
    const result = await formDialog({
      title: "Add logger",
      form: f,
      submitLabel: "Add",
      submit: async (v) => {
        const name = String(v.name || "").trim();
        const existed = rows.some((r) => r.name === name);
        await api.logging.set(name, v.level);
        done = { name, level: v.level, existed };
      }
    });
    if (result !== "submitted" || !done) return;
    toast(done.existed ? `The logger ${done.name} logs at ${done.level}.` : `The logger ${done.name} is added at ${done.level}.`);
    await reload();
  }

  async function resetAll() {
    const ok = await confirm({
      title: "Reset every logger?",
      text: "Every logger goes back to its default level, and the loggers added here are removed.",
      confirmLabel: "Reset all",
      danger: true
    });
    if (!ok) return;
    try {
      await api.logging.reset();
    } catch (error) {
      toastProblem(error);
      return;
    }
    toast("Every logger is back at its default level.");
    await reload();
  }
}
