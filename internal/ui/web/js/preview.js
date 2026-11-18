// Preview of a server that does not answer every path of the contract yet. After the sign in the
// interface learns for each area whether the server serves its main read request. With an answer
// of GET /api/areas it reads that from the refused requests and sends nothing more. Without that
// answer it sends one probe per area, and the page does not wait for them. An area whose request
// is refused, or answers 501, or 404 on a path of the contract, is "not in this preview": the
// navigation marks it and its screens show a calm state instead of an error. An action that
// answers 501 shows a calm notice.

import { api } from "./api.js";

const today = () => new Date().toISOString().slice(0, 10);

// The read request of each screen that has one of its own, and the probe that sends it. Detail
// screens follow their parent. The search screen has no probe: a search without a hit reads every
// row of the index.
export const PROBES = {
  S03: { request: "GET /service/rest/v1/search" },
  S04: { request: "GET /service/rest/v1/repositories", probe: () => api.repositories.list() },
  S06: { request: "GET /service/rest/v1/formats/raw/upload-specs", probe: () => api.upload.specs("raw") },
  S09: { request: "GET /service/rest/v1/repositorySettings", probe: () => api.repositories.settings() },
  S12: { request: "GET /service/rest/v1/blobstores", probe: () => api.blobStores.list() },
  S14: { request: "GET /api/cleanup-policies", probe: () => api.cleanupPolicies.list() },
  S16: { request: "GET /service/rest/v1/routing-rules", probe: () => api.routingRules.list() },
  S18: { request: "POST /api/routing-rules/preview", probe: () => api.routingRules.preview("all", "/") },
  S19: { request: "GET /service/rest/v1/security/privileges", probe: () => api.privileges.list() },
  S21: { request: "GET /service/rest/v1/security/roles", probe: () => api.roles.list() },
  S23: { request: "GET /service/rest/v1/security/user-sources", probe: () => api.userSources() },
  S25: { request: "GET /service/rest/v1/security/anonymous", probe: () => api.anonymous.get() },
  S26: { request: "GET /service/rest/v1/security/ldap", probe: () => api.ldap.list() },
  S28: { request: "GET /service/rest/v1/security/realms/available", probe: () => api.realms.available() },
  S29: { request: "GET /api/tokens", probe: () => api.tokens.list() },
  S30: { request: "GET /api/logging/loggers", probe: () => api.logging.loggers() },
  S31: { request: "GET /api/logs", probe: () => api.logs.list() },
  S33: { request: "GET /service/rest/v1/status/check", probe: () => api.statusChecks() },
  S34: { request: "GET /service/rest/beta/system/information", probe: () => api.systemInformation() },
  S35: { request: "GET /service/rest/v1/capabilities", probe: () => api.capabilities.list() },
  S37: { request: "GET /service/rest/v1/system/node", probe: () => api.node() },
  S38: { request: "GET /service/rest/v1/tasks", probe: () => api.tasks.list() },
  S43: { request: "GET /api/metrics/requests", probe: () => api.metrics.requests(today(), today()) }
};

const results = new Map();
// Paths whose probe answered 404 or 501; a 404 on one of them anywhere is "not in this preview".
const previewPaths = new Set();
const listeners = [];
let current = null;
// Counts the resets: a probe that answers after a sign out marks nothing.
let round = 0;

function parentOf(route) {
  return route && route.parent ? route.parent : null;
}

let routeTable = [];

export function setRoutes(routes) {
  routeTable = routes;
}

// True when the screen or the screen it belongs to is not in this preview.
export function unavailable(id) {
  if (!id) return false;
  if (results.get(id) === "preview") return true;
  const route = routeTable.find((r) => r.id === id);
  const parent = parentOf(route);
  return parent ? results.get(parent) === "preview" : false;
}

export function setCurrent(id) {
  current = id;
}

export function isPreviewError(error) {
  if (!error) return false;
  if (error.status === 501) return true;
  return error.status === 404 && (unavailable(current) || previewPaths.has(error.path));
}

// A screen learns that it is not in this preview when its first request answers so.
export function learn(id, error) {
  if (!id || !error || !(error.status === 501 || error.status === 404)) return;
  if (results.get(id) === "preview") return;
  results.set(id, "preview");
  for (const fn of listeners) fn();
}

export function learnCurrent(error) {
  learn(current, error);
}

export function onChange(fn) {
  listeners.push(fn);
}

export function anyUnavailable() {
  return [...results.values()].includes("preview");
}

// The screens of PROBES the user may open that have no result yet.
function pending(routes, can) {
  return Object.keys(PROBES).filter((id) => {
    const route = routes.find((r) => r.id === id);
    return route && can(route.perm) && !results.has(id);
  });
}

// True when an entry of the refused list of GET /api/areas names the request. The match is the one
// of api.js: the same method and as many segments, each equal or a {name} of the entry.
function refuses(entry, method, path) {
  if (!entry || typeof entry.method !== "string" || typeof entry.path !== "string") return false;
  const want = entry.path.split("/");
  const parts = path.split("/");
  return entry.method.toUpperCase() === method && want.length === parts.length && want.every((p, i) => (/^\{[^/{}]+\}$/.test(p) ? parts[i] !== "" : p === parts[i]));
}

// Marks every area the user may open from the refused requests of GET /api/areas, as its probe
// would. An area whose request is refused is not in this preview, and every other area is served.
// It sends no request.
export function fromAreas(routes, can, refused) {
  const list = Array.isArray(refused) ? refused : [];
  for (const id of pending(routes, can)) {
    const [method, path] = PROBES[id].request.split(" ");
    const later = list.some((entry) => refuses(entry, method, path));
    results.set(id, later ? "preview" : "available");
    if (later) previewPaths.add(path);
  }
  for (const fn of listeners) fn();
}

// Asks every area the user may open that has a probe, at most once per page load. The page does
// not wait for the answers: each marks its area as it arrives, and a probe that fails or never
// answers holds nothing back. The promise settles when every probe has answered.
export function probe(routes, can) {
  const asked = round;
  const ids = pending(routes, can).filter((id) => PROBES[id].probe);
  return Promise.all(
    ids.map(async (id) => {
      let later = false;
      let path = null;
      try {
        await PROBES[id].probe();
      } catch (error) {
        later = error.status === 501 || error.status === 404;
        path = error.path;
      }
      if (asked !== round) return;
      if (later && path) previewPaths.add(path);
      // The first result stands: the screen may have learned it from its own request meanwhile.
      if (results.has(id)) return;
      results.set(id, later ? "preview" : "available");
      if (!later) return;
      for (const fn of listeners) fn();
    })
  );
}

export function reset() {
  round += 1;
  results.clear();
  previewPaths.clear();
}

export const PREVIEW_TITLE = "Not in this preview";
export const PREVIEW_TEXT = "This server does not offer this function yet. It comes with a later step of the server.";
