// Forms built from a description: labels, help, conditional fields, validation in the browser,
// errors of the server beside their field, a summary with links to every field in error.
//
// form({ sections: [{ title?, help?, grid?, show?(values), fields: [field] }] | fields: [field],
//        values, readOnly?, onChange?(values) })
//
// field: { name, label, type, required?, help?, placeholder?, options?: [{value, label}] | (values) => [...],
//          min?, max?, integer?, minLength?, pattern?: {regex, message}, validate?(value, values) -> text,
//          readOnly?, show?(values), wide?, mono?, autocomplete?, inputmode?, invert? (checkbox),
//          ordered? (transfer), rows?, render?(value) (static) }
// types: text, password, email, url, number, textarea, checkbox, select, radio, list, transfer, static

import { h, clone, getPath, setPath, uid } from "../dom.js";
import { notice } from "./notice.js";

function deletePath(obj, path) {
  const keys = path.split(".");
  let o = obj;
  for (const k of keys.slice(0, -1)) {
    if (!o || typeof o !== "object") return;
    o = o[k];
  }
  if (o && typeof o === "object") delete o[keys[keys.length - 1]];
}

function isEmpty(value) {
  return value === undefined || value === null || value === "" || (Array.isArray(value) && value.length === 0);
}

function optionList(field, values) {
  const options = typeof field.options === "function" ? field.options(values) : field.options || [];
  return options.map((o) => (typeof o === "object" ? o : { value: String(o), label: String(o) }));
}

