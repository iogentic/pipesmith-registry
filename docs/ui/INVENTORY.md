# Inventory

What the administration interface of the old product does, one row per capture, and what this interface does with it. Written on 2026-11-12.

## Method

- Source: 83 captures of the old interface, taken on an install by a signed in administrator, and the notes written beside them. The captures are numbered 01 to 84; number 56 is not used.
- Each row describes the function of a capture in our own words. No text, layout, icon or name of the old interface is taken over, and no host, repository or person of the install is named.
- Each row has one status:
  - **build**: a screen of this interface covers the function. The screen id points to the list under Screens.
  - **left out**: not built now, with the reason. Every left out row is a decision of `DECISIONS.md` and can be added later.
  - **outside**: outside the promise of the product: plugins and separate commercial products of the old vendor, clustering, formats the install does not use, and the old interface as such.
- Counted by hand from the table below and again by `ui/checks/check_docs.py`, which reads this file: 83 rows, 65 build, 12 left out, 6 outside.

## Frame

| Capture | Function | Status | Screen or reason |
| --- | --- | --- | --- |
| 01 | Sign in with user name and password | build | S01 |
| 16 | Landing page of the repository section with one card per entry | build | S08 |
| 38 | Landing page of the security section | build | S08 |
| 58 | Landing page of the support section | build | S08 |
| 65 | Landing page of the system section | build | S08 |
| 81 | User menu: own account, own keys, sign out | build | S41, S29, sign out in the frame |
| 82 | Own account: names, mail, change of the own password | build | S41 |
| 83 | Help menu: version and links to vendor resources | build | S42, version and product links only |
| 84 | Notification bell in the top bar | left out | showed no content on the install; no function to rebuild |

## Browse and Search

| Capture | Function | Status | Screen or reason |
| --- | --- | --- | --- |
| 02 | Landing page with usage counters and links to health, policies and browsing | build | S02 with counts of this server; the usage limits and the purchase offer of the old vendor are not part of it |
| 03 | Component search across repositories by keyword | build | S03 |
| 04 | More search criteria: format, group, name, version, checksum, repository | build | S03 |
| 05 | Search without format fields | build | S03, keyword and criteria |
| 06 | Search with the Docker fields: image name, tag, layer, digest | build | S03, format docker |
| 07 | Search with the Maven fields: group, artifact, version, base version, classifier, extension | build | S03, format maven2 |
| 08 | Search with the npm fields: scope, name, version, author, description, keywords, licence | build | S03, format npm |
| 09 | Search with the NuGet fields | outside | NuGet repositories on the install hold no asset |
| 10 | Search with the raw field: name | build | S03, format raw |
| 11 | List of all repositories to browse, with type, format, state and address | build | S04 |
| 12 | Tree of an empty repository | build | S05, empty state |
| 13 | Folder tree of one repository with checksum files beside each file | build | S05 |
| 14 | Detail of one file: path, size, type, blob, times, uploader, checksums; delete | build | S05, detail pane |
| 15 | List of hosted repositories that take uploads, then an upload form per repository | build | S06, S07 for maven2, npm and raw |

## Repositories

| Capture | Function | Status | Screen or reason |
| --- | --- | --- | --- |
| 17 | List of repositories with type, format, blob store, state and address; create | build | S09 |
| 18 | Settings of a hosted Maven repository: version, layout and redeploy policy, store, cleanup | build | S11, maven2 hosted |
| 19 | Settings of a Maven proxy: remote, cache ages, blocking, negative cache, routing rule, HTTP auth | build | S11, maven2 proxy |
| 20 | Settings of a Maven group: ordered members | build | S11, maven2 group |
| 21 | Settings of a hosted Docker repository: connector port, anonymous pull, redeploy of latest | build | S11, docker hosted |
| 22 | Settings of a Docker proxy: index, foreign layers, connector port, proxy settings | build | S11, docker proxy |
| 23 | First step of create: pick format and type | build | S10, for maven2, npm, docker and raw |
| 24 | Create form of a Maven proxy | build | S11, create mode |
| 28 | Connection settings of the database | left out | the server keeps its state in its own files; there is no connection to set |
| 29 | Marking hosted repositories as proprietary for a supply chain product | outside | feature of a separate commercial product |
| 30 | List of content selectors | left out | area not in use on the install (no selector stored) |
| 32 | List of routing rules with the count of repositories that use each | build | S16 |
| 34 | Edit a routing rule: mode, matchers, which repositories use it, path test | build | S17 |
| 35 | Create a content selector with an expression and a preview | left out | area not in use on the install |
| 36 | Create a routing rule | build | S17, create mode |
| 37 | Test a path against the routing rules of many repositories at once | build | S18 |

## Blob Stores

| Capture | Function | Status | Screen or reason |
| --- | --- | --- | --- |
| 25 | List of blob stores with path, type, state, blob count, used and free space | build | S12 |
| 26 | Edit a file blob store: path and soft quota; delete | build | S13 |
| 27 | Create a blob store: choose a type, then its fields | build | S13, type file; the cloud types are not used on the install |

## Cleanup Policies

| Capture | Function | Status | Screen or reason |
| --- | --- | --- | --- |
| 31 | List of cleanup policies with format and description | build | S14 |
| 33 | Edit a cleanup policy: format, age and usage criteria, preview of what it would remove | build | S15 |

## Security

