// The only module that talks to the server. Every path of docs/ui/API.md is named here once.
// REST paths keep the shape of the public REST API of the old product; /api paths are the server's
// own, existing (port step 2) or proposed in docs/ui/API.md.

const REST = "/service/rest/v1";
const BETA = "/service/rest/beta";

const listeners = { unauthorized: [], exampleData: [], writesRefused: [] };
let csrfToken = null;

export function on(event, handler) {
  listeners[event].push(handler);
}

function emit(event, detail) {
  for (const handler of listeners[event]) handler(detail);
}

export function setCsrfToken(token) {
  csrfToken = token || null;
}

// The requests the server refuses with 501, from GET /api/areas (docs/ui/API.md, section Areas).
// {name} in a path stands for one segment. The interface answers such a request itself, so the
// browser logs no failed request for it.
let refused = [];

export function setRefused(list) {
  refused = (Array.isArray(list) ? list : [])
    .filter((r) => r && typeof r.method === "string" && typeof r.path === "string")
    .map((r) => ({ method: r.method.toUpperCase(), parts: r.path.split("/"), area: typeof r.area === "string" ? r.area : null }));
}

function refusalOf(method, path) {
  const parts = path.split("/");
  return (
    refused.find((r) => r.method === method && r.parts.length === parts.length && r.parts.every((p, i) => (/^\{[^/{}]+\}$/.test(p) ? parts[i] !== "" : p === parts[i]))) || null
  );
}

function documentToken() {
  const meta = document.querySelector('meta[name="pipesmith-csrf"]');
  return meta ? meta.getAttribute("content") : null;
}

export class ApiError extends Error {
  constructor(status, message, body, path = null) {
    super(message || `The server answered ${status}.`);
    this.status = status;
    this.body = body;
    this.path = path;
    // Validation answers: [{"id": "<field>", "message": "<text>"}]
    this.fieldErrors = Array.isArray(body)
      ? body.filter((e) => e && typeof e.message === "string").map((e) => ({ id: String(e.id ?? "*"), message: e.message }))
      : [];
  }
}

const STATUS_TEXT = {
  400: "The server did not accept the request.",
  401: "Your session has ended. Sign in again.",
  403: "You do not have the permission for this.",
  404: "The server does not know this object.",
  409: "The request conflicts with the current state.",
  429: "Too many attempts. Wait a moment and try again.",
  501: "This function is not available in this server.",
  503: "The server is read only or not ready."
};

function query(params) {
  if (!params) return "";
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === "") continue;
    q.append(k, String(v));
  }
  const s = q.toString();
  return s ? `?${s}` : "";
}

function seg(value) {
  return encodeURIComponent(String(value));
}

// A path with "/" kept and each segment encoded, for log file names.
function segs(value) {
  return String(value).split("/").map(encodeURIComponent).join("/");
}

export async function request(method, path, options = {}) {
  const headers = { Accept: "application/json, text/plain;q=0.9" };
  let body;
  if (options.form) {
    body = options.form;
  } else if (options.text !== undefined) {
    headers["Content-Type"] = "text/plain; charset=utf-8";
    body = options.text;
  } else if (options.body !== undefined) {
    headers["Content-Type"] = "application/json";
    body = JSON.stringify(options.body);
  }
  if (method !== "GET" && method !== "HEAD") {
    const token = documentToken() || csrfToken;
    if (token) headers["X-Pipesmith-Csrf"] = token;
  }
  const known = refusalOf(method, path);
  if (known) {
    const error = new ApiError(501, STATUS_TEXT[501], { message: STATUS_TEXT[501], area: known.area }, path);
    if (method !== "GET" && options.effect !== false) emit("writesRefused", error);
    throw error;
  }
  let response;
  try {
    response = await fetch(path + query(options.query), { method, headers, body, credentials: "same-origin", cache: "no-store" });
  } catch (e) {
    throw new ApiError(0, "The server cannot be reached.", null);
  }
  if (response.headers.get("X-Pipesmith-Example-Data")) emit("exampleData", true);

  const type = response.headers.get("Content-Type") || "";
  let data = null;
  if (response.status !== 204) {
    const raw = await response.text();
    if (raw && type.includes("json")) {
      try {
        data = JSON.parse(raw);
      } catch (e) {
        data = raw;
      }
    } else {
      data = raw || null;
    }
  }
  if (!response.ok) {
    const message = data && typeof data === "object" && !Array.isArray(data) && data.message ? data.message : STATUS_TEXT[response.status];
    const error = new ApiError(response.status, message, data, path);
    if (response.status === 429) error.retryAfter = Number(response.headers.get("Retry-After")) || null;
    if (response.status === 401 && !options.quiet401) emit("unauthorized", error);
    // A change refused with 501: this server takes no changes yet. Checks without an effect
    // (previews, tests, verifications) say nothing about changes and carry effect: false.
    if (response.status === 501 && method !== "GET" && options.effect !== false) emit("writesRefused", error);
    throw error;
  }
  return data;
}

