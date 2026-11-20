# Decisions

Decisions taken while the owner was away, each with the way to undo it. Started on 2026-11-12. Three things were set by the owner before and are not repeated here. The interface uses plain HTML, CSS and JavaScript without a build step or a framework. It offers a light and a dark theme with a choice on the first visit. It takes the look of the product site, since U57 its Aurora design.

## Architecture

| Id | Decision | Why | Undo |
| --- | --- | --- | --- |
| U1 | One page `index.html` with routes after the hash (`#/admin/repositories`). The server plan took it over and serves the files below `/ui/` (its decision A21) | the server needs no fallback route and no template; any static file server can serve `ui/web` | switch `ui/web/js/router.js` to the history API and let the server answer every interface path with `index.html` |
| U2 | Static files only, no template. The request token comes from the session answer. If the page carries `<meta name="pipesmith-csrf" content="...">`, `api.js` takes that one first. The server plan took over the session answer and writes nothing into the document (its decision A21) | a static page has no token until a server writes one | the server writes the meta element into `index.html` when it serves it; nothing in the interface changes |
| U3 | Where the public REST API of the old product has a path, the interface calls it, not `/api` | the task of this session says so, because the server answers in that shape anyway. The interface section of the server plan says the pages call `/api`; this is the one place where the two differ | change the paths in `ui/web/js/api.js`, the only module that names one |
| U4 | Proposed paths sit under `/api` and use the camel case of the REST shapes they stand next to | they pair with REST objects on the same screen | rename in `api.js` and `docs/ui/API.md` |
| U5 | The mock and the checks are Python with the standard library; the browser checks add Playwright through `uv run --with playwright` | they serve development and stay out of the shipped binary; the server repository already runs its proof suite in Python | in phase 6 the checks run against the server itself, and the mock can go |
| U6 | The session answer lists the expanded permission strings, and the interface tests them with the wildcard rule | the interface can hide what would fail without asking the server per button | drop the list and ask a proposed `/api/permissions/check` per screen |
| U7 | The mock marks its answers with the header `X-Pipesmith-Example-Data: 1`, and the frame shows a band "Example data" while it sees it | example data must not look live; the server never sends the header | none needed |
| U8 | No CLI launcher of its own. `python3 ui/mock/server.py` starts the mock and prints the address | this repository goes into the server repository in phase 6, which has its launcher | add one with the house launcher skill |
| U9 | Screenshots of the checks go to `out/` and are not committed | they are build output and change on every run | none needed |

## Screens

| Id | Decision | Why | Undo |
| --- | --- | --- | --- |
| U10 | Left out: database connection (capture 28) | the server keeps its state in its own files | none, there is nothing to connect |
| U11 | Left out: content selectors (captures 30, 35) | no selector is stored on the install | a list and editor on the pattern of S16 and S17 against the REST paths `/v1/security/content-selectors` |
| U12 | Privileges are read only (S19, S20); create is left out (capture 43) | the privilege table of the install is empty; the server plan builds the stored and generated kinds without an editor | an editor per type against the REST paths `/v1/security/privileges/<type>` |
| U13 | Left out: trusted certificates (captures 51, 57) | outside the 20 areas in use; the server answers the area with a refusal | a list and an add dialog against `/v1/security/ssl` |
| U14 | Left out: support archive (capture 63) | area not in use; the archive of the old product was made for its vendor's support | a page against a proposed `/api/support/archive` |
| U15 | Left out: REST explorer (capture 66) | it needs a third party library and a build | link a static description file of the server REST surface from S42 |
| U16 | Left out: create a capability (capture 69) | a new capability of another type has no effect in this server; the imported ones stay editable on S36 | a type chooser fed by `GET /api/capabilities/support` |
| U17 | Left out: mail server (capture 70) and with it the notification mail field of tasks | area not in use on the install | a form against the REST paths `/v1/email` and a field in S40 |
| U18 | Left out: outbound HTTP proxy (capture 71) | area not in use on the install | a form against a proposed `/api/settings/http` |
| U19 | Left out: notification bell (capture 84) | it showed no content | none needed |
| U20 | Left out: log targets of phase 7 | their fields are not fixed in the server plan yet | a list and editor on the pattern of S26 and S27 |
| U21 | Upload for maven2, npm and raw only | Docker images arrive through the registry protocol; NuGet holds no asset on the install | add a format to S06 when the server takes it |
| U22 | The health check of a proxy is shown as not available | plan decision P12: the server calls no outside service | none while P12 holds |
| U23 | Fields the server ignores are sent with a fixed value and not shown: `component.proprietaryComponents` false, `npm.removeQuarantined` false; `group.writableMember` is not sent | they belong to separate commercial products, and plan decision P10 refuses writes to groups | show them in S11 |
| U24 | Metrics come from proposed paths, not from `/v1/monthly-metrics` | the REST description declares no answer body for it | point `api.js` at the REST path once its shape is known |
| U25 | The permission domains `ldap`, `logging`, `atlas`, `capabilities`, `tasks`, `metrics`, and `settings` for policies and routing rules are unverified guesses | the identity notes of the product confirm only `users`, `roles`, `privileges`, `settings`, `search`, `apikey`, `userschangepw`, `component`, `blobstores` and the two repository forms | correct the one table in `ui/web/js/routes.js` |

