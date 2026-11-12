// S34 System information: the sections the server reports, each as a key and value list, version
// and schema first, and the whole answer as a JSON file made in the browser.

import { h, download } from "../../dom.js";
import { api } from "../../api.js";
import { bytes, day } from "../../format.js";
import { pageHead, failure, button, emptyState, facts } from "../../ui/page.js";

const FIRST = ["version", "schema"];

function capital(text) {
  const s = String(text);
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function scalar(value) {
  return value === null || ["string", "number", "boolean"].includes(typeof value);
}

function show(key, value) {
  if (value === null || value === undefined || value === "") return null;
  if (typeof value === "boolean") return value ? "Yes" : "No";
  if (typeof value === "number" && /bytes$/i.test(key)) return `${bytes(value)} (${value} bytes)`;
  if (Array.isArray(value) && value.every(scalar)) return value.join(", ");
  if (typeof value === "object") return h("pre", { class: "mono small", text: JSON.stringify(value, null, 2) });
  return String(value);
}

export async function render(ctx) {
  let data = null;
  const save = button("Download as JSON", {
    attrs: { id: "download-info" },
    onClick: () => download(`system-information-${day(Date.now())}.json`, `${JSON.stringify(data, null, 2)}\n`)
  });
  ctx.main.append(pageHead({ title: "System information", sub: "What the running server reports about itself.", actions: [save] }));
  try {
    data = await api.systemInformation();
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/system-information", { replace: true }) });
    return;
  }
  const keys = data && typeof data === "object" ? Object.keys(data) : [];
  if (!keys.length) {
    save.disabled = true;
    ctx.main.append(emptyState({ title: "Nothing reported", text: "The server sent no section." }));
    return;
  }
  const order = [...FIRST.filter((k) => keys.includes(k)), ...keys.filter((k) => !FIRST.includes(k))];
  ctx.main.append(
    h(
      "div",
      { class: "info-grid" },
      order.map((key, i) => {
        const section = data[key];
        const id = `info-${i}`;
        const rows = section && typeof section === "object" && !Array.isArray(section) ? Object.entries(section).map(([k, v]) => [capital(k), show(k, v)]) : [["Value", show(key, section)]];
        return h("section", { class: "panel info-section", "aria-labelledby": id }, h("h2", { id, text: capital(key) }), rows.length ? facts(rows) : h("p", { class: "muted", text: "This section is empty." }));
      })
    )
  );
}
