// S47 Updates: the installed version, the newest release of the channel with its notes, the
// scheduled update, the history, and the signing state of the install root the server runs from.
// The interface never writes the trusted key and installs nothing: a web session must not be able
// to replace the root of trust. Its one action asks the server to check the releases again.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { time, day } from "../../format.js";
import { render as markdown, safeUrl } from "../../markdown.js";
import { pageHead, failure, button, busy, facts } from "../../ui/page.js";
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

// A panel whose parts stand apart from each other.
function spaced(id, title, ...children) {
  return panel(id, title, h("div", { class: "stack" }, ...children));
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

const STATUS = {
  up_to_date: ["ok", "Up to date"],
  update_available: ["info", "Update available"],
  unknown: ["warn", "Unknown"]
};

const SOURCE = {
  never: ["warn", "Not checked yet"],
  ok: ["ok", "Checked"],
  rate_limited: ["warn", "Rate limited"],
  unreachable: ["error", "Not reachable"],
  error: ["error", "Check failed"],
  unsupported: ["warn", "No release notes"]
};

const RESULT = {
  ok: ["ok", "Healthy"],
  refused: ["warn", "Refused"],
  back: ["error", "Went back"],
  failed: ["error", "Failed"]
};

const RUN = {
  nothing: ["ok", "Nothing new"],
  staged: ["info", "Downloaded, not switched"],
  waiting: ["info", "Waits for the window"],
  switched: ["ok", "Updated"],
  back: ["error", "Update failed, went back"],
  failed: ["error", "Failed"]
};

function stateMark(table, key) {
  const [kind, label] = table[key] || ["warn", key || "Unknown"];
  return mark(kind, label);
}

function muted(text) {
  return h("span", { class: "muted", text });
}

// A link only to an http or https address; anything else is plain text.
function link(href, text, id) {
  if (!safeUrl(href)) return h("span", { id, text });
  return h("a", { href, id, target: "_blank", rel: "noopener noreferrer", text });
}

function releaseLabel(rel) {
  return rel.published_at ? `${rel.tag} (${day(rel.published_at)})` : rel.tag;
}

function updateText(entry) {
  const who = entry.auto ? "by the timer" : "by hand";
  return `${time(entry.time)}: ${entry.from || "?"} to ${entry.to || "?"}, ${who}`;
}

function versionPanel(data) {
  const src = data.source || {};
  const latest = data.latest;
  const checked = h(
    "div",
    { class: "cell-stack" },
    h("span", { id: "source-state" }, stateMark(SOURCE, src.state), src.checked_at ? ` ${time(src.checked_at)}` : ""),
    src.message ? h("span", { class: "muted small break", id: "source-message", text: src.message }) : null,
    src.retry_at ? h("span", { class: "muted small", id: "source-retry", text: `The source answers again from ${time(src.retry_at)}.` }) : null,
    src.skipped ? h("span", { class: "muted small", id: "source-skipped", text: src.skipped }) : null
  );
  let next = muted("Only on the button");
  if (src.background && src.next_check_at) next = h("span", { id: "next-check", text: `${time(src.next_check_at)}, every ${src.interval_hours} hours` });
  else if (src.background) next = h("span", { id: "next-check", text: `Soon, then every ${src.interval_hours} hours` });
  const rows = [
    ["Installed version", mono(data.version, "installed-version")],
    ["Newest release", latest ? h("span", { class: "cell-stack" }, latest.url ? link(latest.url, releaseLabel(latest), "latest-version") : mono(releaseLabel(latest), "latest-version")) : muted("Not known yet")],
    ["Status", h("span", { id: "update-status" }, stateMark(STATUS, data.status))],
    ["Last check", checked],
    ["Next check", next],
    ["Last update", data.last_update ? h("span", { id: "last-update", text: updateText(data.last_update) }) : muted(data.installed ? "No update recorded" : "Not installed through an install root")],
    ["Source", src.name ? mono(src.name, "source-name") : muted("None")]
  ];
  const parts = [facts(rows)];
  if (src.note) parts.push(h("p", { class: "muted small", id: "source-note", text: src.note }));
  if (data.status === "update_available" && data.installed) {
    parts.push(
      h("p", { text: "To install it now, run on the host as root:" }),
      block(`sudo ${word(`${data.root}/current/pipesmith-registry`)} update --fetch --install --root ${word(data.root)}`, "install-command")
    );
  }
  return spaced("version-title", "Version", ...parts);
}

function autoPanel(data) {
  const auto = data.auto;
  const timer = auto.timer || {};
  let timerValue;
  if (!timer.readable) timerValue = h("span", { class: "muted break", id: "timer-state", text: timer.message || "Not readable" });
  else if (timer.message) timerValue = h("span", { class: "break", id: "timer-state", text: timer.message });
  else timerValue = h("span", { id: "timer-state", text: `${timer.unit}: ${timer.active || "unknown"}, ${timer.enabled || "no unit file"}` });
  const run = auto.last_run;
  const lastRun = run
    ? h(
        "div",
        { class: "cell-stack" },
        h("span", { id: "last-run" }, stateMark(RUN, run.result), ` ${time(run.time)}${run.version ? `, ${run.version}` : ""}`),
        run.message ? h("span", { class: run.result === "back" || run.result === "failed" ? "error-text small break" : "muted small break", id: "last-run-message", text: run.message }) : null
      )
    : muted("No run recorded");
  const parts = [];
  if (run && (run.result === "back" || run.result === "failed")) {
    const n = notice(
      "error",
      run.result === "back" ? "The last automatic update failed" : "The last timer run failed",
      run.result === "back"
        ? `The switch to ${run.version} did not answer healthy, so the server went back to the release before by itself.`
        : "The timer run ended with an error before a switch. The message below says why."
    );
    n.id = "auto-failed";
    parts.push(n);
  }
  parts.push(
    facts([
      ["Automatic updates", h("span", { id: "auto-enabled" }, auto.enabled ? mark("ok", "On") : mark("off", "Off"))],
      ["Window", auto.window ? mono(auto.window, "auto-window") : muted("Any time")],
      ["Timer", timerValue],
      ["Next timer run", timer.next ? h("span", { id: "timer-next", text: time(timer.next) }) : muted("Not planned")],
      ["Last timer run", lastRun]
    ])
  );
  if (auto.error) parts.push(h("p", { class: "error-text small break", id: "auto-error", text: auto.error }));
  parts.push(
    h(
      "p",
      { class: "muted small" },
      "Automatic updates are switched on the host in ",
      mono(data.config.path),
      " with scheduled_switch, and the timer with systemctl enable --now pipesmith-registry-update.timer."
    )
  );
  return spaced("auto-title", "Automatic updates", ...parts);
}

function notesOf(rel) {
  const body = rel.body && rel.body.trim() ? markdown(rel.body) : [h("p", { class: "muted", text: "This release has no notes." })];
  return h("div", { class: "release-notes" }, body, rel.truncated ? h("p", { class: "muted small", text: "The notes are cut here; the release page holds all of them." }) : null);
}

function notesPanel(data) {
  const latest = data.latest;
  if (!latest) {
    return panel("notes-title", "Release notes", h("p", { class: "muted", id: "notes-none", text: "No release is known yet. Check for a new version to read the releases." }));
  }
  const older = (data.releases || []).filter((r) => r !== latest && r.tag !== latest.tag);
  const list = older.map((rel) => {
    const details = h(
      "details",
      { class: "release" },
      h("summary", {}, h("span", { class: "mono", text: releaseLabel(rel) }), rel.prerelease ? h("span", { class: "muted small", text: " prerelease" }) : null)
    );
    let filled = false;
    details.addEventListener("toggle", () => {
      if (details.open && !filled) {
        filled = true;
        details.append(notesOf(rel), rel.url ? h("p", { class: "small" }, link(rel.url, "Release page")) : null);
      }
    });
    return details;
  });
  return spaced(
    "notes-title",
    "Release notes",
    h("h3", { id: "notes-latest", text: latest.name && latest.name !== latest.tag ? `${latest.tag}: ${latest.name}` : latest.tag }),
    h("p", { class: "muted small" }, latest.published_at ? `Published ${time(latest.published_at)}. ` : "", latest.url ? link(latest.url, "Release page") : null),
    h("div", { id: "notes-body" }, notesOf(latest)),
    older.length ? h("h3", { text: "Older releases" }) : null,
    older.length ? h("div", { class: "stack", id: "notes-older" }, list) : null
  );
}

function historyPanel(data) {
  const rows = data.history || [];
  if (!rows.length) {
    return panel("history-title", "History", h("p", { class: "muted", id: "history-empty", text: data.history_error || "No update or rollback is recorded yet." }));
  }
  const table = h(
    "table",
    { class: "data compact", id: "history-table" },
    h("thead", {}, h("tr", {}, ["Time", "Action", "From", "To", "Result", "Started", "Reason"].map((t) => h("th", { scope: "col", text: t })))),
    h(
      "tbody",
      {},
      rows.map((e) =>
        h(
          "tr",
          {},
          h("td", { class: "nowrap", text: time(e.time) }),
          h("td", { text: e.action }),
          h("td", { class: "mono", text: e.from }),
          h("td", { class: "mono", text: e.to }),
          h("td", {}, stateMark(RESULT, e.result)),
          h("td", { text: e.auto ? "Timer" : "By hand" }),
          h("td", { class: "break small", text: e.reason })
        )
      )
    )
  );
  return panel("history-title", "History", h("div", { class: "table-wrap" }, table), data.history_error ? h("p", { class: "error-text small", text: data.history_error }) : null);
}

function backgroundPanel(data) {
  const repo = data.repository;
  return spaced(
    "background-title",
    "Background",
    h("p", {
      text:
        "Every release is signed. The server checks the releases of its channel and shows them here. Installing runs on the host as root: update downloads SHA256SUMS, its signature bundle, the binary of this platform and its bill of materials, checks the signature with the trusted key and every checksum, and only then runs the new binary. It switches the link current, restarts the unit and waits for its health; when the new release does not answer healthy, it goes back to the release before by itself."
    }),
    h("p", {
      text: "The timer pipesmith-registry-update.timer runs every hour. It downloads a new release at any time and switches to it only when scheduled_switch is true and the time lies inside the window."
    }),
    repo
      ? h(
          "p",
          { id: "repo-links" },
          "Source code: ",
          link(repo.url, repo.name, "repo-link"),
          ". All releases with their files: ",
          link(repo.releases_url, "Releases", "releases-link"),
          "."
        )
      : null,
    h("p", {}, "The manual describes updates in ", h("a", { href: "manual/#update-layout", target: "_blank", rel: "noopener", text: "Update Layout" }), ".")
  );
}

export async function render(ctx) {
  const check = button("Check for new version", {
    kind: "primary",
    attrs: { id: "updates-check" },
    onClick: (event) => busy(event.currentTarget, () => load(true))
  });
  ctx.main.append(pageHead({ title: "Updates", sub: "The installed version, the releases, the automatic update and the signing state of this install.", actions: [check] }));
  const body = h("div", { class: "stack", id: "updates-body" });
  ctx.main.append(body);

  async function load(fresh) {
    let data;
    try {
      data = fresh ? await api.updatesCheck() : await api.updates();
    } catch (error) {
      failure(body, error, { retry: () => load(false) });
      return;
    }
    const readOnly = notice(
      "info",
      "Read only",
      "The web interface shows the trusted key and never writes it, and it installs nothing. A web session must not be able to replace the root of trust, so the key is changed and updates are installed on the host only."
    );
    readOnly.id = "updates-read-only";
    const read = h("p", { class: "muted small", text: `Read at ${time(Date.now())}.` });
    const parts = [versionPanel(data)];
    if (!data.installed) {
      const missing = notice("warning", "Not installed through an install root", data.message);
      missing.id = "updates-not-installed";
      parts.unshift(missing);
      parts.push(notesPanel(data), backgroundPanel(data), readOnly, read);
      body.replaceChildren(...parts);
      return;
    }
    parts.push(autoPanel(data), notesPanel(data), historyPanel(data), readOnly, keyPanel(data));
    // An update.json the server may not read is no fault of the key: the problem text says what to do.
    if (data.key.state !== "good" && data.key.state !== "unreadable") parts.push(fixPanel(data));
    parts.push(releasePanel(data.release), backgroundPanel(data), read);
    body.replaceChildren(...parts);
  }

  await load(false);
}
