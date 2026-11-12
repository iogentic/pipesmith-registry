// S07 Upload form. The fields come from the upload specs of the format, the multipart field names
// follow docs/ui/API.md, section S07: <format>.<field> for the component, <format>.asset<N> for the
// file N and <format>.asset<N>.<field> for its fields; npm has the one file npm.asset.
//
// Each file input sits in the form as a static field of the same name, so a validation answer of
// the server for a file shows beside it like any other field error.

import { h, getPath, uid } from "../../dom.js";
import { api } from "../../api.js";
import { formatName, plural } from "../../format.js";
import { pageHead, failure, notFound, noAccess, linkButton, busy, stateView } from "../../ui/page.js";
import { form } from "../../ui/form.js";
import { notice, errorText, problem } from "../../ui/notice.js";
import { takesUploads, mayUpload, treeHref } from "./common.js";

const MAX_FILES = 3;
const LABELS = {
  groupId: "Group ID",
  artifactId: "Artifact ID",
  version: "Version",
  "generate-pom": "Generate a POM file from these coordinates",
  packaging: "Packaging",
  extension: "Extension",
  classifier: "Classifier",
  directory: "Directory",
  filename: "File name"
};
const ABOUT = {
  maven2: "Enter the coordinates and add up to three files. A POM file among them can supply the coordinates.",
  npm: "Add one package file made by npm pack. Its package.json names the package and the version.",
  raw: "Enter the folder and add up to three files."
};

// The extension Maven would give a file name: tar.gz for a .tar.gz, else the part after the last dot.
function extensionOf(fileName) {
  const lower = fileName.toLowerCase();
  for (const double of ["tar.gz", "tar.bz2", "tar.xz"]) if (lower.endsWith(`.${double}`)) return double;
  const i = fileName.lastIndexOf(".");
  return i > 0 ? fileName.slice(i + 1) : "";
}

function fieldOf(format, spec, n) {
  const name = n ? `${format}.asset${n}.${spec.name}` : `${format}.${spec.name}`;
  const label = LABELS[spec.name] || spec.description || spec.name;
  const help = LABELS[spec.name] ? spec.description || null : null;
  if (spec.type === "BOOLEAN") return { name, label, type: "checkbox", help, wide: true };
  const field = { name, label, type: "text", required: !spec.optional, help, autocomplete: "off", mono: true };
  if (spec.name === "packaging") field.show = (values) => Boolean(getPath(values, `${format}.generate-pom`));
  if (spec.name === "directory") field.wide = true;
  return field;
}