// One control per field. Each returns { el, focusEl, get(), set(value), refresh?(values) }.
const controls = {
  input(field, id, describedBy, onInput) {
    const types = { text: "text", password: "password", email: "email", url: "url", number: "number" };
    const input = h("input", {
      class: ["input", field.mono && "mono"],
      id,
      name: field.name,
      type: types[field.type] || "text",
      placeholder: field.placeholder,
      autocomplete: field.autocomplete || (field.type === "password" ? "new-password" : "off"),
      inputmode: field.inputmode || (field.type === "number" ? "numeric" : null),
      min: field.min,
      max: field.max,
      step: field.type === "number" ? field.step || 1 : null,
      readonly: field.readOnly,
      spellcheck: field.mono ? "false" : null,
      "aria-describedby": describedBy,
      "aria-required": field.required ? "true" : null
    });
    input.addEventListener("input", onInput);
    return {
      el: input,
      focusEl: input,
      get() {
        if (field.type === "number") {
          if (input.value.trim() === "") return null;
          const n = Number(input.value);
          return Number.isFinite(n) ? n : input.value;
        }
        return input.value;
      },
      set(value) {
        input.value = value === null || value === undefined ? "" : String(value);
      }
    };
  },

  textarea(field, id, describedBy, onInput) {
    const area = h("textarea", {
      class: ["textarea", field.mono && "mono"],
      id,
      name: field.name,
      rows: field.rows || 4,
      placeholder: field.placeholder,
      readonly: field.readOnly,
      "aria-describedby": describedBy,
      "aria-required": field.required ? "true" : null
    });
    area.addEventListener("input", onInput);
    return { el: area, focusEl: area, get: () => area.value, set: (v) => (area.value = v ?? "") };
  },

  checkbox(field, id, describedBy, onInput) {
    const box = h("input", { type: "checkbox", id, name: field.name, disabled: field.readOnly, "aria-describedby": describedBy });
    box.addEventListener("change", onInput);
    const el = h("label", { class: "check", for: id }, box, h("span", { class: "label", text: field.label }));
    return {
      el,
      focusEl: box,
      labelled: true,
      get: () => (field.invert ? !box.checked : box.checked),
      set: (v) => (box.checked = field.invert ? !v : Boolean(v))
    };
  },

  select(field, id, describedBy, onInput, values) {
    const select = h("select", { class: "select", id, name: field.name, disabled: field.readOnly, "aria-describedby": describedBy, "aria-required": field.required ? "true" : null });
    let current = "";
    const fill = (vals) => {
      const opts = optionList(field, vals);
      select.replaceChildren(
        ...(field.required && !field.noBlank ? [h("option", { value: "", text: field.blank || "Choose" })] : field.blank ? [h("option", { value: "", text: field.blank })] : []),
        ...opts.map((o) => h("option", { value: o.value, text: o.label }))
      );
      select.value = current;
      if (select.value !== current) select.value = "";
    };
    fill(values);
    select.addEventListener("change", () => {
      current = select.value;
      onInput();
    });
    return {
      el: select,
      focusEl: select,
      get: () => (select.value === "" ? null : select.value),
      set(v) {
        current = v === null || v === undefined ? "" : String(v);
        select.value = current;
      },
      refresh: fill
    };
  },

  radio(field, id, describedBy, onInput, values) {
    const group = h("div", { class: "radios", role: "radiogroup", id, "aria-describedby": describedBy });
    const inputs = optionList(field, values).map((o, i) => {
      const rid = `${id}-${i}`;
      const input = h("input", { type: "radio", name: id, id: rid, value: o.value, disabled: field.readOnly });
      input.addEventListener("change", onInput);
      group.append(h("label", { class: "check", for: rid }, input, h("span", { class: "label", text: o.label })));
      return input;
    });
    return {
      el: group,
      focusEl: inputs[0],
      get: () => (inputs.find((i) => i.checked) || {}).value ?? null,
      set: (v) => inputs.forEach((i) => (i.checked = i.value === String(v)))
    };
  },

  list(field, id, describedBy, onInput) {
    const rows = h("div");
    const add = h("button", { type: "button", class: "btn small", text: field.addLabel || "Add another", disabled: field.readOnly });
    const el = h("div", { id, "aria-describedby": describedBy }, rows, h("div", { class: "actions", dataset: { listActions: "" } }, add));
    const addRow = (value = "") => {
      const n = rows.childElementCount + 1;
      const input = h("input", { class: ["input", field.mono && "mono"], value, readonly: field.readOnly, "aria-label": `${field.label} ${n}`, spellcheck: "false", autocomplete: "off" });
      input.addEventListener("input", onInput);
      const remove = h("button", { type: "button", class: "btn small quiet", text: "Remove", "aria-label": `Remove ${field.label} ${n}`, disabled: field.readOnly });
      const row = h("div", { class: "repeat-row" }, input, remove);
      remove.addEventListener("click", () => {
        row.remove();
        if (!rows.childElementCount) addRow();
        onInput();
      });
      rows.append(row);
      return input;
    };
    add.addEventListener("click", () => {
      addRow().focus();
    });
    addRow();
    return {
      el,
      focusEl: () => rows.querySelector("input"),
      get: () => [...rows.querySelectorAll("input")].map((i) => i.value.trim()).filter(Boolean),
      set(v) {
        rows.replaceChildren();
        const list = Array.isArray(v) && v.length ? v : [""];
        list.forEach((item) => addRow(item));
      }
    };
  },

  transfer(field, id, describedBy, onInput, values) {
    let selected = [];
    let all = optionList(field, values);
    const filterId = `${id}-filter`;
    const availId = `${id}-available`;
    const filter = h("input", { class: "input", id: filterId, type: "search", placeholder: "Filter", autocomplete: "off", disabled: field.readOnly });
    const available = h("select", { class: "select", id: availId, multiple: true, size: 8, disabled: field.readOnly });
    const chosen = h("select", { class: "select", id, multiple: true, size: 8, disabled: field.readOnly, "aria-describedby": describedBy });
    const label = (value) => (all.find((o) => o.value === value) || { label: value }).label;
    const draw = () => {
      const f = filter.value.trim().toLowerCase();
      available.replaceChildren(
        ...all.filter((o) => !selected.includes(o.value) && (!f || o.label.toLowerCase().includes(f) || o.value.toLowerCase().includes(f))).map((o) => h("option", { value: o.value, text: o.label }))
      );
      chosen.replaceChildren(...selected.map((v) => h("option", { value: v, text: label(v) })));
    };
    const picked = (select) => [...select.selectedOptions].map((o) => o.value);
    const addSel = () => {
      const vals = picked(available);
      if (!vals.length) return;
      selected = selected.concat(vals.filter((v) => !selected.includes(v)));
      draw();
      onInput();
    };
    const removeSel = () => {
      const vals = picked(chosen);
      if (!vals.length) return;
      selected = selected.filter((v) => !vals.includes(v));
      draw();
      onInput();
    };
    const move = (step) => {
      const vals = picked(chosen);
      if (vals.length !== 1) return;
      const i = selected.indexOf(vals[0]);
      const j = i + step;
      if (j < 0 || j >= selected.length) return;
      [selected[i], selected[j]] = [selected[j], selected[i]];
      draw();
      chosen.value = vals[0];
      onInput();
    };
    filter.addEventListener("input", draw);
    available.addEventListener("dblclick", addSel);
    chosen.addEventListener("dblclick", removeSel);
    const btn = (text, fn, aria) => {
      const b = h("button", { type: "button", class: "btn small", text, "aria-label": aria, disabled: field.readOnly });
      b.addEventListener("click", fn);
      return b;
    };
    const moves = h(
      "div",
      { class: "transfer-moves" },
      btn("Add", addSel, `Add the marked entries to ${field.selectedLabel || "Selected"}`),
      btn("Remove", removeSel, `Remove the marked entries from ${field.selectedLabel || "Selected"}`),
      field.ordered ? btn("Up", () => move(-1), "Move the marked entry up") : null,
      field.ordered ? btn("Down", () => move(1), "Move the marked entry down") : null
    );
    const el = h(
      "div",
      { class: "transfer" },
      h(
        "div",
        { class: "transfer-side" },
        h("label", { class: "title", for: availId, text: field.availableLabel || "Available" }),
        h("label", { class: "visually-hidden", for: filterId, text: `Filter ${field.availableLabel || "Available"}` }),
        filter,
        available
      ),
      moves,
      h("div", { class: "transfer-side" }, h("label", { class: "title", for: id, text: field.selectedLabel || "Selected" }), chosen)
    );
    draw();
    return {
      el,
      focusEl: available,
      get: () => [...selected],
      set(v) {
        selected = Array.isArray(v) ? [...v] : [];
        draw();
      },
      refresh(vals) {
        all = optionList(field, vals);
        draw();
      }
    };
  },

  static(field, id) {
    const out = h("div", { id, class: ["static", field.mono && "mono", "break"] });
    let current = null;
    return {
      el: out,
      focusEl: null,
      get: () => current,
      set(v) {
        current = v;
        out.replaceChildren(field.render ? field.render(v) : isEmpty(v) ? h("span", { class: "muted", text: "None" }) : String(v));
      }
    };
  }
};

