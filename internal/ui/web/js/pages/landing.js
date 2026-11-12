// S08 Section landing: one card per screen of the section the user may open.

import { h } from "../dom.js";
import { ROUTES, SECTIONS } from "../routes.js";
import { pageHead, notFound, emptyState } from "../ui/page.js";
import { unavailable } from "../preview.js";

export async function render(ctx) {
  const section = SECTIONS.find((s) => s.landing === `/admin/${ctx.params.section}`);
  if (!section) {
    ctx.main.append(notFound("section"));
    return;
  }
  ctx.setTitle(section.title);
  const items = ROUTES.filter((r) => r.section === section.id && r.nav && ctx.allowed(r.perm));
  ctx.main.append(pageHead({ title: section.title, sub: section.text }));
  if (!items.length) {
    ctx.main.append(emptyState({ title: "Nothing to open here", text: "Your roles include no page of this section." }));
    return;
  }
  ctx.main.append(
    h(
      "div",
      { class: "cards" },
      items.map((r) =>
        h(
          "a",
          { class: ["card", unavailable(r.id) && "later"], href: `#${r.path}` },
          h("h2", { text: r.nav }),
          h("p", { text: r.text || "" }),
          unavailable(r.id) ? h("p", { class: "card-note", text: "Not in this preview" }) : null
        )
      )
    )
  );
}
