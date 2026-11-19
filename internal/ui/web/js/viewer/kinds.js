// Kinds of files the viewer colours (docs/plans/browse-tools.md, section Viewer Parts). The ending
// decides; without a known ending the content type decides, and the rest is plain text.

export const KINDS = [
  { kind: "xml", label: "XML", endings: ["xml", "pom", "xsd", "xsl", "wsdl"], types: ["application/xml", "text/xml"], suffix: "+xml" },
  { kind: "json", label: "JSON", endings: ["json", "module"], types: ["application/json"], suffix: "+json" },
  { kind: "toml", label: "TOML", endings: ["toml"], types: ["application/toml"] },
  { kind: "keyvalue", label: "Key value", endings: ["properties", "cfg", "ini", "conf"], types: [] },
  { kind: "yaml", label: "YAML", endings: ["yaml", "yml"], types: ["application/yaml", "application/x-yaml", "text/yaml"] },
  { kind: "markdown", label: "Markdown", endings: ["md", "markdown"], types: ["text/markdown"] },
  { kind: "asciidoc", label: "AsciiDoc", endings: ["adoc", "asciidoc"], types: [] }
];

// The bytes the text test reads: a NUL byte among them means binary.
export const PROBE_BYTES = 8192;

// The ending of a file name in lower case, or "" for a name without one. A name that starts with its
// only dot, such as ".pom", has no ending.
export function endingOf(name) {
  const base = String(name || "").split("/").pop();
  const dot = base.lastIndexOf(".");
  return dot > 0 ? base.slice(dot + 1).toLowerCase() : "";
}

// The media type of a content type without its parameters, in lower case.
export function mediaType(contentType) {
  return String(contentType || "").split(";")[0].trim().toLowerCase();
}

function byEnding(name) {
  const ending = endingOf(name);
  return ending ? KINDS.find((k) => k.endings.includes(ending)) || null : null;
}

function byType(contentType) {
  const type = mediaType(contentType);
  if (!type) return null;
  return KINDS.find((k) => k.types.includes(type) || (k.suffix && type.endsWith(k.suffix))) || null;
}

// The kind of a file: "xml", "json", "toml", "keyvalue", "yaml", "markdown", "asciidoc" or "text".
export function kindOf(name, contentType) {
  const found = byEnding(name) || byType(contentType);
  return found ? found.kind : "text";
}

// The label of a kind for the status line.
export function kindLabel(kind) {
  const found = KINDS.find((k) => k.kind === kind);
  return found ? found.label : "Plain text";
}

// Whether a content type names text without a request: text/*, a type of the table, +json or +xml.
export function textType(contentType) {
  const type = mediaType(contentType);
  return type.startsWith("text/") || byType(type) !== null;
}

// The byte order mark of UTF-16, or null.
export function utf16Of(bytes) {
  if (bytes.length >= 2 && bytes[0] === 0xff && bytes[1] === 0xfe) return "utf-16le";
  if (bytes.length >= 2 && bytes[0] === 0xfe && bytes[1] === 0xff) return "utf-16be";
  return null;
}

// Text or binary from the first bytes of a file: UTF-16 with its mark is text, else a NUL byte in
// the first 8 KiB means binary.
export function isText(bytes) {
  if (utf16Of(bytes)) return true;
  const end = Math.min(bytes.length, PROBE_BYTES);
  for (let i = 0; i < end; i += 1) if (bytes[i] === 0) return false;
  return true;
}

// The rule of the View button (call B4): the file has a download address, and its ending is in the
// table or its content type names text.
export function showsView(info) {
  if (!info || !info.download_url) return false;
  return byEnding(info.name) !== null || textType(info.content_type);
}

// The route of the viewer (S46) with a file or folder open. The slashes of the path stay readable.
export function viewHref(repository, path) {
  const base = `#/view/${encodeURIComponent(repository)}`;
  const clean = String(path || "").replace(/^\/+|\/+$/g, "");
  return clean ? `${base}?path=${encodeURIComponent(clean).replace(/%2F/g, "/")}` : base;
}
