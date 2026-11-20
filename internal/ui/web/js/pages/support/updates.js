// S47 Updates: the update and signing state of the install root the server runs from, read only.
// The interface never writes the trusted key: a web session must not be able to replace the root of
// trust. When the key is missing or refused, the page says which file to change and how.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { time } from "../../format.js";
import { pageHead, failure, button, busy } from "../../ui/page.js";
import { mark, notice } from "../../ui/notice.js";
import { copyButton } from "../repositories/list.js";

const STATES = {
  good: ["ok", "Trusted"],
  missing: ["error", "Missing"],
  refused: ["error", "Refused"],
  unreadable: ["warn", "Not readable"],
  unchecked: ["warn", "Not checked"]
};

// A path as one word of a shell command: quoted when it holds anything but plain path characters.
function word(path) {
  return /^[A-Za-z0-9_.\/:@%+=-]+$/.test(path) ? path : `'${path.replaceAll("'", "'\\''")}'`;
}

function mono(text, id) {
  return h("span", { class: "mono break", id, text });
}

function copyable(text, id) {
  return text ? h("span", { class: "url-cell" }, mono(text, id), copyButton(text)) : null;
}

function block(text, id) {
  return h("div", { class: "export-command" }, h("pre", { class: "log", id, text }), copyButton(text));
}

function panel(id, title, ...children) {
  return h("section", { class: "panel sys-panel", "aria-labelledby": id }, h("h2", { id, text: title }), ...children);
}

function checkValue(check) {
  if (!check.ran) return mark("warn", "Not checked");
  if (check.passed) return mark("ok", "Passed");
  return h("div", { class: "cell-stack" }, mark("error", "Refused"), h("span", { class: "error-text mono break", id: "check-message", text: check.message }));
}

function keyPanel(data) {
  const { config, key, check } = data;
  const [kind, label] = STATES[key.state] || ["warn", key.state];
  return panel(
    "key-title",
    "Trusted key",
    h("p", { id: "key-state" }, mark(kind, label), key.problem ? h("span", { class: "break", text: ` ${key.problem}` }) : null),
    h(
      "dl",
      { class: "facts" },
      ...[
        ["Install root", mono(data.root, "install-root")],
        ["update.json", h("div", { class: "cell-stack" }, mono(config.path, "config-path"), h("span", { class: "muted small", text: config.found ? "Exists" : "Does not exist" }))],
        ["trusted_key", key.path ? mono(key.path, "key-path") : h("span", { class: "muted", text: "Not named" })],
        ["Key file", key.path ? h("span", { id: "key-found", text: key.found ? "Exists" : "Does not exist" }) : h("span", { class: "muted", text: "None" })],
        ["Owner and mode", checkValue(check)],
        ["SHA-256", copyable(key.sha256, "key-sha256") || h("span", { class: "muted", text: "None" })],
        ["Key hint", copyable(key.hint, "key-hint") || h("span", { class: "muted", text: "None" })]
      ].flatMap(([dt, dd]) => [h("dt", { text: dt }), h("dd", {}, dd)])
    ),
    h("p", { class: "muted small", text: "SHA-256 is the fingerprint of the key in DER form, as sha256sum prints it. The key hint is the same value in base64, as cosign writes it into a bundle." })
  );
}

function releasePanel(rel) {
  return panel(
    "release-title",
    "Release",
    h(
      "dl",
      { class: "facts" },
      h("dt", { text: "Version" }),
      h("dd", {}, rel.name ? mono(rel.name, "release-name") : h("span", { class: "muted", text: "Unknown" })),
      h("dt", { text: "Source" }),
      h("dd", { id: "release-note", text: rel.note })
    )
  );
}

// The steps of the manual, section Update Layout, with the paths of this install root.
function fixPanel(data) {
  const key = word(`${data.root}/trusted.pub`);
  const config = word(data.config.path);
  const json = `{\n  "trusted_key": "trusted.pub",\n  "data_dir": "/var/lib/pipesmith-registry"\n}`;
  return panel(
    "fix-title",
    "Key setup",
    h("p", { text: "Update installs only a release signed with the trusted key. Set the key on the host, as root:" }),
    h(
      "ol",
      { class: "stack", id: "fix-steps" },
      h(
        "li",
        {},
        h("p", { text: "Take cosign.pub of the release and compare its fingerprint with the one in the release notes." }),
        block("openssl pkey -pubin -in cosign.pub -outform DER | sha256sum", "fix-fingerprint")
      ),
      h(
        "li",
        {},
        h("p", { text: "Install it as the trusted key in the install root, owned by root and not writable by group or others." }),
        block(`sudo install -o root -g root -m 0644 cosign.pub ${key}`, "fix-install")
      ),
      h(
        "li",
        {},
        h("p", {}, "Name the key under trusted_key in ", mono(data.config.path), ". A relative path lies below the install root. Keep the other keys of the file, data_dir among them."),
        block(json, "fix-config")
      ),
      h(
        "li",
        {},
        h("p", { text: "update.json, the key and the folder of the key belong to root or to the owner of the install root. Neither group nor others may write them." }),
        block(`sudo chown root:root ${config} ${key}\nsudo chmod go-w ${config} ${key}`, "fix-mode")
      )
    ),
    h("p", {}, "The manual describes the layout in ", h("a", { href: "manual/#update-layout", target: "_blank", rel: "noopener", text: "Update Layout" }), ". Check again after the change.")
  );
}

export async function render(ctx) {
  const again = button("Check again", { attrs: { id: "updates-refresh" }, onClick: (event) => busy(event.currentTarget, load) });
  ctx.main.append(pageHead({ title: "Updates", sub: "The update and signing state of this install, read only.", actions: [again] }));
  const body = h("div", { class: "stack", id: "updates-body" });
  ctx.main.append(body);

  async function load() {
    let data;
    try {
      data = await api.updates();
    } catch (error) {
      failure(body, error, { retry: load });
      return;
    }
    const readOnly = notice(
      "info",
      "Read only",
      "The web interface shows the trusted key and never writes it. A web session must not be able to replace the root of trust, so the key is changed on the host only."
    );
    readOnly.id = "updates-read-only";
    const read = h("p", { class: "muted small", text: `Read at ${time(Date.now())}.` });
    if (!data.installed) {
      const missing = notice("warning", "Not installed through an install root", data.message);
      missing.id = "updates-not-installed";
      body.replaceChildren(missing, readOnly, read);
      return;
    }
    const parts = [readOnly, keyPanel(data)];
    // An update.json the server may not read is no fault of the key: the problem text says what to do.
    if (data.key.state !== "good" && data.key.state !== "unreadable") parts.push(fixPanel(data));
    parts.push(releasePanel(data.release), read);
    body.replaceChildren(...parts);
  }

  await load();
}
