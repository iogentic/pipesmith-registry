// S37 Node: the identity of this server and its read only mode. Freeze, release and, when the
// system froze the node, force release; each asks first, and the band of the frame follows.

import { h } from "../../dom.js";
import { api } from "../../api.js";
import { pageHead, failure, button, busy, facts } from "../../ui/page.js";
import { notice, toast, errorText, mark, toastProblem } from "../../ui/notice.js";
import { confirm } from "../../ui/dialog.js";
import { copyButton } from "../repositories/list.js";

export async function render(ctx) {
  // The node page is where a freeze ends, so its controls follow the roles, not the read only state.
  const canChange = ctx.allowed("nexus:settings:update");
  ctx.main.append(pageHead({ title: "Node", sub: "The identity of this server and whether it accepts writes." }));
  let node;
  let state;
  try {
    [node, state] = await Promise.all([api.node(), api.readOnly.get()]);
  } catch (error) {
    failure(ctx.main, error, { retry: () => ctx.navigate("/admin/node", { replace: true }) });
    return;
  }

  const identity = h(
    "section",
    { class: "panel sys-panel", "aria-labelledby": "node-identity" },
    h("h2", { id: "node-identity", text: "Identity" }),
    facts([["Node id", h("span", { class: "url-cell" }, h("span", { class: "mono break", id: "node-id", text: node.nodeId }), copyButton(node.nodeId, "Copy"))]])
  );
  const modeBody = h("div", { class: "stack" });
  const mode = h("section", { class: "panel sys-panel", "aria-labelledby": "node-mode" }, h("h2", { id: "node-mode", text: "Read only mode" }), modeBody);
  ctx.main.append(h("div", { class: "stack" }, canChange ? null : notice("info", "Read only page", "Your roles let you read the node, not freeze or release it."), identity, mode));

  function draw() {
    const frozen = Boolean(state.frozen);
    const rows = [["State", frozen ? mark("error", "Read only") : mark("ok", "Writable")]];
    if (frozen) {
      rows.push(["Frozen by", state.systemInitiated ? "The system" : "An administrator"]);
      rows.push(["Reason", state.summaryReason || null]);
    }
    const actions = h("div", { class: "actions" });
    if (canChange && !frozen) actions.append(button("Freeze", { kind: "primary", attrs: { id: "freeze" }, onClick: (event) => change(event.currentTarget, "freeze") }));
    if (canChange && frozen && !state.systemInitiated) actions.append(button("Release", { kind: "primary", attrs: { id: "release" }, onClick: (event) => change(event.currentTarget, "release") }));
    if (canChange && frozen && state.systemInitiated) actions.append(button("Force release", { kind: "danger", attrs: { id: "force-release" }, onClick: (event) => change(event.currentTarget, "force") }));
    modeBody.replaceChildren(
      h(
        "p",
        { class: "muted" },
        frozen
          ? "The node serves downloads and refuses uploads, deletes and other writes."
          : "A frozen node keeps serving downloads and refuses uploads, deletes and other writes, for example during a backup of the blob stores."
      ),
      facts(rows),
      ...(actions.childNodes.length ? [actions] : [])
    );
  }

  const ASK = {
    freeze: {
      title: "Freeze the node?",
      text: "The node becomes read only. Downloads keep working; uploads, deletes and other writes are refused until the node is released.",
      confirmLabel: "Freeze",
      danger: false,
      call: () => api.readOnly.freeze(),
      done: "The node is read only."
    },
    release: {
      title: "Release the node?",
      text: "The node accepts uploads and other writes again.",
      confirmLabel: "Release",
      danger: false,
      call: () => api.readOnly.release(),
      done: "The node accepts writes again."
    },
    force: {
      title: "Force the release?",
      text: "The system froze the node for the reason shown. Release it only after that cause is fixed; writes before that can leave the index or a blob store inconsistent.",
      confirmLabel: "Force release",
      danger: true,
      call: () => api.readOnly.forceRelease(),
      done: "The node accepts writes again."
    }
  };

  async function change(buttonEl, what) {
    const ask = ASK[what];
    const ok = await confirm({ title: ask.title, text: ask.text, confirmLabel: ask.confirmLabel, danger: ask.danger });
    if (!ok) return;
    try {
      await busy(buttonEl, ask.call);
      toast(ask.done);
    } catch (error) {
      toastProblem(error);
    }
    try {
      state = await api.readOnly.get();
    } catch (error) {
      /* the band and the page keep the last state they read */
    }
    draw();
    const next = modeBody.querySelector(".actions button");
    if (next) next.focus();
    await ctx.refreshReadOnly();
  }

  draw();
}
