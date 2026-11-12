// S06 Upload: the hosted Maven, npm and raw repositories the session may add to. Docker images
// arrive through the registry protocol and are not offered here (decision U21).

import { api } from "../../api.js";
import { formatName } from "../../format.js";
import { pageHead, failure } from "../../ui/page.js";
import { dataTable } from "../../ui/table.js";
import { urlCell } from "../repositories/list.js";
import { mayUpload } from "./common.js";

const ASKS = {
  maven2: "Coordinates and up to three files, or a POM file",
  npm: "One package file made by npm pack",
  raw: "A folder and up to three files"
};

export async function render(ctx) {
  ctx.main.append(
    pageHead({
      title: "Upload",
      sub: "Pick the repository to upload to. Hosted Maven, npm and raw repositories take uploads here. Docker images arrive through docker push."
    })
  );
  let repos;
  try {
    repos = await api.repositories.list();
  } catch (error) {
    failure(ctx.main, error, { retry: () => location.reload() });
    return;
  }
  const rows = (repos || []).filter((r) => mayUpload(ctx, r));
  const table = dataTable(
    [
      { key: "name", label: "Name", class: "break" },
      { key: "format", label: "Format", value: (r) => formatName(r.format) },
      { key: "asks", label: "The form asks for", sort: false, value: (r) => ASKS[r.format] },
      { key: "url", label: "Address", sort: false, render: (r) => urlCell(r.url) }
    ],
    {
      rows,
      sort: { key: "name", dir: "asc" },
      rowHref: (r) => `#/upload/${encodeURIComponent(r.name)}`,
      caption: "Repositories that take uploads",
      filterLabel: "Filter the repositories",
      empty: {
        title: "No upload rights",
        text: "Uploads go to hosted Maven, npm and raw repositories that one of your roles lets you add to. An administrator can add that right."
      }
    }
  );
  ctx.main.append(table.el);
}
