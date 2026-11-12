// Every screen of docs/ui/INVENTORY.md with its route, its permission and its module. The permission
// strings are the interface's one copy of the table in docs/ui/API.md, section Screens; domains that
// API.md marks unverified are corrected here and nowhere else.

export const SECTIONS = [
  { id: "browse", title: "Browse" },
  { id: "repository", title: "Repository", landing: "/admin/repository", text: "Repositories, blob stores and the rules that apply to them." },
  { id: "security", title: "Security", landing: "/admin/security", text: "Users, roles, directories and how people sign in." },
  { id: "system", title: "System", landing: "/admin/system", text: "Tasks, capabilities, the node and its counters." },
  { id: "support", title: "Support", landing: "/admin/support", text: "Health checks, system information and logs." }
];

// path: route after "#", with ":name" for one segment and "*name" for the rest of the path.
// nav: label in the side navigation. text: line on the section landing.
export const ROUTES = [
  { id: "S01", path: "/sign-in", title: "Sign in", module: "./pages/signin.js", public: true },
  { id: "S02", path: "/overview", title: "Overview", module: "./pages/overview.js", section: "browse", nav: "Overview" },
  { id: "S03", path: "/search", title: "Search", module: "./pages/browse/search.js", section: "browse", nav: "Search", perm: "nexus:search:read" },
  { id: "S04", path: "/browse", title: "Browse", module: "./pages/browse/repositories.js", section: "browse", nav: "Browse" },
  { id: "S05", path: "/browse/:repository", title: "Repository", module: "./pages/browse/tree.js", section: "browse", parent: "S04" },
  { id: "S06", path: "/upload", title: "Upload", module: "./pages/browse/upload.js", section: "browse", nav: "Upload", perm: "nexus:component:add" },
  { id: "S07", path: "/upload/:repository", title: "Upload", module: "./pages/browse/upload-form.js", section: "browse", parent: "S06", perm: "nexus:component:add" },

  { id: "S08", path: "/admin/:section", title: "Section", module: "./pages/landing.js" },

  { id: "S09", path: "/admin/repositories", title: "Repositories", module: "./pages/repositories/list.js", section: "repository", nav: "Repositories", text: "Create and change hosted, proxy and group repositories.", perm: "nexus:repository-admin:*:*:read" },
  { id: "S10", path: "/admin/repositories/new", title: "New repository", module: "./pages/repositories/new.js", section: "repository", parent: "S09", perm: "nexus:repository-admin:*:*:add" },
  { id: "S11", path: "/admin/repositories/new/:format/:type", title: "New repository", module: "./pages/repositories/editor.js", section: "repository", parent: "S09", perm: "nexus:repository-admin:*:*:add" },
  { id: "S11", path: "/admin/repositories/:name", title: "Repository", module: "./pages/repositories/editor.js", section: "repository", parent: "S09", perm: "nexus:repository-admin:*:*:read" },
  { id: "S12", path: "/admin/blob-stores", title: "Blob stores", module: "./pages/storage/blob-stores.js", section: "repository", nav: "Blob stores", text: "Where the files of the repositories are kept.", perm: "nexus:blobstores:read" },
  { id: "S13", path: "/admin/blob-stores/new", title: "New blob store", module: "./pages/storage/blob-store-editor.js", section: "repository", parent: "S12", perm: "nexus:blobstores:create" },
  { id: "S13", path: "/admin/blob-stores/:name", title: "Blob store", module: "./pages/storage/blob-store-editor.js", section: "repository", parent: "S12", perm: "nexus:blobstores:read" },
  { id: "S14", path: "/admin/cleanup-policies", title: "Cleanup policies", module: "./pages/storage/cleanup-policies.js", section: "repository", nav: "Cleanup policies", text: "Rules that remove old or unused components.", perm: "nexus:settings:read" },
  { id: "S15", path: "/admin/cleanup-policies/new", title: "New cleanup policy", module: "./pages/storage/cleanup-policy-editor.js", section: "repository", parent: "S14", perm: "nexus:settings:update" },
  { id: "S15", path: "/admin/cleanup-policies/:name", title: "Cleanup policy", module: "./pages/storage/cleanup-policy-editor.js", section: "repository", parent: "S14", perm: "nexus:settings:read" },
  { id: "S16", path: "/admin/routing-rules", title: "Routing rules", module: "./pages/storage/routing-rules.js", section: "repository", nav: "Routing rules", text: "Which paths a proxy may fetch from its remote.", perm: "nexus:settings:read" },
  { id: "S18", path: "/admin/routing-preview", title: "Routing preview", module: "./pages/storage/routing-preview.js", section: "repository", parent: "S16", perm: "nexus:settings:read" },
  { id: "S17", path: "/admin/routing-rules/new", title: "New routing rule", module: "./pages/storage/routing-rule-editor.js", section: "repository", parent: "S16", perm: "nexus:settings:update" },
  { id: "S17", path: "/admin/routing-rules/:name", title: "Routing rule", module: "./pages/storage/routing-rule-editor.js", section: "repository", parent: "S16", perm: "nexus:settings:read" },

  { id: "S23", path: "/admin/users", title: "Users", module: "./pages/security/users.js", section: "security", nav: "Users", text: "Local and directory users and their roles.", perm: "nexus:users:read" },
  { id: "S24", path: "/admin/users/new", title: "New user", module: "./pages/security/user-editor.js", section: "security", parent: "S23", perm: "nexus:users:create" },
  { id: "S24", path: "/admin/users/:source/:id", title: "User", module: "./pages/security/user-editor.js", section: "security", parent: "S23", perm: "nexus:users:read" },
  { id: "S21", path: "/admin/roles", title: "Roles", module: "./pages/security/roles.js", section: "security", nav: "Roles", text: "Sets of privileges and other roles.", perm: "nexus:roles:read" },
  { id: "S22", path: "/admin/roles/new", title: "New role", module: "./pages/security/role-editor.js", section: "security", parent: "S21", perm: "nexus:roles:create" },
  { id: "S22", path: "/admin/roles/:id", title: "Role", module: "./pages/security/role-editor.js", section: "security", parent: "S21", perm: "nexus:roles:read" },
  { id: "S19", path: "/admin/privileges", title: "Privileges", module: "./pages/security/privileges.js", section: "security", nav: "Privileges", text: "The single rights that roles are made of.", perm: "nexus:privileges:read" },
  { id: "S20", path: "/admin/privileges/:name", title: "Privilege", module: "./pages/security/privilege.js", section: "security", parent: "S19", perm: "nexus:privileges:read" },
  { id: "S26", path: "/admin/ldap", title: "LDAP", module: "./pages/security/ldap.js", section: "security", nav: "LDAP", text: "Directory servers users sign in with.", perm: "nexus:ldap:read" },
  { id: "S27", path: "/admin/ldap/new", title: "New LDAP connection", module: "./pages/security/ldap-editor.js", section: "security", parent: "S26", perm: "nexus:ldap:create" },
  { id: "S27", path: "/admin/ldap/:name", title: "LDAP connection", module: "./pages/security/ldap-editor.js", section: "security", parent: "S26", perm: "nexus:ldap:read" },
  { id: "S28", path: "/admin/realms", title: "Realms", module: "./pages/security/realms.js", section: "security", nav: "Realms", text: "Which ways of signing in are active, in which order.", perm: "nexus:settings:read" },
  { id: "S25", path: "/admin/anonymous", title: "Anonymous access", module: "./pages/security/anonymous.js", section: "security", nav: "Anonymous access", text: "What a request without a user may do.", perm: "nexus:settings:read" },
  { id: "S29", path: "/tokens", title: "Tokens", module: "./pages/security/tokens.js", perm: "nexus:apikey:*" },

  { id: "S38", path: "/admin/tasks", title: "Tasks", module: "./pages/system/tasks.js", section: "system", nav: "Tasks", text: "Scheduled and manual jobs of the server.", perm: "nexus:tasks:read" },
  { id: "S39", path: "/admin/tasks/new", title: "New task", module: "./pages/system/task-new.js", section: "system", parent: "S38", perm: "nexus:tasks:create" },
  { id: "S40", path: "/admin/tasks/new/:type", title: "New task", module: "./pages/system/task-editor.js", section: "system", parent: "S38", perm: "nexus:tasks:create" },
  { id: "S40", path: "/admin/tasks/:id", title: "Task", module: "./pages/system/task-editor.js", section: "system", parent: "S38", perm: "nexus:tasks:read" },
  { id: "S35", path: "/admin/capabilities", title: "Capabilities", module: "./pages/system/capabilities.js", section: "system", nav: "Capabilities", text: "Imported features and what this server does with them.", perm: "nexus:capabilities:read" },
  { id: "S36", path: "/admin/capabilities/:id", title: "Capability", module: "./pages/system/capability.js", section: "system", parent: "S35", perm: "nexus:capabilities:read" },
  { id: "S37", path: "/admin/node", title: "Node", module: "./pages/system/node.js", section: "system", nav: "Node", text: "Identity of the node and read only mode.", perm: "nexus:settings:read" },
  { id: "S43", path: "/admin/metrics", title: "Metrics", module: "./pages/system/metrics.js", section: "system", nav: "Metrics", text: "Requests and downloads per day.", perm: "nexus:metrics:read" },

  { id: "S33", path: "/admin/status", title: "Status", module: "./pages/support/status.js", section: "support", nav: "Status", text: "Health checks of the server.", perm: "nexus:atlas:read" },
  { id: "S34", path: "/admin/system-information", title: "System information", module: "./pages/support/system-information.js", section: "support", nav: "System information", text: "Version, schema and settings of the running server.", perm: "nexus:atlas:read" },
  { id: "S30", path: "/admin/logging", title: "Logging", module: "./pages/support/logging.js", section: "support", nav: "Logging", text: "The level of each logger.", perm: "nexus:logging:read" },
  { id: "S31", path: "/admin/logs", title: "Log files", module: "./pages/support/logs.js", section: "support", nav: "Log files", text: "Read and download the log files.", perm: "nexus:logging:read" },
  { id: "S32", path: "/admin/logs/*file", title: "Log file", module: "./pages/support/log-viewer.js", section: "support", parent: "S31", perm: "nexus:logging:read" },

  { id: "S41", path: "/account", title: "Account", module: "./pages/account.js" }
];

// S42 About is a dialog of the frame, S44 No access and S45 Not found are states of the frame.
export const FRAME_SCREENS = ["S42", "S44", "S45"];

function compile(path) {
  const names = [];
  const pattern = path
    .split("/")
    .map((part) => {
      if (part.startsWith(":")) {
        names.push(part.slice(1));
        return "([^/]+)";
      }
      if (part.startsWith("*")) {
        names.push(part.slice(1));
        return "(.+)";
      }
      return part.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    })
    .join("/");
  return { regex: new RegExp(`^${pattern}$`), names };
}

const COMPILED = ROUTES.map((route) => ({ route, ...compile(route.path) }));

export function match(path) {
  for (const { route, regex, names } of COMPILED) {
    const m = regex.exec(path);
    if (!m) continue;
    const params = {};
    names.forEach((name, i) => {
      params[name] = decodeURIComponent(m[i + 1]);
    });
    if (route.id === "S08" && !SECTIONS.some((s) => s.landing === `/admin/${params.section}`)) continue;
    return { route, params };
  }
  return null;
}

export function byId(id) {
  return ROUTES.find((r) => r.id === id);
}
