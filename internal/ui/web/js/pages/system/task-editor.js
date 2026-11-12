// S40 Task editor: create a task of one type, or read and change one; run, stop and delete it.
// The settings follow docs/ui/API.md, section S40: property values travel as strings, the schedule
// carries only the fields of its type, times show in the browser's zone and travel in UTC.

import { h, uid } from "../../dom.js";
import { api } from "../../api.js";
import { time, formatName } from "../../format.js";
import { pageHead, failure, button, busy, notFound, facts } from "../../ui/page.js";
import { form } from "../../ui/form.js";
import { notice, toast, errorText, problem, toastProblem } from "../../ui/notice.js";
import { confirm } from "../../ui/dialog.js";
import { SCHEDULES, NEEDS_START, WEEK_DAYS, scheduleName, stateMark, resultMark, poller, parseLocal, zoneText } from "./tasks-shared.js";

const LOCAL_TIME = { regex: /^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$/, message: "Write the time as YYYY-MM-DD HH:MM, for example 2026-11-14 02:30." };

function parseMonthDays(text) {
  const parts = String(text || "").split(/[\s,]+/).filter(Boolean);
  if (!parts.length) return null;
  const days = [];
  for (const part of parts) {
    if (!/^\d{1,2}$/.test(part)) return null;
    const n = Number(part);
    if (n < 1 || n > 31) return null;
    if (!days.includes(n)) days.push(n);
  }
  return days.sort((a, b) => a - b);
}

function tomorrowAtOne() {
  const d = new Date();
  d.setDate(d.getDate() + 1);
  d.setHours(1, 0, 0, 0);
  return d.getTime();
}

// One form field per field of the task type. Choices of repositories and blob stores come from
// their lists; when a list cannot be read the field takes the name as text.
function propertyField(field, choices, current) {
  const base = { name: `properties.${field.id}`, label: field.label || field.id, required: Boolean(field.required) };
  const withCurrent = (options) => (current && !options.some((o) => o.value === current) ? [...options, { value: current, label: `${current} (not found)` }] : options);
  switch (field.type) {
    case "number":
      return { ...base, type: "number" };
    case "boolean":
      return { ...base, type: "checkbox", required: false, wide: true };
    case "select":
      return { ...base, type: "select", options: withCurrent((field.options || []).map((o) => ({ value: String(o.value), label: o.label || String(o.value) }))) };
    case "repository":
      if (!choices.repositories) return { ...base, type: "text", mono: true, help: "The list of repositories could not be read. Enter the name, or * for all." };
      return {
        ...base,
        type: "select",
        options: withCurrent([{ value: "*", label: "All repositories" }, ...choices.repositories.map((r) => ({ value: r.name, label: `${r.name} (${formatName(r.format)} ${r.type})` }))])
      };
    case "blobstore":
      if (!choices.stores) return { ...base, type: "text", mono: true, help: "The list of blob stores could not be read. Enter the name." };
      return { ...base, type: "select", options: withCurrent(choices.stores.map((s) => ({ value: s.name, label: s.unavailable ? `${s.name} (unavailable)` : s.name }))) };
    default:
      return { ...base, type: "text" };
  }
}

function toValues(settings, type) {
  const properties = {};
  for (const field of type.fields) {
    const raw = settings.properties ? settings.properties[field.id] : undefined;
    const value = raw === undefined || raw === null ? field.default : raw;
    if (field.type === "boolean") properties[field.id] = value === true || value === "true";
    else if (field.type === "number") properties[field.id] = value === undefined || value === null || value === "" ? null : Number(value);
    else properties[field.id] = value === undefined || value === null ? "" : String(value);
  }
  const s = settings.schedule || { type: "manual" };
  return {
    name: settings.name || "",
    enabled: settings.enabled !== false,
    properties,
    schedule: {
      type: s.type || "manual",
      startAt: time(s.startAt || tomorrowAtOne()),
      monthDays: (s.monthDays || []).join(", "),
      cron: s.cron || ""
    }
  };
}

function toBody(values, weekDays, type) {
  const properties = {};
  for (const field of type.fields) {
    const value = values.properties ? values.properties[field.id] : undefined;
    if (field.type === "boolean") properties[field.id] = value ? "true" : "false";
    else if (value !== null && value !== undefined && String(value).trim() !== "") properties[field.id] = String(value).trim();
  }
  const kind = values.schedule.type;
  const schedule = { type: kind };
  if (NEEDS_START.includes(kind)) schedule.startAt = parseLocal(values.schedule.startAt).toISOString();
  if (kind === "weekly") schedule.weekDays = weekDays;
  if (kind === "monthly") schedule.monthDays = parseMonthDays(values.schedule.monthDays);
  if (kind === "cron") schedule.cron = String(values.schedule.cron || "").trim();
  return { typeId: type.id, name: String(values.name || "").trim(), enabled: Boolean(values.enabled), schedule, properties };
}

