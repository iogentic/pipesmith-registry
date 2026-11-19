// The API page: lists the operations of openapi.json beside it. A classic script, loaded with
// defer after manual.js, which sets the theme and the served mark.
(function () {
  var METHODS = ["get", "put", "post", "delete", "options", "head", "patch"];
  var list = document.getElementById("api-list");
  var count = document.getElementById("api-count");
  var tools = document.getElementById("api-tools");
  var filter = document.getElementById("api-filter");
  var refusedSwitch = document.getElementById("api-refused");
  var problem = document.getElementById("api-error");
  var ops = [];
  var served = 0;
  var refused = 0;

  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }

  // resolve follows a $ref of the form #/a/b inside the document.
  function resolve(doc, value) {
    var ref = value && value.$ref;
    if (typeof ref !== "string" || ref.indexOf("#/") !== 0) return value;
    var at = doc;
    var parts = ref.slice(2).split("/");
    for (var i = 0; i < parts.length && at; i++) at = at[parts[i]];
    return at || value;
  }

  function table(className, heads, rows) {
    var t = el("table", className);
    var head = el("tr");
    heads.forEach(function (text) {
      head.appendChild(el("th", null, text));
    });
    var thead = el("thead");
    thead.appendChild(head);
    t.appendChild(thead);
    var body = el("tbody");
    rows.forEach(function (row) {
      var tr = el("tr");
      row.forEach(function (cell, i) {
        var td = el("td", i === 0 ? "mono" : null, cell);
        tr.appendChild(td);
      });
      body.appendChild(tr);
    });
    t.appendChild(body);
    return t;
  }

  function operation(doc, method, path, op) {
    var details = el("details", "op");
    details.dataset.method = method.toUpperCase();
    details.dataset.path = path;
    details.dataset.refused = op["x-pipesmith-refused"] ? "true" : "false";
    var summary = el("summary");
    summary.appendChild(el("span", "method method-" + method, method.toUpperCase()));
    summary.appendChild(el("code", "op-path", path));
    summary.appendChild(el("span", "op-summary", op.summary || ""));
    summary.appendChild(el("span", "op-perm", op["x-pipesmith-permission"] || ""));
    details.appendChild(summary);

    var body = el("div", "op-body");
    if (op.description) body.appendChild(el("p", null, op.description));
    if (op["x-pipesmith-needs"]) {
      var needs = op["x-pipesmith-needs"].split(", ");
      var modes = needs.map(function (n) { return n === "writes" ? "writes on" : "security on"; });
      body.appendChild(el("p", "op-needs", "Needs " + needs.join(" and ") + ": the operation exists only with " + modes.join(" and ") + "."));
    }
    var params = (op.parameters || []).map(function (p) {
      p = resolve(doc, p);
      return [p.name, p.in, (p.schema && p.schema.type) || "", p.description || ""];
    });
    if (op.requestBody) {
      var content = op.requestBody.content || {};
      var type = Object.keys(content)[0] || "";
      params.push(["body", "body", type, op.requestBody.description || ""]);
    }
    if (params.length) {
      body.appendChild(el("h4", null, "Parameters"));
      body.appendChild(table("params", ["Name", "Where", "Type", "Meaning"], params));
    }
    var answers = Object.keys(op.responses || {}).map(function (status) {
      var answer = resolve(doc, op.responses[status]);
      var types = Object.keys(answer.content || {}).join(", ");
      return [status, types, answer.description || ""];
    });
    body.appendChild(el("h4", null, "Answers"));
    body.appendChild(table("answers", ["Status", "Content type", "Meaning"], answers));
    details.appendChild(body);
    return details;
  }

  function countText(matching) {
    var text = served + " operations are served, " + refused + " are not served yet.";
    if (matching !== null) text += " " + matching + (matching === 1 ? " operation matches" : " operations match") + " the filter.";
    return text;
  }

  function apply() {
    var words = filter.value.trim().toLowerCase();
    var showRefused = refusedSwitch.checked;
    var matching = 0;
    ops.forEach(function (o) {
      var match = !words || o.text.indexOf(words) >= 0;
      var shown = match && (showRefused || !o.refused);
      o.node.hidden = !shown;
      if (shown) matching++;
    });
    var sections = list.querySelectorAll("section.api-tag");
    for (var i = 0; i < sections.length; i++) {
      sections[i].hidden = !sections[i].querySelector("details.op:not([hidden])");
    }
    count.textContent = countText(words ? matching : null);
  }

  function show(doc) {
    var version = (doc.info && doc.info.version) || "";
    document.getElementById("api-version").textContent = version;
    document.title = "Pipesmith Registry API " + version;
    var byTag = {};
    (doc.tags || []).forEach(function (tag) {
      var section = el("section", "api-tag");
      section.appendChild(el("h2", null, tag.name));
      if (tag.description) section.appendChild(el("p", "api-tag-text", tag.description));
      byTag[tag.name] = section;
      list.appendChild(section);
    });
    Object.keys(doc.paths || {}).forEach(function (path) {
      var item = doc.paths[path];
      METHODS.forEach(function (method) {
        var op = item[method];
        if (!op) return;
        var node = operation(doc, method, path, op);
        var section = byTag[(op.tags || [])[0]];
        if (!section) return;
        section.appendChild(node);
        var isRefused = !!op["x-pipesmith-refused"];
        if (isRefused) refused++;
        else served++;
        ops.push({ node: node, refused: isRefused, text: (method + " " + path + " " + (op.summary || "")).toLowerCase() });
      });
    });
    tools.hidden = false;
    filter.addEventListener("input", apply);
    refusedSwitch.addEventListener("change", apply);
    apply();
  }

  function fail() {
    problem.textContent = "The OpenAPI document could not be loaded; the download link above still offers it.";
    problem.hidden = false;
    count.textContent = "";
  }

  if (!window.fetch) {
    fail();
    return;
  }
  fetch("openapi.json")
    .then(function (answer) {
      if (!answer.ok) throw new Error("openapi.json answered " + answer.status);
      return answer.json();
    })
    .then(show)
    .catch(fail);
})();
