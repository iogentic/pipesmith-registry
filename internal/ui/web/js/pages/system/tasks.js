// S38 Tasks: every task with its state, last result, schedule and times; run and stop per row.
// While a task runs the list asks again, every second after an action and every 10 seconds else.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { time } from "../../format.js";
import { pageHead, linkButton, failure, button, busy } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { toast, errorText, toastProblem } from "../../ui/notice.js";
import { stateMark, resultMark, scheduleName, poller, typeIdView } from "./tasks-shared.js";

const running = (rows) => rows.some((t) => t.currentState === "RUNNING");

export async function render(ctx) {
  const canCreate = ctx.can("nexus:tasks:create");
  const canStart = ctx.can("nexus:tasks:start");
  const canStop = ctx.can("nexus:tasks:stop");
  ctx.main.append(
    pageHead({
      title: "Tasks",
      sub: "Jobs the server runs on their schedule or when they are started here.",
      actions: canCreate ? [linkButton("Create task", "#/admin/tasks/new", "primary")] : []
    })
  );

  let pages = 1;
  let token = null;
  async function fetchRows() {
    let rows = [];
    let next = null;
    let count = 0;
    do {
      const page = await api.tasks.list(next);
      rows = rows.concat((page && page.items) || []);
      next = (page && page.continuationToken) || null;
      count += 1;
    } while (next && count < pages);
    token = next;
    return rows;
  }

  let rows;
  try {
    rows = await fetchRows();
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/tasks", { replace: true }) });
    return null;
  }

  // The task whose button had the focus; after a redraw the focus goes back into its row.
  let focusTask = null;

  function actions(task) {
    const holder = h("div", { class: "actions" });
    if (task.currentState === "RUNNING") {
      if (canStop) {
        holder.append(
          button("Stop", { small: true, attrs: { "data-act": "stop", "data-task": task.id, "aria-label": `Stop ${task.name}` }, onClick: (event) => act(event.currentTarget, task, "stop") })
        );
      }
    } else if (canStart) {
      holder.append(button("Run", { small: true, attrs: { "data-act": "run", "data-task": task.id, "aria-label": `Run ${task.name}` }, onClick: (event) => act(event.currentTarget, task, "run") }));
    }
    return holder;
  }

  const columns = [
    { key: "name", label: "Name" },
    { key: "type", label: "Type", render: (t) => typeIdView(t.type) },
    {
      key: "currentState",
      label: "State",
      render: (t) => h("div", { class: "cell-stack" }, stateMark(t.currentState), t.currentState === "RUNNING" && t.message ? h("span", { class: "muted small", text: t.message }) : null)
    },
    {
      key: "lastRun",
      label: "Last run",
      value: (t) => t.lastRun || "",
      render: (t) =>
        h(
          "div",
          { class: "cell-stack" },
          resultMark(t.lastRunResult),
          t.lastRun ? h("span", { class: "small nowrap", text: time(t.lastRun) }) : null,
          t.currentState !== "RUNNING" && t.message ? h("span", { class: "muted small", text: t.message }) : null
        )
    },
    { key: "schedule", label: "Schedule", value: (t) => scheduleName(t.schedule) },
    { key: "nextRun", label: "Next run", value: (t) => t.nextRun || "", render: (t) => (t.nextRun ? h("span", { class: "nowrap", text: time(t.nextRun) }) : h("span", { class: "muted", text: "Not scheduled" })) }
  ];
  if (canStart || canStop) columns.push({ key: "actions", label: "Actions", sort: false, value: () => "", render: actions });

  const table = dataTable(columns, {
    rows,
    rowHref: (t) => `#/admin/tasks/${encodeURIComponent(t.id)}`,
    caption: "Tasks",
    filterLabel: "Filter the tasks",
    empty: {
      title: "No task yet",
      text: "Create a task to compact blob stores, apply cleanup policies or rebuild metadata on a schedule.",
      actions: canCreate ? [linkButton("Create task", "#/admin/tasks/new", "primary")] : []
    },
    more: {
      label: "Load more tasks",
      available: () => Boolean(token),
      load: async () => {
        const page = await api.tasks.list(token);
        token = (page && page.continuationToken) || null;
        pages += 1;
        return (page && page.items) || [];
      }
    }
  });
  table.el.classList.add("tasks-table");
  ctx.main.append(table.el);

  function restoreFocus() {
    if (!focusTask) return;
    const active = document.activeElement;
    if (active && active !== document.body && document.contains(active)) return;
    const id = CSS.escape(focusTask);
    const target = table.el.querySelector(`button[data-task="${id}"]`) || table.el.querySelector(`a.row-link[href="#/admin/tasks/${CSS.escape(encodeURIComponent(focusTask))}"]`);
    if (target) target.focus();
  }

  let shown = JSON.stringify(rows);
  async function refresh() {
    if (!ctx.main.isConnected) {
      poll.stop();
      return false;
    }
    let fresh;
    try {
      fresh = await fetchRows();
    } catch (error) {
      return running(table.rows());
    }
    const json = JSON.stringify(fresh);
    if (json !== shown) {
      shown = json;
      table.setRows(fresh);
    }
    restoreFocus();
    return running(fresh);
  }

  const poll = poller(refresh);

  async function act(buttonEl, task, what) {
    focusTask = task.id;
    try {
      await busy(buttonEl, () => (what === "run" ? api.tasks.run(task.id) : api.tasks.stop(task.id)));
      toast(what === "run" ? `${task.name} is running.` : `${task.name} is stopped.`, what === "run" ? "info" : "success");
    } catch (error) {
      toastProblem(error);
    }
    poll.kick();
    poll.update(await refresh());
  }

  const forget = (event) => {
    if (focusTask && !table.el.contains(event.target)) focusTask = null;
  };
  document.addEventListener("focusin", forget);
  poll.update(running(rows));

  return {
    dispose() {
      poll.stop();
      document.removeEventListener("focusin", forget);
    }
  };
}