// The days of a weekly schedule: one check box per day, its own error line, kept beside the
// fields of the form because the form has no control for a set of check boxes.
function weekDaysControl(readOnly, onChange) {
  const errorId = uid("days-error");
  const error = h("div", { class: "error-text", id: errorId });
  const boxes = WEEK_DAYS.map((day) => {
    const id = uid("day");
    const input = h("input", { type: "checkbox", id, name: "weekday", value: day.id, disabled: readOnly, "aria-describedby": errorId });
    input.addEventListener("change", () => {
      showError("");
      onChange();
    });
    return { input, el: h("label", { class: "check", for: id }, input, h("span", { class: "label", text: day.label })) };
  });
  function showError(message) {
    error.textContent = message || "";
    for (const box of boxes) {
      if (message) box.input.setAttribute("aria-invalid", "true");
      else box.input.removeAttribute("aria-invalid");
    }
  }
  const el = h(
    "fieldset",
    { class: "fieldset wide", id: "weekdays" },
    h("legend", {}, "Days of the week", readOnly ? null : h("span", { class: "req", "aria-hidden": "true", text: "*" })),
    h("div", { class: "weekdays" }, boxes.map((b) => b.el)),
    error
  );
  return {
    el,
    get: () => boxes.filter((b) => b.input.checked).map((b) => b.input.value),
    set: (days) => boxes.forEach((b) => (b.input.checked = (days || []).includes(b.input.value))),
    showError,
    focus() {
      boxes[0].input.focus();
      el.scrollIntoView({ block: "center" });
    }
  };
}

function summaryFacts(s) {
  return facts([
    ["State", stateMark(s.currentState)],
    ["Last result", resultMark(s.lastRunResult)],
    ["Message", s.message || null],
    ["Last run", s.lastRun ? time(s.lastRun) : "Never"],
    ["Next run", s.nextRun ? time(s.nextRun) : "Not scheduled"],
    ["Schedule", scheduleName(s.schedule)]
  ]);
}