## Behaviour

| Id | Decision | Why | Undo |
| --- | --- | --- | --- |
| U26 | Deleting a repository or a blob store needs its name typed; every other delete needs a confirmation | those two remove data nobody can rebuild | change `danger` in the delete calls of S11 and S13 |
| U27 | The LDAP bind password is asked for on every save | the server never sends it back (plan decision P9) | none while P9 holds |
| U28 | "Allow anonymous pull" for Docker is the opposite of `forceBasicAuth` | the REST shape has no field of that name; this is how the setting reads in practice | none needed |
| U29 | The interface sets 15 pixels as the body size, the site 17 | tables and forms of an administration interface are denser than a page that is read | change `--font-size` in `ui/web/css/tokens.css` |
| U30 | The theme choice is stored under the key `pipesmith-theme`, the key of the site | one key for the product family | rename in `ui/web/js/theme.js` |
| U31 | `GET /api/session` answers `200` with `user` `null` when there is no session, instead of `401` | the first request of every page would otherwise log an error in the browser console, and the checks fail on console errors | answer `401` and drop the console check for that one request |
| U32 | Below 700 pixels a table shows its rows as blocks and offers sorting through a "Sort by" list | the column heads are hidden at that width, so they cannot sort | none needed |
| U33 | The user editor route names the source: `#/admin/users/<source>/<id>` | the same user id can exist in the local source and in a directory | none needed |
| U34 | Every path inside `index.html` is relative | the server may serve the interface under `/` or under a prefix | none needed |
| U35 | Each area has a stylesheet of its own in `ui/web/css/areas/`; shared parts stay in `components.css` | four builders worked in parallel without touching one another's files | merge the area files into `components.css` |
| U36 | The mock has a third path of its own, `GET /__mock/scenario/{name}`, to show a frozen node | the checks need the read only states without a write before each screen | none needed; it never reaches the server |
| U37 | A cron expression of a task is read in UTC, and start times are entered in the time zone of the browser and sent in UTC | the contract named no time zone for cron | read cron in the zone of the server and say so in S40 |
| U38 | The token page reads the active realms; the proposal is that the server answers `GET /service/rest/v1/security/realms/active` for a session with `nexus:apikey:*` as well | the old product needs `nexus:settings:read` for it, which a user who only issues tokens does not hold | offer every kind and let `POST /api/tokens` refuse with `400` |
| U39 | The task list offers Run for every task and shows the type id | `TaskXO` has neither `enabled` nor a type name, and the list may call only the task list | add both fields to an `/api` list of tasks |
| U40 | A group shows "In members" instead of counts on the Browse page | a group holds no files of its own, and the server counts zero for it | show the sum of the members |

## Preview

Taken on 2026-11-12 for the first build of the server, after its plan listed 14 differences to this contract (F1 to F14). F6, F8 and F12 were decided for now by the session that leads the product work.

