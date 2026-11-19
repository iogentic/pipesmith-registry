// Theme and served mark of the manual and the API page, and the operations table of the manual.
// A classic script, loaded with defer on both pages; the table code finds no table on the API page.
(function () {
  var UI_KEY = "pipesmith-theme"; // the key of the interface
  var KEY = "pipesmith-registry-docs-theme"; // the key of the manual
  var root = document.documentElement;

  function stored(key) {
    try {
      var value = localStorage.getItem(key);
      return value === "light" || value === "dark" ? value : null;
    } catch (e) {
      return null;
    }
  }

  function system() {
    return window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
  }

  root.setAttribute("data-theme", stored(UI_KEY) || stored(KEY) || system());

  // Served by the binary when the last folder of the address is manual, also below a prefix.
  if (/\/manual\/[^\/]*$/.test(location.pathname)) {
    root.setAttribute("data-served", "registry");
  } else {
    root.removeAttribute("data-served");
  }

  var button = document.getElementById("theme-toggle");
  function label() {
    button.textContent = root.getAttribute("data-theme") === "dark" ? "Light theme" : "Dark theme";
  }
  if (button) {
    button.addEventListener("click", function () {
      var next = root.getAttribute("data-theme") === "dark" ? "light" : "dark";
      root.setAttribute("data-theme", next);
      try {
        localStorage.setItem(KEY, next);
      } catch (e) {
        /* storage refused: the pick holds for this page */
      }
      label();
    });
    label();
  }

  // Print form: a cover, a contents page and numbered headings, built only while the page is
  // printed. preparePrint(pages) takes an object that maps a heading id to its page number;
  // without it the contents show no page numbers. A second call replaces what the first built.
  var printed = null;

  function make(tag, className, text) {
    var element = document.createElement(tag);
    if (className) element.className = className;
    if (text) element.textContent = text;
    return element;
  }

  function slug(text) {
    return text.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  }

  function restorePrint() {
    if (!printed) return;
    printed.added.forEach(function (node) {
      if (node.parentNode) node.parentNode.removeChild(node);
    });
    printed.ids.forEach(function (element) {
      element.removeAttribute("id");
    });
    printed.marked.forEach(function (link) {
      link.classList.remove("print-href");
      if (!link.getAttribute("class")) link.removeAttribute("class");
    });
    printed.unlinked.forEach(function (pair) {
      pair[0].setAttribute("href", pair[1]);
    });
    root.setAttribute("data-theme", printed.theme);
    printed = null;
  }

  function preparePrint(pages) {
    restorePrint();
    var state = { theme: root.getAttribute("data-theme"), added: [], ids: [], marked: [], unlinked: [] };
    printed = state;
    root.setAttribute("data-theme", "light");

    // Chapters and their sections, numbered as in the chapter list.
    var heads = [];
    var sections = document.querySelectorAll("main > section");
    var chapter = 0;
    for (var i = 0; i < sections.length; i++) {
      var h2 = sections[i].querySelector(":scope > h2");
      if (!h2) continue;
      chapter++;
      heads.push({ section: sections[i], head: h2, number: String(chapter), level: 2 });
      var h3s = sections[i].querySelectorAll("h3");
      for (var j = 0; j < h3s.length; j++) {
        heads.push({ section: sections[i], head: h3s[j], number: chapter + "." + (j + 1), level: 3 });
      }
    }
    heads.forEach(function (item) {
      var text = item.head.textContent.trim();
      item.title = text;
      if (item.level === 2) {
        item.id = item.section.id;
      } else {
        if (!item.head.id) {
          var id = item.section.id + "-" + slug(text);
          while (document.getElementById(id)) id += "-x";
          item.head.id = id;
          state.ids.push(item.head);
        }
        item.id = item.head.id;
      }
      var number = make("span", "number", item.number);
      var gap = document.createTextNode(" ");
      item.head.insertBefore(gap, item.head.firstChild);
      item.head.insertBefore(number, gap);
      state.added.push(number, gap);
    });

    // Cover: logo, name, "Manual", the version of the header, the licence.
    var cover = make("section", "print-cover");
    var logo = document.querySelector("header.top img");
    if (logo) {
      var img = make("img", "print-logo");
      img.src = logo.getAttribute("src");
      img.alt = logo.getAttribute("alt") || "";
      cover.appendChild(img);
    }
    cover.appendChild(make("p", "print-name", "Pipesmith Registry"));
    cover.appendChild(make("p", "print-title", "Manual"));
    var shown = document.querySelector("header.top .version");
    var version = shown ? (shown.textContent.match(/(\d[\w.+-]*)\s*$/) || [])[1] : null;
    if (version) cover.appendChild(make("p", "print-version", "Version " + version));
    cover.appendChild(make("p", "print-licence", "Apache License 2.0"));

    // Contents, in the parts of the chapter list.
    var contents = make("nav", "print-contents");
    contents.setAttribute("aria-label", "Contents");
    contents.appendChild(make("p", "print-contents-title", "Contents"));
    var byId = {};
    heads.forEach(function (item) {
      if (item.level === 2) byId[item.id] = item;
    });
    var toc = document.querySelector("nav.toc");
    var list = null;
    var children = toc ? toc.children : [];
    for (var k = 0; k < children.length; k++) {
      var child = children[k];
      if (child.classList.contains("part")) {
        contents.appendChild(make("p", "print-part", child.textContent.trim()));
        list = null;
        continue;
      }
      var links = child.querySelectorAll("a[href^='#']");
      for (var m = 0; m < links.length; m++) {
        var head = byId[links[m].getAttribute("href").slice(1)];
        if (!head) continue;
        if (!list) {
          list = make("ol", "print-list");
          contents.appendChild(list);
        }
        heads.forEach(function (item) {
          if (item.section !== head.section) return;
          var row = make("li", item.level === 2 ? "print-chapter" : "print-section");
          var link = make("a", null);
          link.href = "#" + item.id;
          link.appendChild(make("span", "number", item.number));
          link.appendChild(document.createTextNode(" " + item.title));
          row.appendChild(link);
          row.appendChild(make("span", "print-fill"));
          var page = pages && pages[item.id] != null ? String(pages[item.id]) : "";
          row.appendChild(make("span", "print-page", page));
          list.appendChild(row);
        });
      }
    }

    document.body.insertBefore(contents, document.body.firstChild);
    document.body.insertBefore(cover, document.body.firstChild);
    state.added.push(cover, contents);

    // A link to another page of this folder leads nowhere on paper: it prints as plain text.
    var local = document.querySelectorAll("main a[href], footer a[href]");
    for (var q = 0; q < local.length; q++) {
      var target = local[q].getAttribute("href");
      if (target.charAt(0) === "#" || /^https?:/.test(target)) continue;
      state.unlinked.push([local[q], target]);
      local[q].removeAttribute("href");
    }

    // An outside link prints its address after the text where the text is not the address.
    var outside = document.querySelectorAll("main a[href^='http'], footer a[href^='http']");
    for (var n = 0; n < outside.length; n++) {
      if (outside[n].textContent.trim() !== outside[n].getAttribute("href")) {
        outside[n].classList.add("print-href");
        state.marked.push(outside[n]);
      }
    }
  }

  window.pipesmithManual = { preparePrint: preparePrint, restorePrint: restorePrint };
  // A page prepared by a caller with page numbers keeps them when the browser prints it.
  window.addEventListener("beforeprint", function () {
    if (!printed) preparePrint();
  });
  window.addEventListener("afterprint", restorePrint);

  // The table of served operations in the chapter API, filled from the OpenAPI document beside the
  // page. Refused operations are left out; a failed fetch shows the note with the link to api.html.
  var METHODS = ["get", "put", "post", "delete", "options", "head", "patch"];
  var tables = document.querySelectorAll("table[data-openapi]");
  for (var k = 0; k < tables.length; k++) operations(tables[k]);

  function cell(row, text, code) {
    var td = document.createElement("td");
    if (code) {
      var c = document.createElement("code");
      c.textContent = text;
      td.appendChild(c);
    } else {
      td.textContent = text;
    }
    row.appendChild(td);
  }

  function operations(table) {
    var src = table.getAttribute("data-openapi");
    var problem = document.getElementById("ops-error");
    function fail() {
      if (problem) problem.hidden = false;
    }
    if (!window.fetch) {
      fail();
      return;
    }
    fetch(src)
      .then(function (answer) {
        if (!answer.ok) throw new Error(src + " answered " + answer.status);
        return answer.json();
      })
      .then(function (doc) {
        var body = table.querySelector("tbody");
        Object.keys(doc.paths || {}).forEach(function (path) {
          METHODS.forEach(function (method) {
            var op = doc.paths[path][method];
            if (!op || op["x-pipesmith-refused"]) return;
            var row = document.createElement("tr");
            cell(row, method.toUpperCase(), true);
            cell(row, path, true);
            cell(row, op.summary || "", false);
            var who = op["x-pipesmith-permission"] || "";
            if (op["x-pipesmith-needs"]) who += " (needs " + op["x-pipesmith-needs"].split(", ").join(" and ") + ")";
            cell(row, who, false);
            body.appendChild(row);
          });
        });
        table.hidden = false;
      })
      .catch(fail);
  }
})();
