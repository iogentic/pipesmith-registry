// Parts the browse screens share: which repositories take uploads, and the address of a path in
// the tree of S05.

export const UPLOAD_FORMATS = ["maven2", "npm", "raw"];

// Hosted repositories of the formats the upload form serves (decision U21).
export function takesUploads(repo) {
  return Boolean(repo) && repo.type === "hosted" && UPLOAD_FORMATS.includes(repo.format);
}

// Whether the session may upload to the repository: both permissions of docs/ui/API.md, section S07.
export function mayUpload(ctx, repo) {
  return takesUploads(repo) && ctx.can("nexus:component:add") && ctx.can(`nexus:repository-view:${repo.format}:${repo.name}:add`);
}

// The route of S05 with a file or folder open. The slashes of the path stay readable.
export function treeHref(repository, path) {
  const base = `#/browse/${encodeURIComponent(repository)}`;
  const clean = String(path || "").replace(/^\/+|\/+$/g, "");
  return clean ? `${base}?path=${encodeURIComponent(clean).replace(/%2F/g, "/")}` : base;
}