| Capture | Function | Status | Screen or reason |
| --- | --- | --- | --- |
| 39 | List of privileges with type and permission | build | S19, read only |
| 40 | List of roles | build | S21 |
| 41 | List of users per source, with realm and state; create a local user | build | S23 |
| 42 | Detail of a privilege; the built in ones are read only | build | S20, read only |
| 43 | Create a privilege of a chosen type | left out | no stored privilege on the install; roles use the built in and generated ones |
| 44 | Edit a role: applied privileges and contained roles | build | S22 |
| 45 | Create a role or map an external group to a role | build | S22, create mode, both kinds |
| 46 | Edit a user: names, mail, state, roles; change password; delete | build | S24 |
| 47 | Create a local user with password and roles | build | S24, create mode |
| 48 | Anonymous access: on or off, the user and realm it acts as | build | S25 |
| 49 | List of LDAP connections in their order; change order; clear cache | build | S26 |
| 50 | Realms: which are active and in what order | build | S28 |
| 51 | List of trusted certificates | left out | outside the 20 areas in use; the server answers the area with a refusal |
| 52 | LDAP connection: protocol, host, port, base, bind method and account, timeouts; verify | build | S27, tab Connection |
| 53 | LDAP user and group mapping; verify mapping and a sign in | build | S27, tab Users and Groups |
| 54 | Create an LDAP connection | build | S27, create mode |
| 55 | Dialog to reorder the LDAP connections | build | S26, order dialog |
| 57 | Add a trusted certificate from a server or as pasted text | left out | as 51 |

## Tasks

| Capture | Function | Status | Screen or reason |
| --- | --- | --- | --- |
| 77 | List of tasks with type, state, schedule, next and last run, last result | build | S38 |
| 78 | Detail of a task: summary and settings; run, stop, delete | build | S40 |
| 79 | First step of create: pick a task type | build | S39, the types the server offers |
| 80 | Create form of a task: name, enabled, type fields, schedule | build | S40, create mode |

## Logging

| Capture | Function | Status | Screen or reason |
| --- | --- | --- | --- |
| 59 | Logger names with their level; add a logger; reset to defaults | build | S30 |
| 60 | List of log files with size and time | build | S31 |
| 61 | Tail of one log file with refresh rate and size; download | build | S32 |

## Status and Support

| Capture | Function | Status | Screen or reason |
| --- | --- | --- | --- |
| 62 | Health checks of the node with message and state | build | S33 |
| 63 | Build a support archive from chosen parts | left out | area not in use on the install; the archive was made for the vendor's support |
| 64 | System information as key and value sections; download as JSON | build | S34 |

## System

| Capture | Function | Status | Screen or reason |
| --- | --- | --- | --- |
| 66 | Embedded explorer of the REST interface | left out | needs a third party explorer library, against the decision for no build step and no framework |
| 67 | List of capabilities with state, type and notes | build | S35 |
| 68 | Detail of a capability: summary, settings, notes; enable, disable, delete | build | S36 |
| 69 | Create a capability of a chosen type | left out | a new capability of another type has no effect in this server; the imported ones stay editable |
| 70 | Mail server for notifications, with a test mail | left out | area not in use on the install |
| 71 | Outbound HTTP proxy and timeouts | left out | area not in use on the install (no setting stored) |
| 72 | Licence of the old product: upload | outside | licence of the old vendor |
| 73 | Node: identity and read only mode | build | S37 |
| 74 | Migration wizard from an earlier generation of the old product | outside | the install is already on the generation this product replaces |
| 75 | Connection to a separate policy server of the old vendor | outside | feature of a separate commercial product |
| 76 | Historical usage per month under the licence | outside | licence of the old vendor; this server's own counters are on S43 |

## Areas

The 20 areas in use on the install, from the feature inventory of the product. Every area has at least one screen.

| Area | Screens |
| --- | --- |
| Content serving | S04 and S09 show the address of each repository; S11 sets the Docker port |
| Browse | S04, S05 |
| Search | S03 |
| Components and assets | S05 (detail, delete), S06, S07 (upload) |
| Repositories | S09, S10, S11 |
| Blob stores | S12, S13 |
| Cleanup policies | S14, S15 |
| Tasks | S38, S39, S40 |
| Routing rules | S16, S17, S18 |
| Users | S23, S24 |
| Roles | S21, S22 |
| Realms | S28 |
| LDAP | S26, S27 |
| Keys and tokens | S29 (own tokens), S24 (tokens of a user) |
| Capabilities | S35, S36 |
| Status and metrics | S02, S33, S43 |
| Download counts | S43 |
| Nodes and lifecycle | S37, S34 |
| Upgrade and schema | S34 (version and schema of the state files), S42 |
| Firewall and health check | S11 shows the health check of a proxy as not available; S33 lists it |

Three areas outside the 20 still have a screen because the server touches them: anonymous access (S25), privileges (S19, S20, read only) and read only mode (S37).

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
| S08 | Section landing | `#/admin/repository`, `#/admin/security`, `#/admin/support`, `#/admin/system` |
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
| S42 | About | dialog from the top bar |
| S43 | Metrics | `#/admin/metrics` |
| S44 | No access | shown in place of any screen the user may not open |
| S45 | Not found | shown for an unknown route or a missing object |

S29, S43, S44 and S45 have no capture. S29 and S43 cover areas in use that the captures did not show; S44 and S45 are states every screen needs.

## Beyond the Captures

- Log targets of phase 7 (syslog, GELF, Loki and the others): no screen. The server plan sets them with a command of the server in phase 7. The update history of phase 8 appears as a section of the system information on S34.
- Compaction, rescans and the deletion queue: the blob store editor S13 shows the count of blobs marked for deletion, and compaction and rescans run as tasks on S38 to S40.
