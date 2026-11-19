// The export of a folder or of one image as an archive (docs/plans/browse-tools.md, section Export
// Contract). The dialog asks the server for the count and the size first (dry=1), shows them with the
// form and the command for the local use, and starts the download after one more dry call shows a
// free place. The commands follow what the clients did with an export in task V2.1 of that plan:
// Maven 3.9 resolves a release copied into its local repository, but a SNAPSHOT with time stamped
// files only from the folder as a file:// repository.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { bytes, number } from "../../format.js";
import { facts } from "../../ui/page.js";
import { notice, toast, problem } from "../../ui/notice.js";
import { openDialog } from "../../ui/dialog.js";
import { copyButton } from "../repositories/list.js";

const FORMS = {
  maven: "Maven repository layout, zip",
  zip: "Zip of the folder",
  npm: "Zip of the package tarballs",
  oci: "OCI image layout, tar"
};

// A folder of a repository of this format can be exported; a Docker repository exports one image
// from the detail of its manifest.
export function exportsFolders(repo) {
  return Boolean(repo) && repo.format !== "docker";
}

// A dry answer says busy while two exports stream, or while an export of the same account streams
// (own_export): the download would meet 429, which the browser shows as a failed download only, so
// the dialog shows the wait instead.
function busyError(info) {
  return info && info.busy ? { status: 429, retryAfter: info.retry_after || 30, own: Boolean(info.own_export), body: {} } : null;
}

// The read permission of a download decides, as on the server (call B17).
export function mayExport(ctx, repo) {
  return Boolean(repo) && ctx.can(`nexus:repository-view:${repo.format}:${repo.name}:read`);
}

function stem(name) {
  return String(name).replace(/\.(zip|tar)$/, "");
}

// The commands for the local use of an archive, and a note on them.
export function usage(form, name) {
  const folder = stem(name);
  switch (form) {
    case "maven":
      return {
        commands: [`unzip ${name} -d ${folder}`],
        note:
          `Add the folder to the repositories of the project with the address file:///<path>/${folder}. Maven then resolves the releases and the SNAPSHOT versions of the export. ` +
          "Unpacked into ~/.m2/repository the files serve releases only: Maven 3.9 does not find a SNAPSHOT with time stamped files there."
      };
    case "npm":
      return {
        commands: [`unzip ${name} -d ${folder}`, `npm install ./${folder}/<package>/<package>-<version>.tgz`],
        note: "Each package tarball installs on its own, also without a registry."
      };
    case "oci":
      return { commands: [`docker load -i ${name}`], note: "Docker loads the image under the name and tag of the export." };
    default:
      return { commands: [`unzip ${name} -d ${folder}`], note: "The archive holds the files below the folder, with their paths." };
  }
}

function commandRow(command) {
  return h("div", { class: "export-command" }, h("code", { class: "log", text: command }), copyButton(command, "Copy"));
}

function refusal(error) {
  const body = (error && error.body) || {};
  if (error && error.status === 413) {
    const more = body.complete === false ? " or more" : "";
    return notice(
      "warning",
      "Too large to export",
      `The export would hold ${number(body.files)}${more} files and ${bytes(body.bytes)}${more}. ` +
        `The server exports at most ${number(body.max_files)} files and ${bytes(body.max_bytes)} at a time. Export a folder further down.`
    );
  }
  if (error && error.status === 429) {
    const wait = error.retryAfter || 30;
    if (error.own || /per account/.test(String(body.message || ""))) {
      return notice("warning", "Your export runs", `The server streams one export at a time per account, and yours has not ended yet. Wait until it ends and try again.`);
    }
    return notice("warning", "Two exports run now", `The server streams two exports at a time. Wait ${wait} s and try again.`);
  }
  return problem(error, "The export cannot start");
}

// openExport shows the dialog for a folder (path "" is the root) or for an image manifest.
export async function openExport({ repository, path, title, platform = null }) {
  let info = null;
  let error = null;
  try {
    info = await api.exportInfo(repository, path, platform);
    error = busyError(info);
  } catch (e) {
    error = e;
  }
  const what = title || (path ? path : `the root of ${repository}`);
  const platforms = error && error.status === 400 && error.body && Array.isArray(error.body.platforms) ? error.body.platforms : null;
  if (platforms && platforms.length) {
    await choosePlatform({ repository, path, title: what, platforms });
    return;
  }
  if (error) {
    await openDialog({
      title: `Export ${what}`,
      body: h("div", { class: "stack export-dialog" }, refusal(error)),
      actions: [{ label: "Close", value: "close", kind: "" }]
    });
    return;
  }
  const { commands, note } = usage(info.form, info.name);
  const rows = [
    ["Files", info.exact === false ? `about ${number(info.files)}` : number(info.files)],
    ["Size", info.exact === false ? `about ${bytes(info.bytes)}` : `${bytes(info.bytes)} (${number(info.bytes)} bytes)`],
    ["Form", FORMS[info.form] || info.form],
    ["Archive", h("span", { class: "mono break", text: info.name })]
  ];
  if (info.image) rows.splice(2, 0, ["Image", h("span", { class: "mono break", text: info.image })]);
  if (platform) rows.splice(3, 0, ["Platform", h("span", { class: "mono", text: platform })]);
  const notices = h("div", { class: "export-notices" });
  const body = h(
    "div",
    { class: "stack export-dialog" },
    notices,
    facts(rows),
    info.exact === false ? h("p", { class: "muted small", text: "A group counts the files of its members; the archive holds each path once." }) : null,
    h("section", { class: "detail-section" }, h("h3", { text: "Local use" }), ...commands.map(commandRow), h("p", { class: "muted small", text: note }))
  );
  await openDialog({
    title: `Export ${what}`,
    body,
    actions: [
      { label: "Cancel", value: "cancel", kind: "" },
      {
        label: "Download",
        value: "download",
        kind: "primary",
        handler: async () => {
          // the places may have filled since the dialog opened: ask again right before the download
          let again = null;
          try {
            again = busyError(await api.exportInfo(repository, path, platform));
          } catch (e) {
            again = e;
          }
          if (again) {
            notices.replaceChildren(refusal(again));
            return false;
          }
          notices.replaceChildren();
          const link = h("a", { href: api.exportUrl(repository, path, platform), download: info.name, hidden: true });
          document.body.append(link);
          link.click();
          link.remove();
          toast(`The download of ${info.name} started.`, "info");
          return true;
        }
      }
    ]
  });
}

// An index of several stored platforms exports one of them: the dialog asks which, then asks the
// server for the count and the size of that one.
async function choosePlatform({ repository, path, title, platforms }) {
  const id = `export-platform-${Math.random().toString(36).slice(2, 8)}`;
  const select = h("select", { class: "input", id }, platforms.map((p) => h("option", { value: p, text: p })));
  let chosen = null;
  await openDialog({
    title: `Export ${title}`,
    body: h(
      "div",
      { class: "stack export-dialog" },
      h("p", { text: `The index names ${platforms.length} platforms that this repository stores. An archive holds one image.` }),
      h("div", { class: "field" }, h("label", { class: "label", for: id, text: "Platform" }), select)
    ),
    actions: [
      { label: "Cancel", value: "cancel", kind: "" },
      {
        label: "Continue",
        value: "continue",
        kind: "primary",
        handler: () => {
          chosen = select.value;
          return true;
        }
      }
    ],
    onOpen: () => select.focus()
  });
  if (chosen) await openExport({ repository, path, title, platform: chosen });
}