const KIND = { text: "input", password: "input", email: "input", url: "input", number: "input" };

export function form({ sections, fields, values = {}, readOnly = false, onChange } = {}) {
  const groups = sections || [{ fields }];
  let base = clone(values) || {};
  const entries = [];
  const sectionEls = [];
  const summary = h("div", { class: "form-summary" });
  const root = h("div", { class: "form" }, summary);
  let dirty = false;

  const changed = () => {
    dirty = true;
    refresh();
    if (onChange) onChange(current());
  };

  for (const group of groups) {
    const body = h("div", { class: group.grid ? "form-grid" : "form-stack" });
    const sectionEl = h(
      "section",
      { class: "form-section" },
      group.title ? h("h2", { text: group.title }) : null,
      group.help ? h("p", { class: "help", text: group.help }) : null,
      body
    );
    sectionEls.push({ group, el: sectionEl });
    root.append(sectionEl);
    for (const field of group.fields) {
      const f = { ...field, readOnly: field.readOnly || readOnly };
      const id = uid("f");
      const helpId = f.help ? `${id}-help` : null;
      const errorId = `${id}-error`;
      const describedBy = [helpId, errorId].filter(Boolean).join(" ");
      const kind = KIND[f.type] || f.type || "input";
      const control = controls[kind](f, id, describedBy, changed, base);
      const error = h("div", { class: "error-text", id: errorId });
      const isGroup = kind === "radio" || kind === "transfer" || kind === "list";
      let wrapper;
      const labelContent = [f.label, f.required && !f.readOnly ? h("span", { class: "req", "aria-hidden": "true", text: "*" }) : null];
      if (control.labelled) {
        wrapper = h("div", { class: ["field", f.wide && "wide"] }, control.el, f.help ? h("div", { class: "help", id: helpId, text: f.help }) : null, error);
      } else if (isGroup) {
        wrapper = h(
          "fieldset",
          { class: ["fieldset", f.wide && "wide"], "aria-describedby": describedBy },
          h("legend", {}, labelContent),
          f.help ? h("div", { class: "help", id: helpId, text: f.help }) : null,
          control.el,
          error
        );
      } else {
        wrapper = h(
          "div",
          { class: ["field", f.wide && "wide"] },
          h("label", { class: "label", for: id }, labelContent),
          control.el,
          f.help ? h("div", { class: "help", id: helpId, text: f.help }) : null,
          error
        );
      }
      body.append(wrapper);
      const initial = getPath(base, f.name);
      control.set(initial === undefined && f.default !== undefined ? f.default : initial);
      entries.push({ field: f, control, wrapper, error, id });
    }
  }

  function shown(entry, vals) {
    const section = sectionEls.find((s) => s.group.fields.includes(entry.field) || s.group.fields.some((x) => x.name === entry.field.name));
    if (section && section.group.show && !section.group.show(vals)) return false;
    return !entry.field.show || entry.field.show(vals);
  }

  function current() {
    const out = clone(base) || {};
    for (const entry of entries) {
      if (entry.field.type === "static") continue;
      setPath(out, entry.field.name, entry.control.get());
    }
    return out;
  }

  function refresh() {
    const vals = current();
    for (const { group, el } of sectionEls) el.hidden = Boolean(group.show && !group.show(vals));
    for (const entry of entries) {
      entry.wrapper.hidden = !shown(entry, vals);
      if (entry.control.refresh) entry.control.refresh(vals);
    }
  }

  function setError(entry, message) {
    entry.error.textContent = message || "";
    const target = typeof entry.control.focusEl === "function" ? entry.control.focusEl() : entry.control.focusEl;
    const inputs = entry.wrapper.querySelectorAll("input, select, textarea");
    inputs.forEach((el) => {
      if (message) el.setAttribute("aria-invalid", "true");
      else el.removeAttribute("aria-invalid");
    });
    return target;
  }

  function focusEntry(entry) {
    const target = typeof entry.control.focusEl === "function" ? entry.control.focusEl() : entry.control.focusEl;
    if (target) target.focus();
    entry.wrapper.scrollIntoView({ block: "center" });
  }

  function showSummary(items, extra = []) {
    summary.replaceChildren();
    if (!items.length && !extra.length) return;
    const list = h(
      "ul",
      {},
      items.map(({ entry, message }) => {
        const link = h("button", { type: "button", class: "link-like", text: `${entry.field.label}: ${message}` });
        link.addEventListener("click", () => focusEntry(entry));
        return h("li", {}, link);
      }),
      extra.map((message) => h("li", { text: message }))
    );
    const count = items.length + extra.length;
    summary.append(notice("error", count === 1 ? "One field needs a change" : `${count} fields need a change`, null, list));
  }

  function check(entry, vals) {
    const f = entry.field;
    const value = entry.control.get();
    if (f.type === "static" || f.readOnly) return null;
    if (f.required && f.type !== "checkbox" && isEmpty(value)) return f.type === "transfer" ? "Pick at least one." : "This field is required.";
    if (isEmpty(value)) return null;
    if (f.type === "number") {
      if (typeof value !== "number") return "Enter a number.";
      if (f.integer !== false && !Number.isInteger(value)) return "Enter a whole number.";
      if (f.min !== undefined && value < f.min) return `The smallest allowed value is ${f.min}.`;
      if (f.max !== undefined && value > f.max) return `The largest allowed value is ${f.max}.`;
    }
    if (f.minLength && String(value).length < f.minLength) return `Enter at least ${f.minLength} characters.`;
    if (f.pattern && typeof value === "string" && !f.pattern.regex.test(value)) return f.pattern.message;
    if (f.type === "url" && typeof value === "string" && !/^https?:\/\/[^\s/]+/i.test(value)) return "Enter an address that starts with http:// or https://.";
    if (f.type === "email" && typeof value === "string" && !/^[^\s@]+@[^\s@]+$/.test(value)) return "Enter a mail address.";
    if (f.validate) return f.validate(value, vals) || null;
    return null;
  }

  function validate() {
    const vals = current();
    const problems = [];
    for (const entry of entries) {
      const message = shown(entry, vals) ? check(entry, vals) : null;
      setError(entry, message);
      if (message) problems.push({ entry, message });
    }
    showSummary(problems);
    if (problems.length) focusEntry(problems[0].entry);
    return problems.length === 0;
  }

  // Shows the field errors of a 400 answer. Returns true when something was shown.
  function showServerErrors(error) {
    if (!error || !error.fieldErrors || !error.fieldErrors.length) return false;
    const vals = current();
    const items = [];
    const extra = [];
    for (const { id, message } of error.fieldErrors) {
      const path = id.replace(/\[\d+\]$/, "");
      const entry = entries.find((e) => e.field.name === path && shown(e, vals)) || entries.find((e) => e.field.name.endsWith(`.${path}`) && shown(e, vals));
      if (entry) {
        setError(entry, message);
        items.push({ entry, message });
      } else {
        extra.push(message);
      }
    }
    showSummary(items, extra);
    if (items.length) focusEntry(items[0].entry);
    else summary.scrollIntoView({ block: "center" });
    return true;
  }

  refresh();

  return {
    el: root,
    values: () => {
      const vals = current();
      for (const entry of entries) if (!shown(entry, vals)) deletePath(vals, entry.field.name);
      return vals;
    },
    set(next) {
      base = clone(next) || {};
      for (const entry of entries) entry.control.set(getPath(base, entry.field.name));
      dirty = false;
      refresh();
    },
    validate,
    showServerErrors,
    clearErrors() {
      summary.replaceChildren();
      entries.forEach((e) => setError(e, null));
    },
    focusFirst() {
      const entry = entries.find((e) => !e.wrapper.hidden && !e.field.readOnly && e.field.type !== "static");
      if (entry) focusEntry(entry);
    },
    isDirty: () => dirty,
    markClean() {
      dirty = false;
    }
  };
}
