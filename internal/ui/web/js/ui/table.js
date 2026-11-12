// Data table: filter, sort by column, pages, empty state. Below 700 pixels each row becomes a block
// with the column name above each cell, so no table scrolls sideways.
//
// columns: [{ key, label, value?(row), render?(row) -> node|string, sort?: false, num?: true, class? }]
// options: { rows, rowHref?(row), filter?: true, filterLabel?, pageSize?: 25, sort?: {key, dir},
//            empty: {title, text, actions}, caption, more?: {label, load() -> rows or null} }

import { h, replace } from "../dom.js";
import { emptyState } from "./page.js";

const SIZES = [25, 50, 100];

function cellValue(column, row) {
  if (column.value) return column.value(row);
  return row[column.key];
}

function compare(a, b) {
  if (a === b) return 0;
  if (a === null || a === undefined || a === "") return 1;
  if (b === null || b === undefined || b === "") return -1;
  if (typeof a === "number" && typeof b === "number") return a - b;
  return String(a).localeCompare(String(b), "en", { numeric: true, sensitivity: "base" });
}

export function dataTable(columns, options = {}) {
  const state = {
    rows: options.rows || [],
    filter: "",
    sort: options.sort || null,
    page: 0,
    size: options.pageSize || 25
  };
  const root = h("div", { class: "data-table" });
  const tools = h("div", { class: "table-tools" });
  const holder = h("div");
  const pager = h("div", { class: "pager" });
  const status = h("p", { class: "visually-hidden", "aria-live": "polite" });

  let filterInput = null;
  if (options.filter !== false) {
    const id = `filter-${Math.random().toString(36).slice(2, 8)}`;
    filterInput = h("input", { class: "input", id, type: "search", autocomplete: "off", placeholder: "Filter" });
    filterInput.addEventListener("input", () => {
      state.filter = filterInput.value.trim().toLowerCase();
      state.page = 0;
      draw();
    });
    tools.append(h("div", { class: "filter" }, h("label", { class: "visually-hidden", for: id, text: options.filterLabel || "Filter the rows" }), filterInput));
  }
  // Below 700 pixels the column heads are hidden, so sorting moves into a list.
  const sortable = columns.filter((c) => c.sort !== false);
  let sortSelect = null;
  if (sortable.length > 1) {
    const sortId = `sort-${Math.random().toString(36).slice(2, 8)}`;
    sortSelect = h(
      "select",
      { class: "select", id: sortId },
      h("option", { value: "", text: "As listed" }),
      sortable.flatMap((c) => [h("option", { value: `${c.key}:asc`, text: `${c.label}, A to Z` }), h("option", { value: `${c.key}:desc`, text: `${c.label}, Z to A` })])
    );
    sortSelect.addEventListener("change", () => {
      const [key, dir] = sortSelect.value.split(":");
      state.sort = key ? { key, dir } : null;
      state.page = 0;
      draw();
    });
    tools.append(h("div", { class: "sort-narrow" }, h("label", { for: sortId, text: "Sort by" }), sortSelect));
  }
  if (options.tools) tools.append(h("div", { class: "actions" }, options.tools));
  if (tools.childNodes.length) root.append(tools);
  root.append(holder, pager, status);

  function visible() {
    let rows = state.rows;
    if (state.filter) {
      rows = rows.filter((row) =>
        columns.some((c) => {
          const v = cellValue(c, row);
          return v !== null && v !== undefined && String(Array.isArray(v) ? v.join(" ") : v).toLowerCase().includes(state.filter);
        })
      );
    }
    if (state.sort) {
      const column = columns.find((c) => c.key === state.sort.key);
      if (column) {
        rows = [...rows].sort((a, b) => compare(cellValue(column, a), cellValue(column, b)) * (state.sort.dir === "desc" ? -1 : 1));
      }
    }
    return rows;
  }

  function header() {
    return h(
      "thead",
      {},
      h(
        "tr",
        {},
        columns.map((c) => {
          const sorted = state.sort && state.sort.key === c.key;
          const ariaSort = sorted ? (state.sort.dir === "desc" ? "descending" : "ascending") : null;
          if (c.sort === false) return h("th", { scope: "col", class: c.num && "num", text: c.label });
          const arrow = sorted ? (state.sort.dir === "desc" ? "↓" : "↑") : "";
          const btn = h("button", { type: "button", class: "sort" }, c.label, h("span", { class: "arrow", "aria-hidden": "true", text: arrow }));
          btn.addEventListener("click", () => {
            state.sort = sorted && state.sort.dir === "asc" ? { key: c.key, dir: "desc" } : { key: c.key, dir: "asc" };
            draw();
            const again = holder.querySelector(`th[data-key="${CSS.escape(c.key)}"] .sort`);
            if (again) again.focus();
          });
          return h("th", { scope: "col", class: c.num && "num", "aria-sort": ariaSort, dataset: { key: c.key } }, btn);
        })
      )
    );
  }

  function row(item) {
    const href = options.rowHref ? options.rowHref(item) : null;
    const tr = h("tr", { class: href && "clickable" });
    columns.forEach((c, i) => {
      let content = c.render ? c.render(item) : cellValue(c, item);
      if (content === null || content === undefined || content === "") content = h("span", { class: "muted", text: "None" });
      if (i === 0 && href) content = h("a", { class: "row-link break", href }, content);
      tr.append(h("td", { class: [c.num && "num", c.class], "data-label": c.label }, content));
    });
    if (href) {
      tr.addEventListener("click", (event) => {
        if (event.target.closest("a, button, input, select, label")) return;
        if (window.getSelection && String(window.getSelection())) return;
        location.hash = href.replace(/^#/, "");
      });
    }
    return tr;
  }

  function drawPager(total) {
    replace(pager);
    const pages = Math.max(1, Math.ceil(total / state.size));
    if (state.page >= pages) state.page = pages - 1;
    const first = total ? state.page * state.size + 1 : 0;
    const last = Math.min(total, (state.page + 1) * state.size);
    pager.append(h("span", { class: "muted", text: total ? `${first} to ${last} of ${total}` : "No rows" }));
    const nav = h("div", { class: "pages" });
    const prev = h("button", { type: "button", class: "btn small", text: "Previous", disabled: state.page === 0 });
    const next = h("button", { type: "button", class: "btn small", text: "Next", disabled: state.page >= pages - 1 });
    prev.addEventListener("click", () => {
      state.page -= 1;
      draw();
    });
    next.addEventListener("click", () => {
      state.page += 1;
      draw();
    });
    if (pages > 1) nav.append(prev, h("span", { class: "muted", text: `Page ${state.page + 1} of ${pages}` }), next);
    const sizeId = `size-${Math.random().toString(36).slice(2, 8)}`;
    const size = h("select", { class: "select", id: sizeId }, SIZES.map((n) => h("option", { value: String(n), selected: n === state.size, text: String(n) })));
    size.addEventListener("change", () => {
      state.size = Number(size.value);
      state.page = 0;
      draw();
    });
    if (total > SIZES[0]) pager.append(nav, h("div", { class: "size" }, h("label", { for: sizeId, text: "Rows per page" }), size));
    else pager.append(nav);
    if (options.more && options.more.available()) {
      const more = h("button", { type: "button", class: "btn small", text: options.more.label || "Load more" });
      more.addEventListener("click", async () => {
        more.disabled = true;
        more.setAttribute("aria-busy", "true");
        try {
          const added = await options.more.load();
          if (added) state.rows = state.rows.concat(added);
        } finally {
          draw();
        }
      });
      pager.append(more);
    }
  }

  function draw() {
    tools.hidden = !state.rows.length && !(options.more && options.more.available());
    if (sortSelect) sortSelect.value = state.sort ? `${state.sort.key}:${state.sort.dir}` : "";
    const rows = visible();
    if (!state.rows.length) {
      replace(holder, h("div", { class: "table-wrap" }, emptyState(options.empty || { title: "Nothing here yet" })));
      replace(pager);
      if (options.more && options.more.available()) drawPager(0);
      if (filterInput) filterInput.disabled = true;
      return;
    }
    if (filterInput) filterInput.disabled = false;
    if (!rows.length) {
      replace(holder, h("div", { class: "table-wrap" }, emptyState({ title: "No row matches the filter", text: "Change or clear the filter to see rows again." })));
      drawPager(0);
      status.textContent = "No row matches the filter.";
      return;
    }
    const start = state.page * state.size;
    const page = rows.slice(start, start + state.size);
    const table = h(
      "table",
      { class: "data" },
      options.caption ? h("caption", { class: "visually-hidden", text: options.caption }) : null,
      header(),
      h("tbody", {}, page.map(row))
    );
    replace(holder, h("div", { class: "table-wrap" }, table));
    drawPager(rows.length);
    status.textContent = state.filter ? `${rows.length} rows match the filter.` : "";
  }

  draw();
  return {
    el: root,
    setRows(rows) {
      state.rows = rows || [];
      draw();
    },
    rows: () => state.rows
  };
}
