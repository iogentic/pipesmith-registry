# Administration Interface

This is the administration interface of Pipesmith Registry, written in plain HTML, CSS and JavaScript modules. It needs neither a build step nor a framework. It talks to the server through one client module, and until the server answers the contract it runs against a mock with invented data.

## Layout

| Path | What it holds |
| --- | --- |
| `ui/web/` | the files the server serves: `index.html`, `css/`, `js/`, `assets/` |
| `ui/web/js/api.js` | the only module that names a server path |
| `ui/web/js/routes.js` | every screen with its route, permission and module |
| `ui/web/js/ui/` | the shared parts: table, form, dialog, notices, page states |
| `ui/web/js/pages/` | one module per screen |
| `ui/mock/` | the mock of the contract: `server.py`, one module per area in `areas/`, invented data in `fixtures/` |
| `ui/checks/` | the checks: one command runs all of them |
| `marketing/` | screenshots for the website with invented data, `manifest.json` and the script that renders them |
| `docs/ui/INVENTORY.md` | the 83 functions of the old interface and what this interface does with each |
| `docs/ui/API.md` | the contract the server implements |
| `docs/ui/DECISIONS.md` | decisions taken while the owner was away, each with its undo |

## Run

The mock needs Python and nothing else; it was run with Python 3.15 on 2026-11-12.

```bash
python3 ui/mock/server.py
```

It serves the interface on `http://localhost:18900/` and binds to localhost only. `--port` takes a port from 18900 to 18949, `--delay` slows every answer down by that many milliseconds.

Users of the mock. The passwords exist only in `ui/mock/fixtures/security.json` and in the checks.

| User | Password | Can |
| --- | --- | --- |
| `admin` | `example-admin-1` | everything |
| `viewer` | `example-viewer-1` | browse, read and search |
| `ci-builder` | `example-builder-1` | read, upload, issue tokens |
| `maven-lead` | `example-maven-1` | administer the Maven repositories, read tasks |
| `dir.platform` | `example-directory-1` | a directory user with the rights of a mapped group |

Every answer of the mock carries `X-Pipesmith-Example-Data: 1`. While the interface sees it, every screen shows a band "Example data".

## Checks

```bash
uv run --no-project --with playwright python3 ui/checks/run_checks.py
```

One command, in this order:

1. `check_text.py` looks for em and en dashes, emoji, absolute machine paths and the name of the old product. It also looks for the words of the local deny list `.denylist`, which git ignores.
2. `check_docs.py`: 83 inventory rows with a status, every build row on a screen, every screen in the contract and in `routes.js`, every left out row in the decisions, every area in use on a screen.
3. `check_images.py`: the PNG files hold image chunks only. Each marketing image is at most 600 KiB and 2880 by 1800 pixels, and `marketing/manifest.json` lists exactly the images of the folder, each with a title, a caption and an alt text.
4. `check_contrast.py`: every text colour of the tokens against every ground, both themes, WCAG 2 formula, limit 4.5; the border of a control against its grounds, limit 3.
5. `check_dom.py`: no native DOM call such as `append` or `replaceChildren` gets a value that may be `null`, because the browser would show it as the word "null".
6. `check_formats.py`: each of the 20 formats the server can hold has a name of its own in `format.js`, so the interface never shows an id such as "nuget".
7. The runner starts a mock on its own port (default 18901) and refuses a port that another process holds. It opens every screen of `ui/checks/screens/*.json` in light and dark at 1280 and 390 pixels. A screen fails on a console error, a failed answer or an element outside the window. It also fails on a container that scrolls sideways, on text below the contrast limit as measured in the page, on a control that another element covers, on content cut off by its frame, and on a raw value such as "null" or "undefined" in the text. It fails as well on a field name shown as a label, a missing file fact shown as "None", a heading over an empty section, and one sentence in two kinds of place. A full page screenshot of each goes to `out/screens/`.
8. The flows of `ui/checks/flows/*.py`, each on a fresh mock state, with clicks at the coordinates of the element after a check that nothing covers it.
9. The preview run: a second mock with `--preview` on the next port plays the first server build (see `docs/ui/API.md`, section Preview). The screens and flows of `ui/checks/preview/` check that areas the server offers work, that the others show "Not in this preview" and carry the mark in the navigation, that no error notice and no example data band appears, and that a refused change stays calm. No request may fail there unless a check says so, so a walk through the preview leaves no error line in the console. One flow plays a server without `GET /api/areas`.