const get = (path, q, options = {}) => request("GET", path, { ...options, query: q });
const post = (path, body, options = {}) => request("POST", path, { ...options, body });
const put = (path, body, options = {}) => request("PUT", path, { ...options, body });
const del = (path, q) => request("DELETE", path, { query: q });

// The path segment of a format in the repository paths of the REST API.
export const FORMAT_SEGMENT = { maven2: "maven", npm: "npm", docker: "docker", raw: "raw" };

export const api = {
  session: {
    signIn: (username, password) => post("/api/session", { username, password }, { quiet401: true }),
    get: () => get("/api/session", null, { quiet401: true }),
    signOut: () => request("DELETE", "/api/session", { quiet401: true })
  },
  areas: () => get("/api/areas"),
  status: () => get("/api/status"),

  repositories: {
    list: () => get(`${REST}/repositories`),
    settings: () => get(`${REST}/repositorySettings`),
    counts: () => get("/api/repositories"),
    byName: (name) => get(`${REST}/repositories/${seg(name)}`),
    get: (format, type, name) => get(`${REST}/repositories/${FORMAT_SEGMENT[format]}/${type}/${seg(name)}`),
    create: (format, type, body) => post(`${REST}/repositories/${FORMAT_SEGMENT[format]}/${type}`, body),
    update: (format, type, name, body) => put(`${REST}/repositories/${FORMAT_SEGMENT[format]}/${type}/${seg(name)}`, body),
    remove: (name) => del(`${REST}/repositories/${seg(name)}`),
    rebuildIndex: (name) => post(`${REST}/repositories/${seg(name)}/rebuild-index`),
    invalidateCache: (name) => post(`${REST}/repositories/${seg(name)}/invalidate-cache`)
  },

  browse: (repository, path, limit, offset) => get(`/api/browse/${seg(repository)}`, { path, limit, offset }),
  asset: (repository, path) => get(`/api/asset/${seg(repository)}`, { path }),
  assets: {
    get: (id) => get(`${REST}/assets/${seg(id)}`),
    remove: (id) => del(`${REST}/assets/${seg(id)}`)
  },
  search: (params) => get(`${REST}/search`, params),
  upload: {
    specs: (format) => get(`${REST}/formats/${seg(format)}/upload-specs`),
    send: (repository, formData) => request("POST", `${REST}/components`, { query: { repository }, form: formData })
  },

  blobStores: {
    list: () => get(`${REST}/blobstores`),
    get: (name) => get(`${REST}/blobstores/file/${seg(name)}`),
    create: (body) => post(`${REST}/blobstores/file`, body),
    update: (name, body) => put(`${REST}/blobstores/file/${seg(name)}`, body),
    remove: (name) => del(`${REST}/blobstores/${seg(name)}`),
    quota: (name) => get(`${REST}/blobstores/${seg(name)}/quota-status`),
    deleted: (name) => get(`/api/blob-stores/${seg(name)}/deleted`)
  },

  cleanupPolicies: {
    list: () => get("/api/cleanup-policies"),
    get: (name) => get(`/api/cleanup-policies/${seg(name)}`),
    create: (body) => post("/api/cleanup-policies", body),
    update: (name, body) => put(`/api/cleanup-policies/${seg(name)}`, body),
    remove: (name) => del(`/api/cleanup-policies/${seg(name)}`),
    preview: (policy, repository) => post("/api/cleanup-policies/preview", { policy, repository }, { effect: false })
  },

  routingRules: {
    list: () => get(`${REST}/routing-rules`),
    get: (name) => get(`${REST}/routing-rules/${seg(name)}`),
    create: (body) => post(`${REST}/routing-rules`, body),
    update: (name, body) => put(`${REST}/routing-rules/${seg(name)}`, body),
    remove: (name) => del(`${REST}/routing-rules/${seg(name)}`),
    test: (mode, matchers, path) => post("/api/routing-rules/test", { mode, matchers, path }, { effect: false }),
    preview: (scope, path) => post("/api/routing-rules/preview", { scope, path }, { effect: false })
  },

  privileges: {
    list: () => get(`${REST}/security/privileges`),
    get: (name) => get(`${REST}/security/privileges/${seg(name)}`)
  },
  roles: {
    list: (source) => get(`${REST}/security/roles`, { source }),
    get: (id) => get(`${REST}/security/roles/${seg(id)}`),
    create: (body) => post(`${REST}/security/roles`, body),
    update: (id, body) => put(`${REST}/security/roles/${seg(id)}`, body),
    remove: (id) => del(`${REST}/security/roles/${seg(id)}`)
  },
  userSources: () => get(`${REST}/security/user-sources`),
  users: {
    list: (source, userId) => get(`${REST}/security/users`, { source, userId }),
    create: (body) => post(`${REST}/security/users`, body),
    update: (userId, body) => put(`${REST}/security/users/${seg(userId)}`, body),
    remove: (userId, realm) => del(`${REST}/security/users/${seg(userId)}`, { realm }),
    changePassword: (userId, password) => request("PUT", `${REST}/security/users/${seg(userId)}/change-password`, { text: password })
  },
  anonymous: {
    get: () => get(`${REST}/security/anonymous`),
    update: (body) => put(`${REST}/security/anonymous`, body)
  },
  realms: {
    available: () => get(`${REST}/security/realms/available`),
    active: () => get(`${REST}/security/realms/active`),
    setActive: (ids) => put(`${REST}/security/realms/active`, ids)
  },
  ldap: {
    list: () => get(`${REST}/security/ldap`),
    get: (name) => get(`${REST}/security/ldap/${seg(name)}`),
    create: (body) => post(`${REST}/security/ldap`, body),
    update: (name, body) => put(`${REST}/security/ldap/${seg(name)}`, body),
    remove: (name) => del(`${REST}/security/ldap/${seg(name)}`),
    changeOrder: (names) => post(`${REST}/security/ldap/change-order`, names),
    clearCache: () => post("/api/ldap/clear-cache"),
    verifyConnection: (connection) => post("/api/ldap/verify-connection", connection, { effect: false }),
    verifyUserMapping: (connection) => post("/api/ldap/verify-user-mapping", connection, { effect: false }),
    verifyLogin: (connection, username, password) => post("/api/ldap/verify-login", { connection, username, password }, { effect: false })
  },
  tokens: {
    list: (userId) => get("/api/tokens", { userId }),
    create: (kind, name) => post("/api/tokens", { kind, name }),
    remove: (id) => del(`/api/tokens/${seg(id)}`)
  },
  account: {
    update: (body) => put("/api/account", body),
    changePassword: (currentPassword, newPassword) => post("/api/account/password", { currentPassword, newPassword })
  },

  logging: {
    loggers: () => get("/api/logging/loggers"),
    set: (name, level) => put(`/api/logging/loggers/${seg(name)}`, { level }),
    remove: (name) => del(`/api/logging/loggers/${seg(name)}`),
    reset: () => post("/api/logging/reset")
  },
  logs: {
    list: () => get("/api/logs"),
    tail: (name, size) => get(`/api/logs/${segs(name)}`, { bytes: size }),
    downloadUrl: (name) => `/api/logs/${segs(name)}?download=1`
  },

  statusChecks: () => get(`${REST}/status/check`),
  writable: () => request("GET", `${REST}/status/writable`),
  systemInformation: () => get(`${BETA}/system/information`),

  capabilities: {
    list: () => get(`${REST}/capabilities`),
    update: (id, body) => put(`${REST}/capabilities/${seg(id)}`, body),
    remove: (id) => del(`${REST}/capabilities/${seg(id)}`),
    support: () => get("/api/capabilities/support")
  },

  node: () => get(`${REST}/system/node`),
  readOnly: {
    get: () => get(`${REST}/read-only`),
    freeze: () => post(`${REST}/read-only/freeze`),
    release: () => post(`${REST}/read-only/release`),
    forceRelease: () => post(`${REST}/read-only/force-release`)
  },

  tasks: {
    list: (continuationToken) => get(`${REST}/tasks`, { continuationToken }),
    get: (id) => get(`${REST}/tasks/${seg(id)}`),
    run: (id) => post(`${REST}/tasks/${seg(id)}/run`),
    stop: (id) => post(`${REST}/tasks/${seg(id)}/stop`),
    settings: (id) => get(`/api/tasks/${seg(id)}`),
    create: (body) => post("/api/tasks", body),
    update: (id, body) => put(`/api/tasks/${seg(id)}`, body),
    remove: (id) => del(`/api/tasks/${seg(id)}`),
    types: () => get("/api/task-types")
  },

  metrics: {
    requests: (from, to) => get("/api/metrics/requests", { from, to }),
    downloads: (from, to) => get("/api/metrics/downloads", { from, to })
  }
};
