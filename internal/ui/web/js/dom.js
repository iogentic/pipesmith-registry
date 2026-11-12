// Small DOM helpers. Text always goes in as text nodes, never as markup, and no element gets a
// style attribute: the content security policy of the server allows neither.

const PROPS = new Set(["value", "checked", "disabled", "selected", "multiple", "indeterminate"]);

export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  if (props) {
    for (const [key, value] of Object.entries(props)) {
      if (value === undefined || value === null || value === false) continue;
      if (key === "style") throw new Error("style attributes are not allowed");
      if (key === "class") {
        el.className = Array.isArray(value) ? value.filter(Boolean).join(" ") : value;
      } else if (key === "text") {
        el.textContent = String(value);
      } else if (key === "on") {
        for (const [event, handler] of Object.entries(value)) el.addEventListener(event, handler);
      } else if (key === "dataset") {
        for (const [k, v] of Object.entries(value)) if (v !== undefined && v !== null) el.dataset[k] = String(v);
      } else if (PROPS.has(key)) {
        el[key] = value;
      } else {
        el.setAttribute(key, value === true ? "" : String(value));
      }
    }
  }
  append(el, children);
  return el;
}

export function append(el, children) {
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    el.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return el;
}

export function clear(el) {
  while (el.firstChild) el.firstChild.remove();
  return el;
}

export function replace(el, ...children) {
  clear(el);
  return append(el, children);
}

let counter = 0;
export function uid(prefix = "id") {
  counter += 1;
  return `${prefix}-${counter}`;
}

export function getPath(obj, path) {
  return path.split(".").reduce((o, k) => (o === null || o === undefined ? undefined : o[k]), obj);
}

export function setPath(obj, path, value) {
  const keys = path.split(".");
  let o = obj;
  for (const k of keys.slice(0, -1)) {
    if (o[k] === null || typeof o[k] !== "object") o[k] = {};
    o = o[k];
  }
  o[keys[keys.length - 1]] = value;
  return obj;
}

export function clone(value) {
  return value === undefined ? undefined : JSON.parse(JSON.stringify(value));
}

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (e) {
    return false;
  }
}

export function download(name, text, type = "application/json") {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = h("a", { href: url, download: name, class: "visually-hidden" });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