export async function render(ctx) {
  const creating = Boolean(ctx.params.type);
  const id = ctx.params.id;
  let types;
  let settings;
  let summary = null;
  try {
    const loads = [api.tasks.types()];
    if (!creating) loads.push(api.tasks.settings(id), api.tasks.get(id));
    [types, settings, summary] = await Promise.all(loads);
  } catch (error) {
    failure(ctx.main, error, { what: "task", retry: () => location.reload() });
    return null;
  }

  const typeId = creating ? ctx.params.type : settings.typeId;
  let type = (types || []).find((t) => t.id === typeId);
  const messages = h("div", { class: "stack" });
  if (!type) {
    if (creating) {
      ctx.main.append(notFound("task type"));
      return null;
    }
    type = { id: typeId, name: typeId, description: "", fields: Object.keys(settings.properties || {}).map((key) => ({ id: key, label: key, type: "string" })) };
    messages.append(notice("warning", "Unknown type", `This server does not list the type ${typeId}. Its settings are shown as text.`));
  }
  if (creating) settings = { typeId, name: "", enabled: true, schedule: { type: "manual" }, properties: {} };

  const needs = new Set(type.fields.map((f) => f.type));
  const [repositories, stores] = await Promise.allSettled([
    needs.has("repository") ? api.repositories.list() : Promise.resolve([]),
    needs.has("blobstore") ? api.blobStores.list() : Promise.resolve([])
  ]);
  const choices = {
    repositories: repositories.status === "fulfilled" ? repositories.value || [] : null,
    stores: stores.status === "fulfilled" ? stores.value || [] : null
  };

  const canEdit = creating ? ctx.can("nexus:tasks:create") : ctx.can("nexus:tasks:update");
  const title = creating ? "New task" : settings.name;
  ctx.setTitle(creating ? `New task: ${type.name}` : settings.name);

  const actionsEl = h("div", { class: "actions" });
  const head = pageHead({
    title,
    sub: `${type.name}. ${type.description || ""}`.trim(),
    crumbs: [{ label: "Tasks", href: "#/admin/tasks" }, ...(creating ? [{ label: "New task", href: "#/admin/tasks/new" }] : [])]
  });
  head.append(actionsEl);
  ctx.main.append(head);

  if (!canEdit) messages.append(notice("info", "Read only", ctx.readOnlyText("Your roles let you read this task, not change it.")));
  const disabledNote = h("div");
  messages.append(disabledNote);

  let daysDirty = false;
  const days = weekDaysControl(!canEdit, () => {
    daysDirty = true;
  });

  const sections = [
    {
      title: "Task",
      grid: true,
      fields: [
        { name: "name", label: "Name", type: "text", required: true, wide: true, autocomplete: "off" },
        { name: "_type", label: "Type", type: "static", render: () => h("span", {}, type.name, " ", h("span", { class: "mono muted small", text: type.id })) },
        { name: "enabled", label: "Enabled", type: "checkbox", wide: true, help: "A disabled task runs neither on its schedule nor when started here." }
      ]
    }
  ];
  if (type.fields.length) {
    sections.push({
      title: "Settings",
      grid: true,
      fields: type.fields.map((field) => propertyField(field, choices, settings.properties ? settings.properties[field.id] : null))
    });
  }
  const kindOf = (v) => (v.schedule ? v.schedule.type : null);
  sections.push({
    title: "Schedule",
    grid: true,
    fields: [
      { name: "schedule.type", label: "Runs", type: "select", required: true, noBlank: true, options: SCHEDULES.map((s) => ({ value: s.value, label: s.label })) },
      {
        name: "schedule.startAt",
        label: "Starts at",
        type: "text",
        required: true,
        mono: true,
        pattern: LOCAL_TIME,
        validate: (value) => (parseLocal(value) ? null : "This date and time does not exist."),
        help: `YYYY-MM-DD HH:MM in the time zone of this browser, ${zoneText()}. No run comes earlier. Runs keep this time of day, hourly runs this minute.`,
        show: (v) => NEEDS_START.includes(kindOf(v))
      },
      {
        name: "schedule.monthDays",
        label: "Days of the month",
        type: "text",
        required: true,
        placeholder: "1, 15",
        help: "Whole numbers from 1 to 31, separated by commas.",
        validate: (value) => (parseMonthDays(value) ? null : "Use whole numbers from 1 to 31, separated by commas."),
        show: (v) => kindOf(v) === "monthly"
      },
      {
        name: "schedule.cron",
        label: "Cron expression",
        type: "text",
        required: true,
        mono: true,
        wide: true,
        placeholder: "0 0 3 ? * MON-FRI",
        help: "Six or seven fields: second, minute, hour, day of the month, month, day of the week and an optional year.",
        show: (v) => kindOf(v) === "cron"
      }
    ]
  });

  let values = toValues(settings, type);
  const f = form({
    sections,
    values,
    readOnly: !canEdit,
    onChange: (v) => {
      days.el.hidden = kindOf(v) !== "weekly";
    }
  });
  const startField = f.el.querySelector('[name="schedule.startAt"]').closest(".field");
  startField.after(days.el);
  days.set((settings.schedule || {}).weekDays || []);
  days.el.hidden = values.schedule.type !== "weekly";
  ctx.setDirty(() => f.isDirty() || daysDirty);

  const summaryBody = h("div");
  const summaryEl = creating ? null : h("section", { class: "panel sys-panel task-summary", id: "task-summary", "aria-labelledby": "task-summary-title" }, h("h2", { id: "task-summary-title", text: "Summary" }), summaryBody);

  const save = h("button", { type: "submit", class: "btn primary", text: creating ? "Create task" : "Save" });
  const discard = h("button", { type: "button", class: "btn", text: creating ? "Cancel" : "Discard changes" });
  const formEl = h("form", { novalidate: true, class: "panel" }, f.el, canEdit ? h("div", { class: "form-actions" }, save, discard) : null);
  ctx.main.append(h("div", { class: "stack" }, messages, summaryEl, formEl));

  let lastAction = false;
  const poll = poller(async () => {
    if (!ctx.main.isConnected) {
      poll.stop();
      return false;
    }
    return refreshSummary();
  });

  function drawDisabledNote() {
    disabledNote.replaceChildren();
    if (!creating && !settings.enabled) disabledNote.append(notice("info", "Disabled", "This task does not run until it is enabled and saved."));
  }

  function drawSummary() {
    if (!summaryEl) return;
    summaryBody.replaceChildren(summaryFacts(summary));
    actionsEl.replaceChildren();
    const isRunning = summary.currentState === "RUNNING";
    if (!isRunning && settings.enabled && ctx.can("nexus:tasks:start")) {
      actionsEl.append(button("Run now", { attrs: { id: "task-run" }, onClick: (event) => runOrStop(event.currentTarget, "run") }));
    }
    if (isRunning && ctx.can("nexus:tasks:stop")) {
      actionsEl.append(button("Stop", { attrs: { id: "task-stop" }, onClick: (event) => runOrStop(event.currentTarget, "stop") }));
    }
    if (!isRunning && ctx.can("nexus:tasks:delete")) {
      actionsEl.append(button("Delete", { kind: "danger", attrs: { id: "task-delete" }, onClick: remove }));
    }
  }

  // After Run or Stop the button the user pressed is gone; the focus moves to the first action.
  async function refreshSummary() {
    let fresh;
    try {
      fresh = await api.tasks.get(id);
    } catch (error) {
      return summary.currentState === "RUNNING";
    }
    if (JSON.stringify(fresh) !== JSON.stringify(summary)) {
      const active = document.activeElement;
      const hadFocus = lastAction && (!active || active === document.body || actionsEl.contains(active));
      summary = fresh;
      drawSummary();
      const target = actionsEl.querySelector("button");
      if (hadFocus && target) target.focus();
    }
    return summary.currentState === "RUNNING";
  }

  async function runOrStop(buttonEl, what) {
    lastAction = true;
    try {
      await busy(buttonEl, () => (what === "run" ? api.tasks.run(id) : api.tasks.stop(id)));
      toast(what === "run" ? `${settings.name} is running.` : `${settings.name} is stopped.`, what === "run" ? "info" : "success");
    } catch (error) {
      toastProblem(error);
    }
    poll.kick();
    poll.update(await refreshSummary());
  }

  async function remove() {
    const ok = await confirm({
      title: `Delete ${settings.name}?`,
      text: "The task and its schedule are removed. Log files it wrote stay in the log directory.",
      confirmLabel: "Delete task",
      danger: true
    });
    if (!ok) return;
    try {
      await api.tasks.remove(id);
    } catch (error) {
      toastProblem(error);
      return;
    }
    ctx.setDirty(null);
    toast(`The task ${settings.name} is deleted.`);
    ctx.navigate("/admin/tasks");
  }

  discard.addEventListener("click", () => {
    if (creating) {
      ctx.setDirty(null);
      ctx.navigate("/admin/tasks/new");
      return;
    }
    f.set(values);
    days.set((settings.schedule || {}).weekDays || []);
    days.showError("");
    days.el.hidden = values.schedule.type !== "weekly";
    daysDirty = false;
    f.clearErrors();
    messages.querySelectorAll(".notice.error").forEach((n) => n.remove());
  });

  formEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    messages.querySelectorAll(".notice.error").forEach((n) => n.remove());
    const okForm = f.validate();
    const weekly = f.values().schedule.type === "weekly";
    const okDays = !weekly || days.get().length > 0;
    days.showError(okDays ? "" : "Pick at least one day.");
    if (!okForm) return;
    if (!okDays) {
      days.focus();
      return;
    }
    const body = toBody(f.values(), days.get(), type);
    let created = null;
    try {
      await busy(save, async () => {
        if (creating) created = await api.tasks.create(body);
        else await api.tasks.update(id, body);
      });
    } catch (error) {
      const dayError = (error.fieldErrors || []).find((e) => e.id === "schedule.weekDays");
      if (dayError) days.showError(dayError.message);
      if (!f.showServerErrors(error)) messages.append(problem(error, creating ? "Not created" : "Not saved"));
      return;
    }
    f.markClean();
    daysDirty = false;
    if (creating) {
      ctx.setDirty(null);
      toast(`The task ${body.name} is created.`);
      ctx.navigate(created && created.id ? `/admin/tasks/${encodeURIComponent(created.id)}` : "/admin/tasks");
      return;
    }
    try {
      [settings, summary] = await Promise.all([api.tasks.settings(id), api.tasks.get(id)]);
      values = toValues(settings, type);
      f.set(values);
      days.set((settings.schedule || {}).weekDays || []);
      days.el.hidden = values.schedule.type !== "weekly";
      head.querySelector("h1").textContent = settings.name;
      ctx.setTitle(settings.name);
      drawSummary();
      drawDisabledNote();
    } catch (error) {
      /* the save went through; the form keeps what was sent */
    }
    toast(`The task ${body.name} is saved.`);
  });

  drawDisabledNote();
  if (!creating) {
    drawSummary();
    poll.update(summary.currentState === "RUNNING");
  }

  return {
    dispose() {
      poll.stop();
    }
  };
}