export async function render(ctx) {
  const name = ctx.params.repository;
  let repos;
  try {
    repos = await api.repositories.list();
  } catch (error) {
    failure(ctx.main, error, { retry: () => location.reload() });
    return;
  }
  const repo = (repos || []).find((r) => r.name === name);
  if (!repo) {
    ctx.main.append(notFound("repository"));
    ctx.setTitle("Not found");
    return;
  }
  if (!takesUploads(repo)) {
    ctx.setTitle("Not for uploads");
    ctx.main.append(
      stateView({
        code: `${formatName(repo.format)} ${repo.type}`,
        title: "Not for uploads",
        text: `The repository ${name} is a ${formatName(repo.format)} ${repo.type} repository and takes no uploads here. Uploads go to hosted Maven, npm and raw repositories.`,
        actions: [linkButton("Pick another repository", "#/upload", "primary"), linkButton("Browse its files", treeHref(name, ""))]
      })
    );
    return;
  }
  if (!mayUpload(ctx, repo)) {
    ctx.main.append(noAccess());
    ctx.setTitle("No access");
    return;
  }
  const format = repo.format;
  let spec;
  try {
    spec = await api.upload.specs(format);
  } catch (error) {
    failure(ctx.main, error, { what: "upload form", retry: () => location.reload() });
    return;
  }

  ctx.setTitle(`Upload to ${name}`);
  ctx.main.append(
    pageHead({
      title: `Upload to ${name}`,
      sub: `${formatName(format)} hosted repository. ${ABOUT[format] || ""}`.trim(),
      crumbs: [{ label: "Upload", href: "#/upload" }],
      actions: [linkButton("Browse its files", treeHref(name, ""))]
    })
  );

  const multiple = Boolean(spec.multipleUpload);
  const rows = multiple ? MAX_FILES : 1;
  const componentFields = (spec.componentFields || []).filter((f) => f.type !== "FILE");
  const assetFields = (spec.assetFields || []).filter((f) => f.type !== "FILE");
  let count = 1;

  const fileInputs = [];
  const sections = [];
  if (componentFields.length) {
    const title = componentFields.find((f) => f.group) ? componentFields.find((f) => f.group).group : formatName(format);
    sections.push({ title, grid: true, fields: componentFields.map((f) => fieldOf(format, f)) });
  }
  for (let n = 1; n <= rows; n += 1) {
    const fieldName = multiple ? `${format}.asset${n}` : `${format}.asset`;
    const input = h("input", { type: "file", class: "input file-input", id: uid("file"), name: fieldName, "aria-required": "true", accept: format === "npm" ? ".tgz" : null });
    fileInputs.push(input);
    const nth = n;
    sections.push({
      title: multiple ? `File ${n}` : "Package",
      grid: true,
      show: n === 1 ? undefined : () => count >= nth,
      fields: [
        { name: fieldName, label: format === "npm" ? "Package file" : "File", type: "static", required: true, wide: true, help: format === "npm" ? "A .tgz file." : null, render: () => input },
        ...assetFields.map((f) => fieldOf(format, f, multiple ? n : null))
      ]
    });
  }

  const f = form({ sections });
  const sectionEls = [...f.el.querySelectorAll(":scope > section.form-section")];
  const assetSections = sectionEls.slice(componentFields.length ? 1 : 0);
  assetSections.forEach((el, i) => {
    el.dataset.asset = String(i + 1);
  });

  // The label of each static file field points at the file input, and the input names its help and
  // error text, so the label opens the chooser and a screen reader reads both.
  for (const input of fileInputs) {
    const holder = input.parentElement;
    const label = f.el.querySelector(`label[for="${holder.id}"]`);
    if (label) label.htmlFor = input.id;
    const described = [`${holder.id}-help`, `${holder.id}-error`].filter((id) => document.getElementById(id) || f.el.querySelector(`#${CSS.escape(id)}`));
    input.setAttribute("aria-describedby", described.join(" "));
  }

  // A chosen file fills the field that derives from it, unless the user typed something of their own.
  const derive = { raw: "filename", maven2: "extension" };
  fileInputs.forEach((input, i) => {
    input.addEventListener("change", () => {
      const file = input.files[0];
      const errorEl = input.closest(".field") && input.closest(".field").querySelector(".error-text");
      if (file && errorEl) {
        errorEl.textContent = "";
        input.removeAttribute("aria-invalid");
      }
      const key = derive[format];
      if (!file || !key) return;
      const target = f.el.querySelector(`[name="${format}.asset${i + 1}.${key}"]`);
      if (!target || (target.value && target.dataset.derived !== target.value)) return;
      const derived = key === "filename" ? file.name : extensionOf(file.name);
      target.value = derived;
      target.dataset.derived = derived;
      target.dispatchEvent(new Event("input", { bubbles: true }));
    });
  });

  const result = h("div", { class: "stack", id: "upload-result" });
  const submit = h("button", { type: "submit", class: "btn primary", text: "Upload" });
  const cancel = linkButton("Cancel", "#/upload");
  const add = h("button", { type: "button", class: "btn small", id: "add-asset", text: "Add another file" });
  const drop = h("button", { type: "button", class: "btn small quiet", id: "remove-asset", text: "Remove the last file" });
  const assetActions = multiple ? h("div", { class: "actions asset-actions" }, add, drop) : null;
  const formEl = h("form", { novalidate: true, class: "panel", id: "upload-form", "aria-label": `Upload to ${name}` }, f.el, assetActions, h("div", { class: "form-actions" }, submit, cancel));
  ctx.main.append(result, formEl);
  ctx.setDirty(() => f.isDirty() || fileInputs.some((i) => i.files.length > 0));

  // The form decides which fields show only when a value changes, so the fields of a file row that
  // comes back are shown here. Asset fields have no condition of their own.
  function drawCount() {
    assetSections.forEach((el, i) => {
      el.hidden = i + 1 > count;
      if (!el.hidden) for (const wrapper of el.querySelectorAll(".field, .fieldset")) wrapper.hidden = false;
    });
    add.hidden = count >= rows;
    drop.hidden = count <= 1;
  }
  if (multiple) {
    drawCount();
    add.addEventListener("click", () => {
      if (count >= rows) return;
      count += 1;
      drawCount();
      fileInputs[count - 1].focus();
    });
    drop.addEventListener("click", () => {
      if (count <= 1) return;
      fileInputs[count - 1].value = "";
      count -= 1;
      drawCount();
      add.focus();
    });
  }

  function control(id) {
    return f.el.querySelector(`[name="${CSS.escape(id)}"]`);
  }

  function focusFile(errors) {
    const first = errors[0] && control(errors[0].id);
    if (first && first.type === "file") first.focus();
  }

  // The checks of the form plus a chosen file per shown row, listed in one summary.
  function check() {
    const ok = f.validate();
    const problems = [];
    for (const wrapper of f.el.querySelectorAll(".field")) {
      if (wrapper.closest("[hidden]")) continue;
      const message = (wrapper.querySelector(".error-text") || {}).textContent;
      const el = wrapper.querySelector("[name]");
      if (message && el) problems.push({ id: el.name, message, el });
    }
    fileInputs.slice(0, count).forEach((input) => {
      if (!input.files.length) problems.push({ id: input.name, message: "Choose a file.", el: input });
    });
    if (ok && !problems.length) return true;
    problems.sort((a, b) => (a.el.compareDocumentPosition(b.el) & Node.DOCUMENT_POSITION_FOLLOWING ? -1 : 1));
    f.showServerErrors({ fieldErrors: problems.map(({ id, message }) => ({ id, message })) });
    focusFile(problems);
    return false;
  }

  function body() {
    const values = f.values();
    const data = new FormData();
    const put = (key) => {
      const value = getPath(values, key);
      if (value === undefined || value === null) return;
      if (typeof value === "boolean") data.append(key, value ? "true" : "false");
      else if (String(value).trim() !== "") data.append(key, String(value).trim());
    };
    for (const field of componentFields) put(`${format}.${field.name}`);
    fileInputs.slice(0, count).forEach((input, i) => {
      data.append(input.name, input.files[0], input.files[0].name);
      for (const field of assetFields) put(multiple ? `${format}.asset${i + 1}.${field.name}` : `${format}.${field.name}`);
    });
    return { data, values };
  }

  function folderOf(values) {
    if (format === "raw") return String(getPath(values, "raw.directory") || "").trim();
    if (format === "maven2") {
      const g = String(getPath(values, "maven2.groupId") || "").trim();
      const a = String(getPath(values, "maven2.artifactId") || "").trim();
      const v = String(getPath(values, "maven2.version") || "").trim();
      return g && a && v ? `${g.replace(/\./g, "/")}/${a}/${v}` : "";
    }
    return "";
  }

  formEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    result.replaceChildren();
    if (!check()) return;
    const { data, values } = body();
    const files = count;
    try {
      await busy(submit, () => api.upload.send(name, data));
    } catch (error) {
      if (f.showServerErrors(error)) {
        focusFile(error.fieldErrors);
      } else {
        result.append(problem(error, "Not uploaded"));
        result.scrollIntoView({ block: "nearest" });
      }
      return;
    }
    const folder = folderOf(values);
    f.set({});
    f.clearErrors();
    fileInputs.forEach((input) => {
      input.value = "";
    });
    for (const el of f.el.querySelectorAll("[data-derived]")) delete el.dataset.derived;
    count = 1;
    if (multiple) drawCount();
    result.append(
      notice(
        "success",
        `Uploaded ${plural(files, "file")} to ${name}.`,
        null,
        h("p", {}, h("a", { href: treeHref(name, folder), text: folder ? "Open the folder in the tree" : "Open the repository in the tree" }))
      )
    );
    result.scrollIntoView({ block: "nearest" });
  });
}
