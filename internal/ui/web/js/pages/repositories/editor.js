// S11 Repository editor: create and edit hosted, proxy and group repositories of maven2, npm,
// docker and raw. The request bodies follow docs/ui/API.md, section S11.

import { h, clone } from "../../dom.js";
import { api } from "../../api.js";
import { formatName, typeName } from "../../format.js";
import { pageHead, failure, button, busy, notFound } from "../../ui/page.js";
import { form } from "../../ui/form.js";
import { notice, toast, errorText, mark, problem, toastProblem } from "../../ui/notice.js";
import { confirm } from "../../ui/dialog.js";
import { urlCell } from "./list.js";

const FORMATS = ["maven2", "npm", "docker", "raw"];
const KINDS = ["hosted", "proxy", "group"];
const NAME = { regex: /^[A-Za-z0-9][A-Za-z0-9._-]*$/, message: "Use letters, digits, dots, hyphens and underscores, starting with a letter or a digit." };

function defaults(format, type) {
  const v = { name: "", online: true, storage: { blobStoreName: null, strictContentTypeValidation: true } };
  if (type === "hosted") {
    v.storage.writePolicy = format === "maven2" || format === "npm" ? "ALLOW_ONCE" : "ALLOW";
    v.cleanup = { policyNames: [] };
  }
  if (type === "proxy") {
    v.proxy = { remoteUrl: "", contentMaxAge: format === "maven2" ? -1 : 1440, metadataMaxAge: 1440 };
    v.negativeCache = { enabled: true, timeToLive: 1440 };
    v.httpClient = { blocked: false, autoBlock: true, connection: { retries: null, userAgentSuffix: "", timeout: null, enableCircularRedirects: false, enableCookies: false, useTrustStore: false }, authentication: null };
    v.routingRuleName = null;
    v.replication = { preemptivePullEnabled: false, assetPathRegex: "" };
    v.cleanup = { policyNames: [] };
  }
  if (type === "group") v.group = { memberNames: [] };
  if (format === "maven2") v.maven = { versionPolicy: type === "group" ? "MIXED" : "RELEASE", layoutPolicy: "STRICT", contentDisposition: "INLINE" };
  if (format === "raw") v.raw = { contentDisposition: "ATTACHMENT" };
  if (format === "docker") {
    v.docker = { v1Enabled: false, forceBasicAuth: true, httpPort: null, httpsPort: null, subdomain: null, pathEnabled: true };
    if (type === "hosted") v.storage.latestPolicy = false;
    if (type === "proxy") v.dockerProxy = { indexType: "REGISTRY", indexUrl: null, cacheForeignLayers: false, foreignLayerUrlWhitelist: [] };
  }
  return v;
}

// The form values as a request body of the contract.
function toBody(values, format, type) {
  const body = clone(values);
  delete body.url;
  delete body.format;
  delete body.type;
  if (type === "hosted") body.component = { proprietaryComponents: false };
  if (type === "proxy") {
    body.routingRule = body.routingRuleName || null;
    delete body.routingRuleName;
    const auth = body.httpClient && body.httpClient.authentication;
    if (!auth || !auth.type) {
      body.httpClient.authentication = null;
    } else {
      for (const key of ["password", "bearerToken"]) if (!auth[key]) delete auth[key];
    }
    if (body.replication && !body.replication.preemptivePullEnabled) body.replication.assetPathRegex = "";
    if (format === "npm") body.npm = { removeQuarantined: false };
  }
  if (type === "group" && body.group) delete body.group.writableMember;
  return body;
}

