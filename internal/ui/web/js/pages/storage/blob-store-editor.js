// S13 Blob store editor: create and edit file blob stores, with the counts of the store, its quota
// state, the blobs marked for deletion and the repositories that use it. docs/ui/API.md, S13.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { bytes, number, plural } from "../../format.js";
import { pageHead, failure, button, busy, facts } from "../../ui/page.js";
import { form } from "../../ui/form.js";
import { notice, toast, errorText, mark, problem } from "../../ui/notice.js";
import { confirm } from "../../ui/dialog.js";
import { MB, NAME, QUOTA_TYPES, repositoryList, sentence } from "./common.js";

// The form keeps the limit in MB; the server takes and gives bytes.
function toValues(store) {
  const quota = store.softQuota;
  return {
    name: store.name,
    path: store.path || "",
    quotaEnabled: Boolean(quota),
    softQuota: { type: quota ? quota.type : "spaceUsedQuota", limit: quota ? Number((quota.limit / MB).toFixed(2)) : null }
  };
}

function toBody(values) {
  const softQuota = values.quotaEnabled ? { type: values.softQuota.type, limit: Math.round(values.softQuota.limit * MB) } : null;
  return { name: values.name, path: String(values.path || "").trim(), softQuota };
}

function sections(creating, canSave) {
  return [
    {
      title: "Store",
      grid: true,
      fields: [
        creating
          ? { name: "name", label: "Name", type: "text", required: true, pattern: NAME, mono: true, help: "Repositories name the store by it. It cannot change later." }
          : { name: "name", label: "Name", type: "static", mono: true },
        { name: "type", label: "Type", type: "static", render: () => "File" },
        {
          name: "path",
          label: "Path",
          type: "text",
          required: true,
          mono: true,
          wide: true,
          placeholder: "/srv/pipesmith/blobs/name",
          help: !canSave
            ? null
            : creating
              ? "An absolute path, or a name the server places in the blobs folder of its data folder."
              : "A new path does not move the files that are already stored. Move them first."
        }
      ]
    },
    {
      title: "Soft quota",
      help: "A soft quota only warns. When the limit is reached, the store shows as over the limit and the server goes on writing.",
      grid: true,
      fields: [
        { name: "quotaEnabled", label: "Warn when a limit is reached", type: "checkbox", wide: true },
        { name: "softQuota.type", label: "Warn when", type: "select", required: true, noBlank: true, options: QUOTA_TYPES, show: (v) => v.quotaEnabled },
        {
          name: "softQuota.limit",
          label: "Limit in MB",
          type: "number",
          required: true,
          min: 1,
          integer: false,
          step: "any",
          inputmode: "decimal",
          help: "1 GB is 1,024 MB, 1 TB is 1,048,576 MB.",
          show: (v) => v.quotaEnabled
        }
      ]
    }
  ];
}