The report goes to `out/report.md` and `out/report.json`. `--only <area>` runs one manifest (`core`, `browse`, `security`, `storage`, `system`, `preview`), `--port` picks the port (the preview run takes the next one), `--no-screens`, `--no-flows` and `--no-preview` skip a part, and `--base <address>` checks the screens against a server that already runs.

```bash
uv run --no-project --with playwright python3 ui/checks/run_checks.py --self-test --port 18905
```

The self test breaks every guard once on purpose and fails when one of them stays quiet.

## Screens

| Id | Screen | Route |
| --- | --- | --- |
| S01 | Sign in | `#/sign-in` |
| S02 | Overview | `#/overview` |
| S03 | Search | `#/search` |
| S04 | Browse | `#/browse` |
| S05 | Repository tree and file detail | `#/browse/<repository>` |
| S06 | Upload | `#/upload` |
| S07 | Upload form | `#/upload/<repository>` |
| S08 | Section landing | `#/admin/repository`, `#/admin/security`, `#/admin/system`, `#/admin/support` |
| S09 | Repositories | `#/admin/repositories` |
| S10 | New repository | `#/admin/repositories/new` |
| S11 | Repository editor | `#/admin/repositories/new/<format>/<type>`, `#/admin/repositories/<name>` |
| S12 | Blob stores | `#/admin/blob-stores` |
| S13 | Blob store editor | `#/admin/blob-stores/new`, `#/admin/blob-stores/<name>` |
| S14 | Cleanup policies | `#/admin/cleanup-policies` |
| S15 | Cleanup policy editor | `#/admin/cleanup-policies/new`, `#/admin/cleanup-policies/<name>` |
| S16 | Routing rules | `#/admin/routing-rules` |
| S17 | Routing rule editor | `#/admin/routing-rules/new`, `#/admin/routing-rules/<name>` |
| S18 | Routing preview | `#/admin/routing-preview` |
| S19 | Privileges | `#/admin/privileges` |
| S20 | Privilege detail | `#/admin/privileges/<name>` |
| S21 | Roles | `#/admin/roles` |
| S22 | Role editor | `#/admin/roles/new`, `#/admin/roles/<id>` |
| S23 | Users | `#/admin/users` |
| S24 | User editor | `#/admin/users/new`, `#/admin/users/<source>/<id>` |
| S25 | Anonymous access | `#/admin/anonymous` |
| S26 | LDAP connections | `#/admin/ldap` |
| S27 | LDAP editor | `#/admin/ldap/new`, `#/admin/ldap/<name>` |
| S28 | Realms | `#/admin/realms` |
| S29 | Tokens | `#/tokens` |
| S30 | Logging | `#/admin/logging` |
| S31 | Log files | `#/admin/logs` |
| S32 | Log viewer | `#/admin/logs/<file>` |
| S33 | Status | `#/admin/status` |
| S34 | System information | `#/admin/system-information` |
| S35 | Capabilities | `#/admin/capabilities` |
| S36 | Capability editor | `#/admin/capabilities/<id>` |
| S37 | Node | `#/admin/node` |
| S38 | Tasks | `#/admin/tasks` |
| S39 | New task | `#/admin/tasks/new` |
| S40 | Task editor | `#/admin/tasks/new/<type>`, `#/admin/tasks/<id>` |
| S41 | Account | `#/account` |
| S42 | About | dialog from the user menu |
| S43 | Metrics | `#/admin/metrics` |
| S44 | No access | in place of a screen the session may not open |
| S45 | Not found | for an unknown route or a missing object |

## Serving

The interface is a set of static files. Its routes sit after the hash in the address, so the server only has to serve files. It needs neither a fallback route nor a template.