function sections({ format, type, creating, stores, policies, rules, members, name }) {
  const list = [];
  list.push({
    title: "Repository",
    grid: true,
    fields: [
      creating
        ? { name: "name", label: "Name", type: "text", required: true, pattern: NAME, help: "Part of the address clients use. It cannot change later.", mono: true, autocomplete: "off" }
        : { name: "name", label: "Name", type: "static", mono: true },
      { name: "kind", label: "Format and type", type: "static", render: () => `${formatName(format)} ${typeName(type).toLowerCase()}` },
      creating ? null : { name: "url", label: "Address", type: "static", wide: true, render: (url) => urlCell(url) },
      { name: "online", label: "Online: the repository answers requests", type: "checkbox", wide: true }
    ].filter(Boolean)
  });

  if (format === "maven2") {
    list.push({
      title: "Maven",
      grid: true,
      fields: [
        {
          name: "maven.versionPolicy",
          label: "Version policy",
          type: "select",
          required: true,
          noBlank: true,
          readOnly: !creating,
          options: [
            { value: "RELEASE", label: "Releases only" },
            { value: "SNAPSHOT", label: "Snapshots only" },
            { value: "MIXED", label: "Releases and snapshots" }
          ],
          help: creating ? "Which versions the repository takes. It cannot change later." : "Fixed when the repository was created."
        },
        {
          name: "maven.layoutPolicy",
          label: "Layout policy",
          type: "select",
          required: true,
          noBlank: true,
          options: [
            { value: "STRICT", label: "Strict: only paths of the Maven layout" },
            { value: "PERMISSIVE", label: "Permissive: any path" }
          ]
        },
        {
          name: "maven.contentDisposition",
          label: "Content disposition",
          type: "select",
          noBlank: true,
          options: [
            { value: "INLINE", label: "Inline: browsers may show files" },
            { value: "ATTACHMENT", label: "Attachment: browsers download files" }
          ]
        }
      ]
    });
  }
  if (format === "raw") {
    list.push({
      title: "Raw",
      fields: [
        {
          name: "raw.contentDisposition",
          label: "Content disposition",
          type: "select",
          noBlank: true,
          options: [
            { value: "INLINE", label: "Inline: browsers may show files" },
            { value: "ATTACHMENT", label: "Attachment: browsers download files" }
          ]
        }
      ]
    });
  }
  if (format === "docker") {
    const fields = [
      {
        name: "docker.httpPort",
        label: "Port of its own",
        type: "number",
        min: 1,
        max: 65535,
        help: "Docker clients reach the repository on this port. Leave it empty to reach it through the path on the main port. A change takes effect after a restart."
      },
      { name: "docker.forceBasicAuth", label: "Allow anonymous pull", type: "checkbox", invert: true, help: "Needs anonymous access and the Docker token realm to be active." }
    ];
    if (type === "hosted") {
      fields.push({
        name: "storage.latestPolicy",
        label: "Allow a new push of the tag latest",
        type: "checkbox",
        help: "Every other tag keeps the deployment policy below.",
        show: (v) => v.storage && v.storage.writePolicy === "ALLOW_ONCE"
      });
    }
    list.push({ title: "Docker", fields });
  }

  if (type === "proxy") {
    const proxyFields = [
      { name: "proxy.remoteUrl", label: "Remote address", type: "url", required: true, wide: true, mono: true, placeholder: "https://repo.example.org/", help: "The repository this proxy fetches from and caches." },
      { name: "proxy.contentMaxAge", label: "Keep content for (minutes)", type: "number", required: true, min: -1, help: "-1 keeps it until a cleanup policy removes it." },
      { name: "proxy.metadataMaxAge", label: "Keep metadata for (minutes)", type: "number", required: true, min: -1 },
      { name: "routingRuleName", label: "Routing rule", type: "select", blank: "No rule: every path may be fetched", options: rules.map((r) => ({ value: r.name, label: `${r.name} (${r.mode === "BLOCK" ? "block" : "allow"})` })) },
      { name: "httpClient.blocked", label: "Blocked: do not contact the remote", type: "checkbox", wide: true },
      { name: "httpClient.autoBlock", label: "Block automatically while the remote does not answer", type: "checkbox", wide: true },
      { name: "healthCheck", label: "Health check", type: "static", wide: true, render: () => h("span", {}, mark("off", "Not available"), " ", h("span", { class: "muted", text: "This server does not call outside services to rate a remote." })) }
    ];
    if (format === "docker") {
      proxyFields.push(
        {
          name: "dockerProxy.indexType",
          label: "Index",
          type: "radio",
          wide: true,
          options: [
            { value: "REGISTRY", label: "The remote address above" },
            { value: "HUB", label: "The public index of the Docker project" },
            { value: "CUSTOM", label: "Another index" }
          ]
        },
        { name: "dockerProxy.indexUrl", label: "Index address", type: "url", mono: true, wide: true, required: true, show: (v) => v.dockerProxy && v.dockerProxy.indexType === "CUSTOM" },
        { name: "dockerProxy.cacheForeignLayers", label: "Cache layers that images keep on other hosts", type: "checkbox", wide: true },
        {
          name: "dockerProxy.foreignLayerUrlWhitelist",
          label: "Hosts of foreign layers",
          type: "list",
          mono: true,
          wide: true,
          help: "One regular expression per row.",
          show: (v) => v.dockerProxy && v.dockerProxy.cacheForeignLayers
        }
      );
    }
    list.push({ title: "Proxy", grid: true, fields: proxyFields });
    list.push({
      title: "Negative cache",
      help: "Remembers paths the remote does not have, so the server does not ask again for each request.",
      grid: true,
      fields: [
        { name: "negativeCache.enabled", label: "Remember missing paths", type: "checkbox", wide: true },
        { name: "negativeCache.timeToLive", label: "Remember for (minutes)", type: "number", required: true, min: 0, show: (v) => v.negativeCache && v.negativeCache.enabled }
      ]
    });
    const auth = (v) => v.httpClient && v.httpClient.authentication && v.httpClient.authentication.type;
    list.push({
      title: "HTTP",
      help: "How the server talks to the remote.",
      grid: true,
      fields: [
        { name: "httpClient.connection.timeout", label: "Timeout (seconds)", type: "number", min: 1, max: 3600, placeholder: "60" },
        { name: "httpClient.connection.retries", label: "Retries", type: "number", min: 0, max: 10, placeholder: "2" },
        { name: "httpClient.connection.userAgentSuffix", label: "Added to the user agent", type: "text", wide: true },
        { name: "httpClient.connection.enableCircularRedirects", label: "Follow redirects that come back to the same address", type: "checkbox", wide: true },
        { name: "httpClient.connection.enableCookies", label: "Keep cookies of the remote", type: "checkbox", wide: true },
        {
          name: "httpClient.authentication.type",
          label: "Authentication",
          type: "select",
          blank: "None",
          options: [
            { value: "username", label: "User name and password" },
            { value: "bearerToken", label: "Bearer token" }
          ]
        },
        { name: "httpClient.authentication.username", label: "User name", type: "text", required: true, autocomplete: "off", show: (v) => auth(v) === "username" },
        {
          name: "httpClient.authentication.password",
          label: "Password",
          type: "password",
          required: creating,
          help: creating ? null : "Leave it empty to keep the stored password.",
          show: (v) => auth(v) === "username"
        },
        { name: "httpClient.authentication.bearerToken", label: "Token", type: "password", required: creating, help: creating ? null : "Leave it empty to keep the stored token.", show: (v) => auth(v) === "bearerToken" },
        { name: "httpClient.authentication.preemptive", label: "Send the credentials with the first request", type: "checkbox", wide: true, show: (v) => Boolean(auth(v)) }
      ]
    });
  }

  if (type === "group") {
    list.push({
      title: "Members",
      help: "The group answers from the first member that has the path. Order matters.",
      fields: [
        {
          name: "group.memberNames",
          label: "Members",
          type: "transfer",
          ordered: true,
          availableLabel: `${formatName(format)} repositories`,
          selectedLabel: "Members in order",
          options: members.filter((r) => r.format === format && r.name !== name).map((r) => ({ value: r.name, label: `${r.name} (${r.type})` }))
        }
      ]
    });
  }

  const storageFields = [
    {
      name: "storage.blobStoreName",
      label: "Blob store",
      type: "select",
      required: true,
      readOnly: !creating,
      options: stores.map((s) => ({ value: s.name, label: s.unavailable ? `${s.name} (unavailable)` : s.name })),
      help: creating ? "Where the files are kept. It cannot change later." : "Fixed when the repository was created."
    },
    { name: "storage.strictContentTypeValidation", label: "Check that the content of each upload matches its type", type: "checkbox", wide: true }
  ];
  if (type === "hosted") {
    storageFields.splice(1, 0, {
      name: "storage.writePolicy",
      label: "Deployment policy",
      type: "select",
      required: true,
      noBlank: true,
      options: [
        { value: "ALLOW", label: "Allow redeploy" },
        { value: "ALLOW_ONCE", label: "Deploy each path once" },
        { value: "DENY", label: "Read only" }
      ]
    });
  }
  list.push({ title: "Storage", grid: true, fields: storageFields });

  if (type !== "group") {
    list.push({
      title: "Cleanup",
      help: "Policies run in the task that applies cleanup policies.",
      fields: [
        {
          name: "cleanup.policyNames",
          label: "Cleanup policies",
          type: "transfer",
          availableLabel: "Policies",
          selectedLabel: "Applied",
          options: policies.filter((p) => p.format === "*" || p.format === format).map((p) => ({ value: p.name, label: p.format === "*" ? `${p.name} (all formats)` : p.name }))
        }
      ]
    });
  }
  return list;
}

