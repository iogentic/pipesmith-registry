# Local Changes

`web/` holds the files of `ui/web` of the interface repository at commit 1e6d884, taken over on 2026-11-12. The files in the table were changed here afterwards, and the interface repository can take the same change. `TestEmbeddedFilesAreTheTakenCommitAndTheListedChanges` in `ui_test.go` pins the git blob id of each of them at the taken commit and here.

| File | Date | Reason |
| --- | --- | --- |
| `web/js/preview.js` | 2026-11-18 | After a sign in and after every reload the page waited for all probes, and the probe of the search screen was a search. A search without a hit reads every row of the index, so on a large index the page waited for a walk over all of it. Each entry of `PROBES` now names its read request beside its probe, and the search screen has a request but no probe. `fromAreas` marks the areas from the refused requests of `GET /api/areas` and sends nothing. `probe` marks each area when its answer arrives, keeps a result the screen learned from its own request meanwhile, and drops answers that arrive after a sign out. |
| `web/js/app.js` | 2026-11-18 | The reason is the one of `preview.js`. `loadAreas` marks the areas through `fromAreas` and returns whether the answer came. `start` and `signedIn` start the probes only without that answer, and they no longer wait for them. |
| `web/css/tokens.css` | 2026-11-18 | `--content-max` held the content area of every screen to 1180 pixels. On a window 2000 pixels wide the content of Browse ended at 1448 pixels and left 520 pixels of the window empty. `--content-max` is now `none`, so the content area reaches the right edge of the window less the padding of `main`. `.content` in `components.css` still reads the token. |
| `web/js/app.js` | 2026-11-19 | The About dialog names the author, on the owner's order of 2026-11-19: `about` shows a copyright line between the description and the licence line. |
| `web/js/app.js` | 2026-11-19 | The manual is served by the binary below `/manual/`, on the owner's order of 2026-11-19. `userMenu` holds two links above About: Documentation to `manual/` and API to `manual/api.html`. Both open a new tab, so the screen the user works on stays. Two buttons in front of the theme button made the top bar 440 pixels wide on a window of 390, and `.hide-narrow` does not hide a `.btn`, so the links sit in the menu at every width. The top bar links the manual and the API page as well, in front of the theme button inside a `span` of class `hide-narrow`, so a window of 600 pixels or less reaches them through the user menu only. |
| `web/js/pages/signin.js` | 2026-11-19 | The manual is public, and the sign in screen is the first page every user sees. Below the form, in a paragraph of class `foot`, the screen links Documentation to `manual/` in a new tab, before any sign in. The rule `.signin .foot` of `components.css` existed already, so no rule was added. |
| `web/js/pages/overview.js` | 2026-11-18 | The overview drew its cards only after every one of its requests had answered, and on a large index `GET /api/status` can take seconds at its first call after a start. `render` now draws the page head, the grid and the shortcuts at once, with a card that says Loading at the place of each card whose request still runs. `fill` puts the cards of a request in those places when it answers. A failed request leaves its places empty, and a failed list of repositories shows its notice above the cards, as before. |
| `web/js/api.js` | 2026-11-19 | The viewer reads the bytes of a file. `api.file` is one new function beside `asset`: the bytes of a download address with an optional range, a refusal as `ApiError`, and a 401 that ends the session as in every other request. The export of a folder (leg V2) adds two lines beside it: `api.exportInfo` asks `GET /api/export/<repository>` with `dry=1` for the count and the size, and `api.exportUrl` is the address of the download. No other line changed. |
| `web/index.html` | 2026-11-19 | One `<link>` line after the one of `system.css` loads `css/areas/viewer.css`, the stylesheet of the file viewer, as the four area stylesheets are loaded. No other line changed. |
| `web/js/routes.js` | 2026-11-19 | One line after the one of S05 adds the screen S46 Viewer at `/view/:repository`, module `./pages/browse/view.js`, below Browse like S05. No other line changed. |
| `web/js/pages/browse/tree.js` | 2026-11-19 | Lines inserted, none moved: an import of `showsView` and `viewHref`, the button "Open in viewer" in the page head and in the detail of a folder, and the button "View" in the detail of a file that the viewer shows as text. Leg V2 inserts an import of the export dialog, the button "Export" in the page head and in the detail of a folder, shown with the read permission of the repository and not for a Docker repository, and the button "Export" in the detail of a Docker manifest. |