| Id | Decision | Why | Undo |
| --- | --- | --- | --- |
| U41 | After the sign in the interface sends one read per area; an area that answers `501` or `404` is marked "Not in preview" and its screens show a calm state | the first server build answers only part of the contract, and colleagues see it today | the server lists its areas in a proposed `/api/areas`, and `preview.js` reads that instead of probing |
| U42 | An action answered with `501` shows a calm notice "Not in this preview" and keeps the form | a refusal of a function that comes later is not an error of the user | show it as an error again in `ui/web/js/ui/notice.js` |
| U43 | F6: the sign in screen shows the product name; the version appears after the sign in | the status path needs a session while anonymous access is off | show the version again once the server answers the status without a session |
| U44 | F8: adding a logger stays in the contract; in the preview it shows as not available, and the server may refuse a new name with `400` | the server has a fixed set of loggers in its first build | drop the action from S30 |
| U45 | F12: no new screens now for log targets, the update history or a list of the server routes | the log targets are set by a command, the update history is a section of S34 | screens on the pattern of S26 and S34 |
| U46 | F14: fields of functions the server does not have are not offered (Docker API version 1, the trust store switch, NTLM, replication); read values go back unchanged | the server refuses a value that would switch such a function on | show the fields again in S11 and S27 |
| U47 | F9: the contract names one task log, `tasks.log`, and a file name outside the log directory answers `400` | the server keeps one task log | none needed |

## Marketing

Taken on 2026-11-12 for the images of the product website, which shows the interface with invented data.

| Id | Decision | Why | Undo |
| --- | --- | --- | --- |
| U48 | The overview shows a table of the blob stores and up to four tasks beside the counts: failed tasks first, then running ones, then the next runs | at 1440 by 900 pixels 71 percent of the light overview image and 73 percent of the dark one were one flat colour; after the change 38 and 34 percent (share of the most common colour, measured by `marketing/render.py`); both reads were already in the contract | remove `storesCard` and `tasksCard` in `ui/web/js/pages/overview.js` |
| U49 | A store that the status answers with `available` false shows "Unknown" for its files and size on the overview, as the store list does; the mock status lists the unreadable store `retired` like the store list | the server counts 0 for a store it cannot read, and 0 reads as an empty store | show the counts in `storesCard` |
| U50 | The marketing images show no band "Example data", name `https://registry.example.com` as the server address and leave one task out of the task list | the website labels the images as example data itself; without that task the list ends inside the window instead of cutting a row | change `start_mock` and `PREPARE` in `marketing/render.py` |

## Server Feedback

Taken on 2026-11-12 after a check of the interface on the first build of the server. That server serves its stores read only, answers every change with `501` and names the Maven fields of the file detail differently from the mock.

| Id | Decision | Why | Undo |
| --- | --- | --- | --- |
| U51 | While the server reports itself read only, or after it refused a change with `501`, a screen treats every permission that changes something (`add`, `create`, `edit`, `update`, `delete`, `start`, `stop`) as missing: controls for changes are hidden, forms are read only, and a screen that only makes changes shows "The server takes no changes". Navigation, landings and the opening of a screen follow the roles only | a button that only leads to a refusal looks like a working function; hiding it reuses the reader view that every screen already has and the checks already cover | let `can` in `ui/web/js/app.js` return the roles only |
| U52 | A `501` to a change counts for the whole server until the page is loaded again, not per area | the screens do not name the requests behind their controls; `GET /api/areas` (U66) names refused changes, but a control would need its request to use that | map each control to its request |
| U53 | The node page keeps freeze, release and force release by the roles, and the LDAP checks, the cleanup preview and the routing test and preview stay while the server takes no changes; their `501` does not count as a refused change | the node page is where a read only state ends; the others change nothing | treat them as changes in `api.js` and `ldap-editor.js` |
| U54 | A user without `nexus:settings:read` cannot read the read only state, so for such a user the controls disappear only after the first refused change | the contract gives the read only state to administrators only | none needed once the server answers `GET /api/areas` (U66), which carries it for every signed in user |
| U55 | The file detail reads the fields of the reference server (`group_id`, `artifact_id`, `kind`, `image`, `pull` and the others in API.md, S05); the mock answers them; an unknown field gets a label made from its name; the property keys of a capability get such a label and the key in the help line | the contract named the three objects but not their fields, and the mock had invented other names | none needed; the labels live in `tree.js` |
| U56 | The mock in preview mode starts read only, frozen by the system, as the server does, and the node screen probes its own read `system/node` instead of the read only state | the server answers the read only state and refuses the node read; the old probe marked the node screen as offered | probe `read-only` again in `preview.js` |

## Aurora

Taken on 2026-11-12. The owner asked, through the session of the product site, for the new symbol with its aurora gradient and for a little of the Aurora design of the iogentic harness. In the owner's words the interface is an app and should not distract.

