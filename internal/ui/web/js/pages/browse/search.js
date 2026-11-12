// S03 Search: components by keyword, by the criteria of docs/ui/API.md, section S03, and by the
// fields of one format. The query lives in the address, so a search can be linked, the search field
// of the top bar lands here, and the back button returns to the search before.

import { h, getPath, setPath } from "../../dom.js";
import { api } from "../../api.js";
import { formatName, number } from "../../format.js";
import { pageHead, failure, loading } from "../../ui/page.js";
import { form } from "../../ui/form.js";
import { dataTable } from "../../ui/table.js";
import { notice, toast, errorText, problem, toastProblem } from "../../ui/notice.js";
import { treeHref } from "./common.js";

const CHECKSUM_BY_LENGTH = { 32: "md5", 40: "sha1", 64: "sha256", 128: "sha512" };
const HEX = /^[0-9a-f]+$/i;
const WILDCARD = "A * matches any text.";

const is = (...formats) => (values) => formats.includes(values.format || "");

// One field per query parameter. A parameter some formats share has one field shown for each of them.
const CRITERIA = [
  { name: "group", label: "Group", show: is(""), help: WILDCARD },
  { name: "maven.groupId", label: "Group ID", show: is("maven2"), help: WILDCARD },
  { name: "maven.artifactId", label: "Artifact ID", show: is("maven2"), help: WILDCARD },
  { name: "npm.scope", label: "Scope", show: is("npm"), help: "Without the @." },
  { name: "docker.imageName", label: "Image name", show: is("docker"), help: "With its folders, for example platform/api-gateway." },
  { name: "name", label: "Name", show: is("", "npm", "raw"), help: WILDCARD },
  { name: "version", label: "Version", show: is("", "maven2", "npm") },
  { name: "docker.imageTag", label: "Tag", show: is("docker") },
  { name: "maven.baseVersion", label: "Base version", show: is("maven2"), help: "For a snapshot, the version that ends in -SNAPSHOT." },
  { name: "maven.extension", label: "Extension", show: is("maven2") },
  { name: "maven.classifier", label: "Classifier", show: is("maven2") },
  { name: "npm.author", label: "Author", show: is("npm") },
  { name: "npm.description", label: "Description", show: is("npm") },
  { name: "npm.keywords", label: "Package keyword", show: is("npm") },
  { name: "npm.license", label: "License", show: is("npm") },
  { name: "docker.layerId", label: "Layer digest", show: is("docker"), mono: true },
  { name: "docker.contentDigest", label: "Manifest digest", show: is("docker"), mono: true }
];

const PARAMS = ["q", "format", "repository", ...CRITERIA.map((c) => c.name)];

function checksumProblem(value) {
  const text = String(value || "").trim();
  if (!text) return null;
  return HEX.test(text) && CHECKSUM_BY_LENGTH[text.length] ? null : "Enter 32, 40, 64 or 128 hex digits: an MD5, SHA-1, SHA-256 or SHA-512 checksum.";
}

// The form values of a query of the address.
function valuesOf(query) {
  const values = {};
  for (const key of PARAMS) if (query[key]) setPath(values, key, query[key]);
  for (const algorithm of Object.values(CHECKSUM_BY_LENGTH)) if (query[algorithm]) values.checksum = query[algorithm];
  return values;
}

// The query parameters of the form values: only filled fields, the checksum under its algorithm.
function paramsOf(values) {
  const out = {};
  for (const key of PARAMS) {
    const value = getPath(values, key);
    if (value !== undefined && value !== null && String(value).trim() !== "") out[key] = String(value).trim();
  }
  const sum = String(values.checksum || "").trim().toLowerCase();
  if (sum && CHECKSUM_BY_LENGTH[sum.length]) out[CHECKSUM_BY_LENGTH[sum.length]] = sum;
  return out;
}

function hashOf(params) {
  const q = new URLSearchParams(params).toString();
  return q ? `#/search?${q}` : "#/search";
}

function firstPath(component) {
  const asset = (component.assets || [])[0];
  return asset && asset.path ? String(asset.path).replace(/^\/+/, "") : "";
}