Section Preview of `docs/ui/API.md` in the interface repository still says that the probe of every served read leaves the browser. With this change no probe leaves it while the server answers `GET /api/areas`, which is the alternative that decision U41 of that repository names.

## Added Files

The files in this table are new here; the taken commit does not hold them. They are the read only file viewer of Browse (`docs/plans/browse-tools.md`, leg V1) and the export dialog (leg V2). `added` in `ui_test.go` names each of them, the tree test leaves them out, and the served count grows by their number.

| File | Date | Holds |
| --- | --- | --- |
| `web/js/viewer/kinds.js` | 2026-11-19 | the kinds of files the viewer colours, by ending and content type |
| `web/js/viewer/load.js` | 2026-11-19 | the loading rules of the viewer: the whole file up to 2 MiB, one range above, a probe of 8 KiB for an unknown kind |
| `web/js/viewer/model.js` | 2026-11-19 | the model of the viewer: lines, folds, the window of rows, find and the partner of a position |
| `web/js/viewer/view.js` | 2026-11-19 | the source view: the window of rows, line numbers, colours, arrows, guides, marks and the toolbar |
| `web/css/areas/viewer.css` | 2026-11-19 | the layout of the viewer and its colours per token kind, light and dark; since leg V2 also the rows of commands of the export dialog |
| `web/js/pages/browse/export-dialog.js` | 2026-11-19 | the export dialog of Browse (leg V2): count, size, form, the command for the local use, then the download of `GET /api/export/<repository>`; the refusals 413 and 429 as notices, and the wait of 429 also when the dry answer says `busy`, asked again right before the download, with its own notice when `own_export` says an export of the same account streams; for a Docker index the choice of one stored platform |
| `web/js/viewer/filetree.js` | 2026-11-19 | the tree of the viewer, with the requests, keys and classes of the tree of S05 |
| `web/js/pages/browse/view.js` | 2026-11-19 | the screen S46: page head, tree, source or card, and the address |
| `web/js/viewer/grammar/result.js` | 2026-11-19 | the result of a scan from offsets: tokens cut at line ends, folds and pairs, shared by the grammars |
| `web/js/viewer/grammar/xml.js` | 2026-11-19 | the XML scanner: tags, attributes, values, comments, entities, declarations and CDATA marks, with folds and pairs |
| `web/js/viewer/grammar/json.js` | 2026-11-19 | the JSON scanner: keys, strings, numbers, keywords and brackets, with folds and pairs |
| `web/js/viewer/grammar/toml.js` | 2026-11-19 | the TOML scanner: tables, keys, strings, numbers, dates, booleans, comments, arrays and inline tables |
| `web/js/viewer/grammar/keyvalue.js` | 2026-11-19 | the scanner of properties, cfg, ini and conf files: comments, sections, keys, values, continuation lines and unicode escapes |
| `web/js/viewer/grammar/yaml.js` | 2026-11-19 | the YAML line scanner: comments, keys, dashes, strings, numbers, keywords, anchors, tags and block scalars, folds by indentation |
| `web/js/viewer/grammar/markdown.js` | 2026-11-19 | the Markdown line scanner for the source: headings, fenced blocks, inline code and markers, folds by heading |
| `web/js/viewer/grammar/asciidoc.js` | 2026-11-19 | the AsciiDoc line scanner for the source: titles, delimited blocks, comments, attributes and inline code, folds by section |
| `web/js/viewer/grammar/lines.js` | 2026-11-19 | folds by indentation and by heading, and inline code, for the line scanners |
