// S39 New task: pick one of the task types this server runs.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { pageHead, failure, emptyState } from "../../ui/page.js";

export async function render(ctx) {
  ctx.main.append(
    pageHead({
      title: "New task",
      sub: "Pick what the task does. The type cannot change after the task is created.",
      crumbs: [{ label: "Tasks", href: "#/admin/tasks" }]
    })
  );
  let types;
  try {
    types = await api.tasks.types();
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/tasks/new", { replace: true }) });
    return;
  }
  if (!types || !types.length) {
    ctx.main.append(emptyState({ title: "No task type", text: "This server offers no task type to create." }));
    return;
  }
  const sorted = [...types].sort((a, b) => String(a.name).localeCompare(String(b.name), "en"));
  ctx.main.append(
    h(
      "div",
      { class: "cards" },
      sorted.map((t) =>
        h(
          "a",
          { class: "card", href: `#/admin/tasks/new/${encodeURIComponent(t.id)}` },
          h("h2", { text: t.name }),
          h("p", { text: t.description || "" }),
          h("p", { class: "mono task-type-id", text: t.id })
        )
      )
    )
  );
}
