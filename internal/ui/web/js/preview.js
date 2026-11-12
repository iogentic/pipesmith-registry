// Preview of a server that does not answer every path of the contract yet. After the sign in the
// interface asks each area once for its main read request. An area whose request answers 501, or
// 404 on a path of the contract, is "not in this preview": the navigation marks it and its screens
// show a calm state instead of an error. An action that answers 501 shows a calm notice.

import { api } from "./api.js";

const today = () => new Date().toISOString().slice(0, 10);

// The read request of each screen that has one of its own. Detail screens follow their parent.
export const PROBES = {
  S03: () => api.search({ q: "pipesmith-preview-probe" }),
  S04: () => api.repositories.list(),
  S06: () => api.upload.specs("raw"),
  S09: () => api.repositories.settings(),
  S12: () => api.blobStores.list(),
  S14: () => api.cleanupPolicies.list(),
  S16: () => api.routingRules.list(),
  S18: () => api.routingRules.preview("all", "/"),
  S19: () => api.privileges.list(),
  S21: () => api.roles.list(),
  S23: () => api.userSources(),
  S25: () => api.anonymous.get(),
  S26: () => api.ldap.list(),
  S28: () => api.realms.available(),
  S29: () => api.tokens.list(),
  S30: () => api.logging.loggers(),
  S31: () => api.logs.list(),
  S33: () => api.statusChecks(),
  S34: () => api.systemInformation(),
  S35: () => api.capabilities.list(),
  S37: () => api.node(),
  S38: () => api.tasks.list(),
  S43: () => api.metrics.requests(today(), today())
};

const results = new Map();
// Paths whose probe answered 404 or 501; a 404 on one of them anywhere is "not in this preview".
const previewPaths = new Set();
const listeners = [];
let current = null;

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

// Asks every area the user may open, at most once per page load.
export async function probe(routes, can) {
  const ids = Object.keys(PROBES).filter((id) => {
    const route = routes.find((r) => r.id === id);
    return route && can(route.perm) && !results.has(id);
  });
  await Promise.all(
    ids.map(async (id) => {
      try {
        await PROBES[id]();
        results.set(id, "available");
      } catch (error) {
        const later = error.status === 501 || error.status === 404;
        results.set(id, later ? "preview" : "available");
        if (later && error.path) previewPaths.add(error.path);
      }
    })
  );
  for (const fn of listeners) fn();
}

export function reset() {
  results.clear();
  previewPaths.clear();
}

export const PREVIEW_TITLE = "Not in this preview";
export const PREVIEW_TEXT = "This server does not offer this function yet. It comes with a later step of the server.";