- Embed `ui/web` in the binary with `embed`. The server plan serves it below `/ui/` and answers the entry address with a redirect there (its decision A21). Every path inside `index.html` is relative, so any prefix works, as long as the page is served at the prefix with a trailing slash.
- Content types: `.js` as `text/javascript`, `.css` as `text/css`, `.html` as `text/html`, `.png` as `image/png`, each with `charset=utf-8` where it is text.
- Headers on the page and its files, as the mock sends them:

```
Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'
X-Content-Type-Options: nosniff
Referrer-Policy: same-origin
```

  The files hold no inline script and no style attribute; the checks fail on a refusal of this policy.
- A sketch of the embedding, for the port to adapt:

```go
package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web
var files embed.FS

// Handler serves the administration interface.
func Handler() http.Handler {
	web, err := fs.Sub(files, "web")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(web))
}
```

## Phase 6

What the port session does to wire the interface, in this order.

1. Move `ui/` and `docs/ui/` into the server repository. `ui/mock/` and `ui/checks/` stay as development tools; `ui/web/` is embedded.
2. Serve `ui/web` as described under Serving, in place of the page of port step 2.
3. Implement the session of `docs/ui/API.md`, section Session Endpoints: the cookie, the request token in `X-Pipesmith-Csrf` on every namespace, the expanded permissions in the session answer, and no Basic challenge on a `401` to a request with the session cookie.
4. Implement the proposed paths, listed at the end of `docs/ui/API.md`. Where a path or a field changes, change `ui/web/js/api.js` and `docs/ui/API.md` together.
5. Answer the REST paths the interface calls in the shape of the old product. The validation answer is the list `[{"id", "message"}]` with `id` as the dot path of the field.
6. Check the permission domains that `docs/ui/API.md` marks unverified against the route table of the server, and correct `ui/web/js/routes.js`.
7. Run the checks against the server instead of the mock: give the server the fixtures of `ui/mock/fixtures/` as test data, start it, and run `run_checks.py --base http://localhost:<port>`. That checks every screen; the flows need a reset of the data between flows, so `--base` skips them until the server has a test reset of its own.
8. Decide the open points of `docs/ui/DECISIONS.md`, starting with U3. The server plan wants the pages to call `/api`; this interface calls the REST paths of the old product where they exist.

## Check Record

The latest full run was on 2026-11-12 from 18:35 to 18:49 on the code of commit `3636176`, with `run_checks.py --port 18910`. It had 763 checks, and all 763 passed in 828 seconds: 6 static checks, 540 screen checks, 69 flows, and the preview run with 140 screen checks and 8 flows. The preview run now fails on every failed request. On the interface of commit `f0c7dfb` it failed 145 of its 154 checks, with 14 failed probes on each page load. The self test broke 23 guards on purpose, and all 23 fired.

The run before was on 2026-11-12 from 14:43 to 14:53 on the code of commit `2b2c006`, the Aurora look, with `run_checks.py --port 18901`. It had 756 checks, and all 756 passed in 592 seconds. The self test broke 19 guards on purpose, and all 19 fired; the new one changes a dark value of the system setting in `tokens.css` alone. `check_contrast.py` measured 74 pairs, the lowest at 3.10 for the border of a control against its limit of 3.

Before that, a run on 2026-11-12 from 13:50 to 13:59 checked the tree of commit `baad3e1`, with `run_checks.py --port 18901`. It had 756 checks, and all 756 passed in 545 seconds: 5 static checks, 536 screen checks, 69 flows, and the preview run with 140 screen checks and 6 flows. A run at 13:33 on the same code had one failure: a page load of S30 timed out while the self test ran beside it. The system area alone then passed 176 of 176. The self test broke 18 guards on purpose, and all 18 fired.

Three mutations of the code of `baad3e1` turned their checks red. `can` in `app.js` without the read only state left 21 controls for changes in the preview and 109 on 91 frozen screens. Without the `writesRefused` event the repository list still offered "Create repository" after a refused save. `fieldLabel` returning the raw name showed `address`, `colour`, `interval` and `text` as labels on 12 capability screens.

