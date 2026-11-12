// S10 New repository: pick the format and the type.

import { h } from "../../dom.js";
import { formatName } from "../../format.js";
import { pageHead } from "../../ui/page.js";

const FORMATS = [
  { id: "maven2", text: "Artifacts for Maven and Gradle builds." },
  { id: "npm", text: "Packages for npm and compatible clients." },
  { id: "docker", text: "Container images for Docker and OCI clients." },
  { id: "raw", text: "Any file under any path." }
];

const TYPES = [
  { id: "hosted", label: "Hosted", text: "Keeps what is deployed to it." },
  { id: "proxy", label: "Proxy", text: "Caches what a remote serves." },
  { id: "group", label: "Group", text: "Serves several repositories under one address." }
];

export async function render(ctx) {
  ctx.main.append(
    pageHead({
      title: "New repository",
      sub: "Pick the format and the type. Neither can change after the repository is created.",
      crumbs: [{ label: "Repositories", href: "#/admin/repositories" }]
    })
  );
  ctx.main.append(
    h(
      "div",
      { class: "cards" },
      FORMATS.map((f) =>
        h(
          "section",
          { class: "card", "aria-labelledby": `fmt-${f.id}` },
          h("h2", { id: `fmt-${f.id}`, text: formatName(f.id) }),
          h("p", { text: f.text }),
          h(
            "ul",
            { class: "nav-list" },
            TYPES.map((t) =>
              h(
                "li",
                {},
                h("a", { href: `#/admin/repositories/new/${f.id}/${t.id}`, "aria-label": `${formatName(f.id)} ${t.label.toLowerCase()}` }, h("strong", { text: t.label }), h("span", { class: "muted small", text: ` ${t.text}` }))
              )
            )
          )
        )
      )
    )
  );
}
