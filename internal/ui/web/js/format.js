// Formatting of numbers, sizes and times for display.

export function number(value) {
  if (value === null || value === undefined || value === "") return "";
  return Number(value).toLocaleString("en-US");
}

export function bytes(value) {
  if (value === null || value === undefined || value === "") return "";
  const n = Number(value);
  if (!Number.isFinite(n)) return "";
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB", "PB"];
  let v = n;
  let i = -1;
  do {
    v /= 1024;
    i += 1;
  } while (v >= 1024 && i < units.length - 1);
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}

function pad(n) {
  return String(n).padStart(2, "0");
}

// A time as YYYY-MM-DD HH:MM in the time zone of the browser. Takes an ISO string or milliseconds.
export function time(value) {
  if (value === null || value === undefined || value === "") return "";
  const d = typeof value === "number" ? new Date(value) : new Date(String(value));
  if (Number.isNaN(d.getTime())) return String(value);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function day(value) {
  return time(value).slice(0, 10);
}

export function plural(n, one, many) {
  return `${number(n)} ${n === 1 ? one : many || `${one}s`}`;
}

export function titleCase(word) {
  if (!word) return "";
  return word.charAt(0).toUpperCase() + word.slice(1);
}

// Words of a field name that are written in capitals.
const CAPITALS = new Set(["id", "url", "uri", "ip", "dn", "md5", "sha1", "sha256", "sha512", "http", "https", "ldap", "api", "json", "xml"]);

// A label for a field name that has none of its own: group_id, artifactId and kind become
// "Group ID", "Artifact ID" and "Kind", so a field the interface does not know never shows raw.
export function fieldLabel(key) {
  const words = String(key ?? "")
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .split(/[\s_.-]+/)
    .filter(Boolean)
    .map((w) => w.toLowerCase());
  if (!words.length) return "Field";
  return words.map((w, i) => (CAPITALS.has(w) ? w.toUpperCase() : i ? w : titleCase(w))).join(" ");
}

// The formats the server can hold, one per format table of its database (ui/checks/check_formats.py).
export const FORMATS = {
  apt: "APT",
  cargo: "Cargo",
  cocoapods: "CocoaPods",
  composer: "Composer",
  conan: "Conan",
  conda: "Conda",
  docker: "Docker",
  gitlfs: "Git LFS",
  go: "Go",
  helm: "Helm",
  huggingface: "Hugging Face",
  maven2: "Maven",
  npm: "npm",
  nuget: "NuGet",
  p2: "p2",
  pypi: "PyPI",
  r: "R",
  raw: "Raw",
  rubygems: "RubyGems",
  yum: "Yum"
};
export const TYPES = { hosted: "Hosted", proxy: "Proxy", group: "Group" };

export function formatName(format) {
  return FORMATS[format] || titleCase(format || "");
}

export function typeName(type) {
  return TYPES[type] || titleCase(type);
}
