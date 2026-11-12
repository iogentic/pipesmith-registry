// Parts the storage screens share: names, quota words, bars, lists of repositories.

import { h } from "../../dom.js";
import { bytes, formatName, typeName } from "../../format.js";

export const MB = 1048576;

export const NAME = {
  regex: /^[A-Za-z0-9][A-Za-z0-9._-]*$/,
  message: "Use letters, digits, dots, hyphens and underscores, starting with a letter or a digit."
};

export const QUOTA_TYPES = [
  { value: "spaceUsedQuota", label: "The used space exceeds the limit" },
  { value: "spaceRemainingQuota", label: "The free space falls below the limit" }
];

export const POLICY_FORMATS = [
  { value: "*", label: "All formats" },
  { value: "maven2", label: "Maven" },
  { value: "npm", label: "npm" },
  { value: "docker", label: "Docker" },
  { value: "raw", label: "Raw" }
];

// A message of the server as a sentence: first letter upper case, a full stop at the end.
export function sentence(text) {
  const t = String(text || "").trim();
  if (!t) return "";
  return t.charAt(0).toUpperCase() + t.slice(1) + (/[.!?]$/.test(t) ? "" : ".");
}

export function policyFormat(format) {
  return format === "*" ? "All formats" : formatName(format);
}

// "Used space over 1.2 TB" or "Free space under 50.0 GB".
export function quotaText(quota) {
  if (!quota) return "";
  return quota.type === "spaceUsedQuota" ? `Used space over ${bytes(quota.limit)}` : `Free space under ${bytes(quota.limit)}`;
}

// Whether a store of the list has reached its soft quota, from the fields of the list.
export function overQuota(store) {
  const quota = store.softQuota;
  if (!quota || store.unavailable) return false;
  return quota.type === "spaceUsedQuota" ? store.totalSizeInBytes > quota.limit : store.availableSpaceInBytes < quota.limit;
}

// "Not changed for 30 days and not downloaded for 90 days".
export function criteriaText(criteria) {
  const c = criteria || {};
  const parts = [];
  if (c.lastBlobUpdatedDays) parts.push(`not changed for ${c.lastBlobUpdatedDays} ${c.lastBlobUpdatedDays === 1 ? "day" : "days"}`);
  if (c.lastDownloadedDays) parts.push(`not downloaded for ${c.lastDownloadedDays} ${c.lastDownloadedDays === 1 ? "day" : "days"}`);
  const text = parts.join(" and ");
  return text ? text.charAt(0).toUpperCase() + text.slice(1) : "";
}

// A bar of the share of a row, hidden from screen readers: the number beside it says the same.
export function bar(fraction) {
  const fill = h("span");
  const share = Number.isFinite(fraction) ? Math.max(0, Math.min(1, fraction)) : 0;
  fill.style.width = `${(share * 100).toFixed(1)}%`;
  return h("div", { class: "bar", "aria-hidden": "true" }, fill);
}

// A number with its bar, for a table cell.
export function barCell(text, fraction) {
  return h("div", { class: "metric-cell" }, h("span", { class: "num", text }), bar(fraction));
}

// Repositories as a list, each a link to its editor when the user may open it.
// repos: [{name, format, type}]
export function repositoryList(ctx, repos) {
  return h(
    "ul",
    { class: "name-list" },
    repos.map((r) =>
      h(
        "li",
        {},
        r.format && ctx.can(`nexus:repository-admin:${r.format}:${r.name}:read`)
          ? h("a", { href: `#/admin/repositories/${encodeURIComponent(r.name)}`, class: "break", text: r.name })
          : h("span", { class: "break", text: r.name }),
        r.format ? h("span", { class: "muted small", text: ` ${formatName(r.format)} ${typeName(r.type).toLowerCase()}` }) : null
      )
    )
  );
}