export async function render(ctx) {
  ctx.main.append(pageHead({ title: "Search", sub: "Find components by keyword, coordinates or checksum in the repositories you may browse." }));

  let repositories;
  try {
    repositories = await api.repositories.list();
  } catch (error) {
    failure(ctx.main, error, { retry: () => location.reload() });
    return;
  }

  const f = form({
    sections: [
      {
        grid: true,
        fields: [
          { name: "q", label: "Keyword", type: "text", wide: true, help: "Matches the group, the name and the version. Several words must all match." },
          {
            name: "format",
            label: "Format",
            type: "select",
            blank: "Any format",
            options: ["maven2", "npm", "docker", "raw"].map((id) => ({ value: id, label: formatName(id) }))
          },
          {
            name: "repository",
            label: "Repository",
            type: "select",
            blank: "Any repository",
            options: (values) => repositories.filter((r) => !values.format || r.format === values.format).map((r) => ({ value: r.name, label: `${r.name} (${formatName(r.format)} ${r.type})` }))
          }
        ]
      },
      {
        title: "Criteria",
        help: "The fields change with the format.",
        grid: true,
        fields: [
          ...CRITERIA.map((c) => ({ type: "text", autocomplete: "off", ...c })),
          { name: "checksum", label: "Checksum", type: "text", mono: true, wide: true, help: "MD5, SHA-1, SHA-256 or SHA-512 of one of the files.", validate: checksumProblem }
        ]
      }
    ],
    values: valuesOf(ctx.query)
  });

  const submit = h("button", { type: "submit", class: "btn primary", text: "Search" });
  const clear = h("button", { type: "button", class: "btn", text: "Clear" });
  const messages = h("div", { class: "stack" });
  const formEl = h("form", { novalidate: true, class: "panel", id: "search-form", role: "search", "aria-label": "Components" }, messages, f.el, h("div", { class: "form-actions" }, submit, clear));
  const results = h("section", { class: "section", id: "search-results", "aria-label": "Results" });
  ctx.main.append(formEl, results);

  let run = 0;

  async function search(params) {
    const mine = ++run;
    messages.replaceChildren();
    results.replaceChildren(loading("Searching"));
    let page;
    try {
      page = await api.search(params);
    } catch (error) {
      if (mine !== run) return;
      results.replaceChildren(problem(error, "The search failed"));
      return;
    }
    if (mine !== run) return;
    let token = page.continuationToken || null;
    const items = page.items || [];
    let loaded = items.length;
    const line = h("p", { class: "muted result-line", role: "status" });
    const say = () => {
      line.textContent = `${number(loaded)} ${loaded === 1 ? "component" : "components"} loaded${token ? ", more on the server." : "."}`;
    };
    const missing = (text) => h("span", { class: "muted", text });
    const table = dataTable(
      [
        { key: "name", label: "Name", class: "break" },
        {
          key: "group",
          label: "Group",
          class: "break",
          value: (c) => (c.group && c.format === "npm" ? `@${c.group}` : c.group),
          render: (c) => (c.group ? (c.format === "npm" ? `@${c.group}` : c.group) : missing("No group"))
        },
        { key: "version", label: "Version", class: "break", render: (c) => c.version || missing("No version") },
        { key: "format", label: "Format", value: (c) => formatName(c.format) },
        { key: "repository", label: "Repository", class: "break" }
      ],
      {
        rows: items,
        filter: false,
        rowHref: (c) => treeHref(c.repository, firstPath(c)),
        caption: "Components that match the search",
        empty: { title: "No component matches", text: "Check the spelling, use * as a wildcard, or remove a criterion." },
        more: {
          label: "Load more",
          available: () => Boolean(token),
          load: async () => {
            try {
              const next = await api.search({ ...params, continuationToken: token });
              token = next.continuationToken || null;
              loaded += (next.items || []).length;
              say();
              return next.items || [];
            } catch (error) {
              toastProblem(error);
              return null;
            }
          }
        }
      }
    );
    say();
    results.replaceChildren(...[h("h2", { text: "Results" }), items.length ? line : null, table.el].filter(Boolean));
  }

  function criteria(values) {
    const params = paramsOf(values);
    const { format, repository, ...rest } = params;
    return { params, any: Object.keys(rest).length > 0 || Boolean(format) || Boolean(repository) };
  }

  formEl.addEventListener("submit", (event) => {
    event.preventDefault();
    messages.replaceChildren();
    if (!f.validate()) return;
    const { params, any } = criteria(f.values());
    if (!any) {
      messages.append(notice("info", "Nothing to search for", "Enter a keyword or fill at least one criterion."));
      return;
    }
    const target = hashOf(params);
    if (location.hash !== target) history.pushState(null, "", target);
    search(params);
  });

  clear.addEventListener("click", () => {
    f.set({});
    f.clearErrors();
    messages.replaceChildren();
    results.replaceChildren();
    run += 1;
    if (location.hash !== "#/search") history.pushState(null, "", "#/search");
    f.focusFirst();
  });

  const { params, any } = criteria(valuesOf(ctx.query));
  if (any) await search(params);
}