export async function render(ctx) {
  ctx.main.classList.add("area-storage");
  const name = ctx.params.name;
  const creating = !name;
  const canSave = creating ? ctx.can("nexus:blobstores:create") : ctx.can("nexus:blobstores:update");
  const canDelete = !creating && ctx.can("nexus:blobstores:delete");
  const canSeeRepos = ctx.can("nexus:repository-admin:*:*:read");

  let stored = { name: "", path: "", softQuota: null };
  const known = { info: null, quota: null, marked: null, repos: null, reposFailed: false };

  async function load() {
    const [one, list, quota, marked, settings] = await Promise.allSettled([
      api.blobStores.get(name),
      api.blobStores.list(),
      api.blobStores.quota(name),
      api.blobStores.deleted(name),
      canSeeRepos ? api.repositories.settings() : Promise.resolve(null)
    ]);
    if (one.status === "rejected") throw one.reason;
    stored = { name, path: one.value.path, softQuota: one.value.softQuota || null };
    known.info = list.status === "fulfilled" ? (list.value || []).find((s) => s.name === name) || null : null;
    known.quota = quota.status === "fulfilled" ? quota.value : null;
    known.marked = marked.status === "fulfilled" ? marked.value : null;
    known.reposFailed = settings.status === "rejected";
    known.repos = settings.status === "fulfilled" && settings.value ? settings.value.filter((r) => (r.storage || {}).blobStoreName === name) : null;
  }

  if (!creating) {
    try {
      await load();
    } catch (error) {
      failure(ctx.main, error, { what: "blob store", retry: () => ctx.navigate(`/admin/blob-stores/${encodeURIComponent(name)}`, { replace: true }) });
      return;
    }
  }

  const title = creating ? "New blob store" : name;
  ctx.setTitle(title);
  const messages = h("div", { class: "stack page-messages" });

  const actions = [];
  if (canDelete) {
    actions.push(
      button("Delete", {
        kind: "danger",
        onClick: async () => {
          const inUse = known.repos ? known.repos.length : 0;
          const ok = await confirm({
            title: `Delete ${name}?`,
            text: inUse
              ? `${plural(inUse, "repository", "repositories")} ${inUse === 1 ? "uses" : "use"} this store. The server refuses the delete while any repository uses it.`
              : "The store is removed from this server. Repositories cannot use it afterwards.",
            confirmLabel: "Delete blob store",
            danger: true,
            typeToConfirm: name
          });
          if (!ok) return;
          messages.replaceChildren();
          try {
            await api.blobStores.remove(name);
          } catch (error) {
            messages.append(problem(error, "Not deleted"));
            messages.scrollIntoView({ block: "nearest" });
            return;
          }
          ctx.setDirty(null);
          toast(`The blob store ${name} is deleted.`);
          ctx.navigate("/admin/blob-stores");
        }
      })
    );
  }

  ctx.main.append(
    pageHead({
      title,
      sub: creating ? "A store of the type file keeps blobs in a folder on a disk of the server." : "File blob store",
      crumbs: [{ label: "Blob stores", href: "#/admin/blob-stores" }],
      actions
    }),
    messages
  );
  if (!creating && known.info && known.info.unavailable) {
    messages.append(notice("warning", "Unavailable", "The server cannot read the folder of this store. Repositories that use it answer with errors until the folder is back. Check the path and the disk."));
  }
  if (!creating && !canSave) messages.append(notice("info", "Read only", ctx.readOnlyText("Your roles let you read this blob store, not change it.")));

  const usage = h("section", { class: "panel", id: "store-usage", "aria-labelledby": "store-usage-title" });
  const users = h("section", { class: "panel", id: "store-repositories", "aria-labelledby": "store-repositories-title" });

  function drawUsage() {
    const { info, quota, marked } = known;
    const unknown = h("span", { class: "muted", text: "Unknown" });
    const down = info && info.unavailable;
    let quotaValue;
    if (!stored.softQuota) quotaValue = h("span", { class: "muted", text: "No soft quota" });
    else if (!quota) quotaValue = h("span", { class: "muted", text: "Not known" });
    else
      quotaValue = h(
        "div",
        { class: "stack-tight" },
        quota.isViolation ? mark("warn", "Over the limit") : mark("ok", "Within the limit"),
        quota.message ? h("div", { class: "muted small", text: sentence(quota.message) }) : null
      );
    usage.replaceChildren(
      h("h2", { id: "store-usage-title", text: "Usage" }),
      facts([
        ["State", info ? (info.unavailable ? mark("error", "Unavailable") : mark("ok", "Available")) : h("span", { class: "muted", text: "Not known" })],
        ["Blobs", info ? (down ? unknown.cloneNode(true) : number(info.blobCount)) : null],
        ["Used space", info ? (down ? unknown.cloneNode(true) : bytes(info.totalSizeInBytes)) : null],
        ["Free on its disk", info ? (down ? unknown.cloneNode(true) : bytes(info.availableSpaceInBytes)) : null],
        ["Soft quota", quotaValue],
        [
          "Marked for deletion",
          marked
            ? h(
                "div",
                { class: "stack-tight" },
                h("span", { text: marked.blobCount ? `${plural(marked.blobCount, "blob")}, ${bytes(marked.bytes)}` : "None" }),
                marked.blobCount ? h("div", { class: "muted small", text: "A compact task removes them from the disk." }) : null
              )
            : h("span", { class: "muted", text: "Not known" })
        ]
      ])
    );
    let body;
    if (known.repos) {
      body = known.repos.length ? repositoryList(ctx, known.repos) : h("p", { class: "muted", text: "No repository uses this store." });
    } else if (known.reposFailed || !canSeeRepos) {
      body = h("p", { class: "muted", text: "Your roles do not show which repositories use this store." });
    }
    users.replaceChildren(h("h2", { id: "store-repositories-title", text: "Repositories" }), body);
  }

  if (!creating) {
    drawUsage();
    ctx.main.append(h("div", { class: "split store-facts" }, usage, users));
  }

  const f = form({ sections: sections(creating, canSave), values: toValues(stored), readOnly: !canSave });
  ctx.setDirty(() => f.isDirty());
  const save = h("button", { type: "submit", class: "btn primary", text: creating ? "Create blob store" : "Save" });
  const discard = h("button", { type: "button", class: "btn", text: creating ? "Cancel" : "Discard changes" });
  const formEl = h("form", { novalidate: true, class: "panel storage-form" }, f.el, canSave ? h("div", { class: "form-actions" }, save, discard) : null);
  ctx.main.append(formEl);

  discard.addEventListener("click", () => {
    if (creating) {
      ctx.setDirty(null);
      ctx.navigate("/admin/blob-stores");
    } else {
      f.set(toValues(stored));
      f.clearErrors();
    }
  });

  formEl.addEventListener("submit", async (event) => {
    event.preventDefault();
    messages.replaceChildren();
    if (!canSave || !f.validate()) return;
    const body = toBody(f.values());
    try {
      await busy(save, () => (creating ? api.blobStores.create(body) : api.blobStores.update(name, { path: body.path, softQuota: body.softQuota })));
    } catch (error) {
      if (!f.showServerErrors(error)) messages.append(problem(error, creating ? "Not created" : "Not saved"));
      return;
    }
    f.markClean();
    if (creating) {
      ctx.setDirty(null);
      toast(`The blob store ${body.name} is created.`);
      ctx.navigate(`/admin/blob-stores/${encodeURIComponent(body.name)}`);
      return;
    }
    try {
      await load();
      f.set(toValues(stored));
      drawUsage();
    } catch (error) {
      /* the save went through; the form keeps what was sent */
    }
    toast(`The blob store ${name} is saved.`);
  });
}
