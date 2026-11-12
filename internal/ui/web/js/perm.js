// The wildcard rule of the permission strings the server keeps from the old product:
// parts split by ":", alternatives inside a part by ",", "*" matches any value, missing trailing
// parts of a granted permission count as "*", extra parts must all be "*". Case does not matter.

function parts(permission) {
  return String(permission)
    .toLowerCase()
    .split(":")
    .map((p) => new Set(p.split(",").map((s) => s.trim())));
}

// The rule of the server: does the granted permission cover the whole required one?
export function implies(granted, required) {
  const g = parts(granted);
  const r = parts(required);
  for (let i = 0; i < r.length; i += 1) {
    if (i >= g.length) return true;
    if (g[i].has("*")) continue;
    for (const value of r[i]) if (!g[i].has(value)) return false;
  }
  for (let i = r.length; i < g.length; i += 1) if (!g[i].has("*")) return false;
  return true;
}

// The rule of the interface: does the granted permission cover at least one case of the required
// one? A "*" in the required permission of a screen means "any value": a user who may administer one
// repository may open the list of repositories, and the server shows that one.
export function overlaps(granted, required) {
  const g = parts(granted);
  const r = parts(required);
  const length = Math.max(g.length, r.length);
  for (let i = 0; i < length; i += 1) {
    const gp = g[i];
    const rp = r[i];
    if (!gp || !rp || gp.has("*") || rp.has("*")) continue;
    let common = false;
    for (const value of rp) if (gp.has(value)) common = true;
    if (!common) return false;
  }
  return true;
}

export function permitted(permissions, required) {
  if (!required) return true;
  const list = Array.isArray(required) ? required : [required];
  return list.every((req) => (permissions || []).some((g) => overlaps(g, req)));
}