The run before that was on 2026-11-12 from 09:49 to 10:02 on commit `68e9fd2`: 754 checks, all passed in 795 seconds, and 17 of 17 guards of the self test fired.

Before the preview and the marketing images, the checks ran three times on 2026-11-12 between 08:15 and 08:47 on commit `314b6d0`. The command was `run_checks.py --port 18901` against the mock, in Chrome through Playwright. Each run had 608 checks and all 608 passed; the runs took 644, 633 and 624 seconds. The 608 checks are 5 static checks, 536 screen checks (134 screen states, each in two themes at two widths) and 67 flows.

Three runs before them, between 07:43 and 08:14, passed 603 of 607 checks each. The four failures were the same in all three: the node page showed the word "null" to a user who may only read it. Commit `314b6d0` fixes the page and adds `check_dom.py`, which finds that class of mistake in the code.

Other evidence, each taken on the same day:

- The self test broke 16 guards on purpose, and all 16 fired.
- After those runs one string of the example data changed (commit `80f3807`, realm names instead of realm ids). The system screens ran again on it: 157 of 157.
- Before its pages existed, each area ran its checks once and failed them. Browse passed 8 of 145, security 4 of 131, storage 8 of 94 and system 8 of 172. The checks that passed were static ones and no access screens, which the frame draws without a page module.
- Each area broke one or two behaviours on purpose and saw the matching flow fail. Browse broke the delete call and the upload classifier, security the role save and the LDAP password prompt, storage the quota unit and the cleanup preview, and system the task run and the week days of a schedule.
- Every one of the 45 screens was looked at in the screenshots of the light theme at 1280 pixels and the dark theme at 390 pixels. For 21 screen states all four variants were looked at, through the contact sheets of `ui/checks/contact_sheet.py`.

The checks do not cover the real server, browsers other than Chrome, touch devices (390 pixels is a window width, not a phone) or a screen reader. Roles, labels and focus order are checked in the code and by keyboard flows, not with assistive technology.

## Marketing

```bash
uv run --no-project --with playwright --with pillow python3 marketing/render.py
```

The script starts the mock on port 18940. It leaves out the example data header and names `https://registry.example.com` as the server address. It signs in as `admin` and takes nine views in light and dark: overview, repository list, an npm proxy, the tree with a file, a Maven search, a user with its roles, an LDAP connection, tasks and blob stores. Each image is 1440 by 900 pixels at a device scale factor of 2, so 2880 by 1800, in English, with animations stopped. The search and the LDAP view are moved down until no edge of the window cuts a line. For each image the script reports any line the bottom edge still cuts and the share of the image in one flat colour. It writes `marketing/manifest.json` with file, theme, title, caption and alt text. An image over 600 KiB would be reduced to 256 colours; none needed it on 2026-11-12, when the 18 images were 256 to 500 KB.

Seven runs on 2026-11-12, compared by SHA-256: runs 2 to 7 gave the same bytes for all 18 images. In run 1 one image, `user-roles-dark.png`, differed; the file was replaced before its pixels were compared, and the difference did not come back in six runs. A render at 13:15 the same day changed one value in the overview and in the task list: the time of the next run. The mock computes it from its clock, so these four images follow the hour of the render. The images in the Aurora look were rendered at 14:53, all 18 at once.

## Preview

Until the server answers the whole contract, the interface probes each area once after the sign in and marks what the server does not offer yet. While the server reports itself read only, or after it refused a change with `501`, no screen offers a control for a change. `docs/ui/API.md`, section Preview, says how; `python3 ui/mock/server.py --preview` shows it with the mock. Section Areas of the same file names one answer of the server, `GET /api/areas`, with which the interface sends no request the server refuses. The interface and the mock have it; the server does not yet.

## Open

- The 12 left out rows of the inventory, each with its reason and undo in `DECISIONS.md`.
- The log targets of phase 7 have no screen; the server sets them with a command (decision U45).
- The permission domains marked unverified in `API.md` wait for the route table of the server.
- The checks have run against the mock only. Phase 6 runs them against the server.

## Decisions

`docs/ui/DECISIONS.md` lists every decision taken while the owner was away, with the way to undo it.