| Id | Decision | Why | Undo |
| --- | --- | --- | --- |
| U57 | The colours are those of the Aurora design of the product sites, light and dark. The dark `--primary` is `#4D8DFF` instead of `#3D7BFF`, as on the website. Success and warning take the Aurora values, danger stays, the focus ring keeps the accent | `#3D7BFF` reaches 4.37 on `--surface-2` and the check needs 4.5; with `#4D8DFF` all 74 pairs pass, the lowest at 3.10 for the border of a control (limit 3) | `ui/web/css/tokens.css` of commit `44bf552` |
| U58 | Aurora shows only as decoration: a line of two pixels under the top bar, a gradient marker on the current navigation entry, the gradient fill of the share bars, and three faint, still glows behind the sign in. Copper and patina give way to the tokens `--aurora`, `--aurora-2` and `--aurora-3` | a calm app; no moving background, and no text in a gradient, because such text cannot be checked for contrast | remove `.top::after`, the marker and the background of `.signin` in `components.css` |
| U59 | The fonts stay the system stack; the fonts of the iogentic sites are not taken | they would add font files and their licences to the package; the look of the harness comes mostly from its colours | font files under `ui/web/fonts` and `--font` in `tokens.css` |
| U60 | The symbol, the favicon and the touch icon come from commit `96a73de` of the product site, without their `eXIf` chunk; the pixels are the same | `check_images.py` allows image chunks only | the three files of commit `44bf552` |
| U61 | The interface does not name iogentic | the request named iogentic as the brand on the website; for the interface it asked for the look only | add the line to the sign in page |

## Demo Findings

Taken on 2026-11-12, after the orchestrating session went through the interface in the demo instance of the server. The demo instance shows paths of a real installation, so none of its screens goes into `marketing/`.

| Id | Decision | Why | Undo |
| --- | --- | --- | --- |
| U62 | A fact of a file the server did not record reads "Not recorded": uploader, address, last change, last download, blob time, size, content type, blob store. "Last downloaded" says "Not recorded" too, no longer "Never" | "None" read like a value that slipped through; the server holds no uploader, address, last change or last download, so "Never" claimed something it does not know | `facts(rows)` without `empty` in `tree.js` |
| U63 | Without a checksum the section Checksums keeps its heading and says "Not recorded" | a heading over an empty area looked broken; the line says the server was asked and holds none | leave the section out when `checksums` is empty |
| U64 | The reason of the read only state shows once per screen, in the band of the frame. The overview no longer shows its own notice and no longer reads the state. The read only notice of a form says "The server takes no changes at the moment, so this page shows what is stored." The node page, which shows the state with its reason and the release, shows no band | the new check for one text in two kinds of place found the sentence of the band a second time on the overview, on S11, S13, S22, S24 and S41, and on S37 | the notice and the read in `overview.js`, and `readOnlyText` in `app.js`, of commit `f0c7dfb`; `nodeShowsState` in `bands()` |
| U65 | `format.js` names the 20 formats the server can hold, one per format table of the database it reads: APT, Cargo, CocoaPods, Composer, Conan, Conda, Docker, Git LFS, Go, Helm, Hugging Face, Maven, npm, NuGet, p2, PyPI, R, Raw, RubyGems, Yum. An id outside the list shows with a capital first letter | the overview showed "nuget"; the names follow the public format list of the old product, read on 2026-11-10, with "Rust / Cargo" as Cargo and "Composer (PHP)" as Composer | the four names of commit `f0c7dfb` |
| U66 | `GET /api/areas` names the requests the server refuses and its read only state (API.md, section Areas). The interface asks once after the sign in, answers a refused request itself as a `501`, and sends nothing. Without the answer it probes as before | the browser logs every failed request as an error; the demo walk through five screens left 13 such lines (count of the orchestrating session). The answer follows the refusal table the server already has | drop `loadAreas` in `app.js` and `setRefused` in `api.js` |
| U67 | S33 does not ask `status/writable` while the interface knows the server is frozen | the server then answers `503`, a red line in the console that says nothing new | ask it in every case in `status.js` |
| U68 | The preview checks allow no failed request in general; a screen or flow that expects one says so with `allow` | the general allowance of `404` and `501` also hid the console lines of U66 | the allowance in `check_screens` and `check_flows` of `run_checks.py` |