export async function render(ctx) {
  const creating = Boolean(ctx.params.format);
  let format = ctx.params.format;
  let type = ctx.params.type;
  const name = ctx.params.name;
  if (creating && (!FORMATS.includes(format) || !KINDS.includes(type))) {
    ctx.main.append(notFound("kind of repository"));
    return;
  }

  let values;
  let stores = [];
  let policies = [];
  let rules = [];
  let members = [];
  try {
    if (!creating) {
      const simple = await api.repositories.get(...(await kindOf(name)), name);
      values = simple;
      format = simple.format;
      type = simple.type;
    } else {
      values = defaults(format, type);
    }
    const loads = await Promise.allSettled([
      api.blobStores.list(),
      type !== "group" ? api.cleanupPolicies.list() : Promise.resolve([]),
      type === "proxy" ? api.routingRules.list() : Promise.resolve([]),
      type === "group" ? api.repositories.list() : Promise.resolve([])
    ]);
    [stores, policies, rules, members] = loads.map((r) => (r.status === "fulfilled" ? r.value || [] : []));
    if (loads[0].status === "rejected" && creating) throw loads[0].reason;
  } catch (error) {
    failure(ctx.main, error, { what: "repository", retry: () => location.reload() });
    return;
  }

  const canEdit = creating || ctx.can(`nexus:repository-admin:${format}:${name}:edit`);
  const canDelete = !creating && ctx.can(`nexus:repository-admin:${format}:${name}:delete`);
  const title = creating ? `New ${formatName(format)} ${typeName(type).toLowerCase()} repository` : name;
  ctx.setTitle(title);

  const actions = [];
  if (!creating && type !== "group" && canEdit) {
    actions.push(
      button("Rebuild index", {
        onClick: async (event) => {
          try {
            await busy(event.currentTarget, () => api.repositories.rebuildIndex(name));
            toast(`The index of ${name} is being rebuilt.`);
          } catch (error) {
            toastProblem(error);
          }
        }
      })
    );
  }
  if (!creating && type !== "hosted" && canEdit) {
    actions.push(
      button("Invalidate cache", {
        onClick: async (event) => {
          try {
            await busy(event.currentTarget, () => api.repositories.invalidateCache(name));
            toast(`The cache of ${name} is invalidated. The next request asks the remote again.`);
          } catch (error) {
            toastProblem(error);
          }
        }
      })
    );
  }
  if (canDelete) {
    actions.push(
      button("Delete", {
        kind: "danger",
        onClick: async () => {
          const ok = await confirm({
            title: `Delete ${name}?`,
            text: "The repository and its settings are removed. Clients that use its address get errors. The files stay in the blob store until a compact task removes them.",
            confirmLabel: "Delete repository",
            danger: true,
            typeToConfirm: name
          });
          if (!ok) return;
          try {
            await api.repositories.remove(name);
            ctx.setDirty(null);
            toast(`The repository ${name} is deleted.`);
            ctx.navigate("/admin/repositories");
          } catch (error) {
            toastProblem(error);
          }
        }
      })
    );
  }

  ctx.main.append(
    pageHead({
      title,
      sub: creating ? null : `${formatName(format)} ${typeName(type).toLowerCase()} repository`,
      crumbs: [{ label: "Repositories", href: "#/admin/repositories" }, ...(creating ? [{ label: "New repository", href: "#/admin/repositories/new" }] : [])],
      actions
    })
  );

  const messages = h("div", { class: "stack page-messages" });
  if (!canEdit) messages.append(notice("info", "Read only", ctx.readOnlyText("Your roles let you read this repository's settings, not change them.")));
  if (!stores.length) messages.append(notice("warning", "No blob store", "Create a blob store first; every repository needs one."));

  const f = form({ sections: sections({ format, type, creating, stores, policies, rules, members, name }), values, readOnly: !canEdit });
  ctx.setDirty(() => f.isDirty());

  const save = h("button", { type: "submit", class: "btn primary", text: creating ? "Create repository" : "Save" });
  const discard = h("button", { type: "button", class: "btn", text: creating ? "Cancel" : "Discard changes" });
  const formEl = h("form", { novalidate: true, class: "panel" }, f.el, canEdit ? h("div", { class: "form-actions" }, save, discard) : null);
  ctx.main.append(messages, formEl);

  discard.addEventListener("click", () => {
    if (creating) {
      ctx.setDirty(null);
      ctx.navigate("/admin/repositories/new");
    } else {
      f.set(values);
      f.clearErrors();
    }
  });

  formEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    messages.replaceChildren();
    if (!f.validate()) return;
    const body = toBody(f.values(), format, type);
    try {
      await busy(save, async () => {
        if (creating) {
          await api.repositories.create(format, type, body);
        } else {
          await api.repositories.update(format, type, name, body);
        }
      });
    } catch (error) {
      if (!f.showServerErrors(error)) messages.append(problem(error, creating ? "Not created" : "Not saved"));
      return;
    }
    f.markClean();
    if (creating) {
      ctx.setDirty(null);
      toast(`The repository ${body.name} is created.`);
      ctx.navigate(`/admin/repositories/${encodeURIComponent(body.name)}`);
    } else {
      try {
        values = await api.repositories.get(format, type, name);
        f.set(values);
      } catch (error) {
        /* the save went through; the form keeps what was sent */
      }
      toast(`The repository ${name} is saved.`);
    }
  });
}

// The format and type of a repository opened by name.
async function kindOf(name) {
  const repo = await api.repositories.byName(name);
  return [repo.format, repo.type];
}
