# API Contract

The requests the administration interface sends, screen by screen. The port session implements the server side against this file; the mock in `ui/mock/` implements it with invented data. Written on 2026-11-12.

## Kinds

Every request carries one of three marks.

| Mark | Meaning |
| --- | --- |
| REST | The public REST API of the old product has this path. The server answers in that shape, so the interface uses the path and the fields unchanged. Base `/service/rest`. |
| API | The server has this path in its own namespace since port step 2. The interface uses it as it is. |
| Proposed | Neither has a path for the function. This file proposes one under `/api`. The port session may change it; the change then goes into this file and into `ui/web/js/api.js`. |

`ui/web/js/api.js` is the only module that names a path. A change of a path is a change of that one file.

The screen ids S01 to S45 are the interface's own. The plans of the server number the captures of the old interface S01 to S83 and write the interface ids as `ui:S01`.

## Conventions

### Transport

- Same origin as the page. The interface never calls another host.
- Request bodies are JSON with `Content-Type: application/json`, except the two marked otherwise: the password change of another user (`text/plain`) and the upload (`multipart/form-data`).
- Answers are JSON unless a row says otherwise. Times in REST answers are ISO 8601 strings in UTC. Times in API answers keep the form of the port, milliseconds since 1970 in fields ending in `_ms`.
- Field names of proposed requests use the camel case of the REST shapes they stand next to.

### Session

- The interface signs in with `POST /api/session` and from then on sends the session cookie the server set. It never sends Basic authentication and never stores a password.
- The cookie is `HttpOnly`, `SameSite=Strict`, `Path=/`, and `Secure` when the request came over TLS.
- Every `POST`, `PUT` and `DELETE` carries the header `X-Pipesmith-Csrf` with the token of the session answer, on every namespace, the REST namespace included. Without it, or with a wrong one, the server answers `403` with `{"message": "missing or wrong request token"}` and changes nothing.
- A `401` to a request that carries the session cookie has no `WWW-Authenticate: Basic` header. Otherwise the browser would open a sign in dialog of its own over the interface.

### Permissions

- The session answer lists the permission strings the user holds, expanded from all roles and in the form of the old product (`nexus:<domain>:<action>`, `nexus:repository-view:<format>:<repository>:<action>`, `nexus:repository-admin:<format>:<repository>:<action>`).
- The interface tests them with the wildcard rule of the old product: parts split by `:`, alternatives by `,`, `*` matches any value, missing trailing parts count as `*`. A `*` in the permission of a screen means any value: the screen opens when the user holds the permission for at least one repository or object, and the server answers with what that user may see.
- The interface hides navigation entries and actions the user does not hold, and shows S44 for a screen the user may not open. The server enforces every right on its own; the interface only avoids offering what would fail.
- The permission per screen is listed under Screens. Domains marked unverified are the closest match the sources allow; the server's route table decides, and `ui/web/js/routes.js` holds the one copy in the interface.

### Errors

| Status | Body | What the interface does |
| --- | --- | --- |
| 400 | `[{"id": "<field>", "message": "<text>"}]` | Shows each message beside the field whose path equals `id` (dot path into the request body, for example `storage.blobStoreName`). An `id` of `*` or one that names no field goes into a notice above the form. |
| 401 | any | Opens S01 and returns to the same route after sign in. |
| 403 | `{"message": "<text>"}` or empty | Shows S44 for a screen, a notice for an action. |
| 404 | `{"message": "<text>"}` or empty | Shows S45 for a screen, a notice for an action. |
| 409 | `{"message": "<text>"}` | Shows the message in a notice. Used for a name that exists, a store or policy still in use, a task that is not running. |
| 429 | empty, header `Retry-After` in seconds | Sign in only: says when to try again. |
| 501 | `{"message": "<text>", "area": "<area>"}` | Shows that the function is not available in this server. This is the refusal of plan decision P3. A `501` to a change also hides the controls for changes until the page is loaded again (section Preview). A request that `GET /api/areas` names as refused gets this answer from the interface itself and is not sent (section Areas). |
| 503 | `{"message": "<text>"}` or empty | Shows that the server is read only or not ready. |
| other | any | Shows the status number and the message if there is one. |

The list form of the 400 body is an assumption: the REST description of the old product declares no error body, so the form is unverified. The interface also accepts `{"message": "<text>"}` for any status.

### Paging

- REST lists with a `continuationToken` (search, assets, components, tasks) are read page by page. The interface shows a button to load the next page while the answer carries a token.
- Every other list is small enough to come whole. The interface sorts, filters and pages it in the browser.

## Session Endpoints

All Proposed.

| Request | Body | Answer |
| --- | --- | --- |
| `POST /api/session` | `{"username": string, "password": string}` | `200` Session, sets the cookie. `401` `{"message"}` for a wrong pair, without saying which half is wrong. `429` while sign in is slowed down. |
| `GET /api/session` | | `200` Session for a valid cookie. Without one also `200`, with `user` and `csrfToken` `null` and `permissions` empty, so that the first request of the page does not fail. |
| `DELETE /api/session` | | `204`, ends the session and clears the cookie. |

Session:

```json
{
  "user": {
    "userId": "string",
    "firstName": "string",
    "lastName": "string",
    "emailAddress": "string",
    "source": "default | LDAP",
    "realm": "string"
  },
  "permissions": ["nexus:*"],
  "csrfToken": "string",
  "expiresInSeconds": 1800
}
```

`expiresInSeconds` is the idle time after which the session ends. The interface does not count it down; it reacts to the `401` that follows.

## Screens

Every screen with the status build in `INVENTORY.md`, with its permission and its requests. A row without a mark in the Kind column continues the row above.

### S01 Sign In

Permission: none.

| Kind | Request | Use |
| --- | --- | --- |
| Proposed | `POST /api/session` | sign in |

The screen shows the product name only. `GET /api/status` needs a session once anonymous access is off, so the version appears after the sign in, on S02 and S42.

### S02 Overview

Permission: a session.

| Kind | Request | Use |
| --- | --- | --- |
| API | `GET /api/status` | version, totals, scan state, and per store `files`, `bytes` and `available`; a store with `available` false shows "Unknown" instead of its counts |
| REST | `GET /service/rest/v1/repositories` | count of repositories by type and format |
| REST | `GET /service/rest/v1/status/check` | count of failing health checks; shown only with `nexus:atlas:read` |
| REST | `GET /service/rest/v1/tasks` | up to four tasks: failed first, then running, then by next run; shown only with `nexus:tasks:read` |

Each format shows by its own name, such as NuGet or PyPI, for every format the server can hold (`ui/web/js/format.js`); an id outside that list shows with a capital first letter. While the server is read only, the band of the frame says so on every screen but S37, which shows the state itself, and the overview does not repeat it.

### S03 Search

Permission: `nexus:search:read`.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/search` | components matching the query, `PageComponentXO` |
| REST | `GET /service/rest/v1/repositories` | choices of the repository field |

Query parameters the interface sends, each only when its field is filled:

| Field | Parameter |
| --- | --- |
| Keyword | `q` |
| Repository | `repository` |
| Format | `format` (`maven2`, `npm`, `docker`, `raw`) |
| Group | `group` |
| Name | `name` |
| Version | `version` |
| Checksum | `md5`, `sha1`, `sha256` or `sha512`, picked by the length of the value (32, 40, 64, 128 hex digits) |
| Maven | `maven.groupId`, `maven.artifactId`, `maven.baseVersion`, `maven.extension`, `maven.classifier` |
| npm | `npm.scope`, `npm.author`, `npm.description`, `npm.keywords`, `npm.license` |
| Docker | `docker.imageName`, `docker.imageTag`, `docker.layerId`, `docker.contentDigest` |
| Paging | `continuationToken` |

For maven2 the artifact field is `maven.artifactId`; for npm and raw the name field is `name`; for docker the image name is `docker.imageName`.

`PageComponentXO`: `{"items": [ComponentXO], "continuationToken": string | null}`. `ComponentXO`: `id`, `repository`, `format`, `group`, `name`, `version`, `assets` (list of `AssetXO`). A row opens S05 at the path of the first asset.

### S04 Browse

Permission: a session. The server lists only repositories the user may browse.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/repositories` | `[RepositoryXO]`: `name`, `format`, `type`, `url`, `attributes` |
| API | `GET /api/repositories` | `{"repositories": [...]}` with `files`, `bytes`, `newest_ms` per repository, joined by name; a group counts zero of its own, so the page shows its files as held by the members; a repository missing there shows no counts |

### S05 Repository Tree

Permission: a session; delete needs `nexus:repository-view:<format>:<repository>:delete`.

| Kind | Request | Use |
| --- | --- | --- |
| API | `GET /api/browse/{repository}?path=&limit=&offset=` | one folder level: `{"repo", "path", "limit", "offset", "more", "children": [{"name", "path", "is_dir", "size", "created_ms"}]}` |
| API | `GET /api/asset/{repository}?path=` | detail of one file: `repo`, `member` (`null` outside a group), `path`, `name`, `size`, `sha1`, `content_type`, `created_ms`, `store`, `access` (how the repository is served: `maven`, `npm`, `docker` or `files`), `download_url`, `maven`, `docker`, `npm` |
| Proposed | field `asset_id` in the answer of `GET /api/asset/{repository}` | the id of the same file in the REST namespace, so the next two rows can follow |
| REST | `GET /service/rest/v1/assets/{id}` | `AssetXO`: `checksum` map, `uploader`, `uploaderIp`, `lastModified`, `lastDownloaded`, `blobCreated`, `blobStoreName`, `fileSize`, `contentType`, `downloadUrl` |
| REST | `DELETE /service/rest/v1/assets/{id}` | delete the file after confirmation, `204` |

The objects `maven`, `npm` and `docker` are `null` or carry these fields, as the reference server and the server answer them:

| Object | Fields |
| --- | --- |
| `maven` | `group_id`, `artifact_id`, `version`, `classifier` (`null` when there is none), `extension`; `null` for metadata and checksum files; a timestamped snapshot file has the version of its folder |
| `npm` | `kind` (`document` or `tarball`), `name` with its scope, `version` (`null` for a document), `registry`, `install` (the install command) |
| `docker` | `kind` (`manifest` or `blob`), `image`, `tag`, `digest`, `port`, `pull` (the pull command of a manifest, `null` otherwise) |

The detail labels these fields by name and offers a copy button for `install` and `pull`. A field the interface does not know gets a label made from its name (`base_version` becomes "Base version"), so no field name shows raw.

A fact the server did not record reads "Not recorded": `uploader`, `uploaderIp`, `lastModified`, `lastDownloaded` or `blobCreated` as `null`, a missing size, content type or blob store. A file without any checksum shows "Not recorded" under Checksums. A file of a proxy repository, also when reached through a group, keeps "Cached from a remote" for a missing uploader.

The tree loads one level per request, 200 children at a time, with a button for the next 200 while `more` is true. Without `asset_id` the detail shows the API fields only and offers no delete.

### S06 Upload

Permission: `nexus:component:add`.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/repositories` | hosted repositories of the formats maven2, npm and raw |

### S07 Upload Form

Permission: `nexus:component:add` and `nexus:repository-view:<format>:<repository>:add`.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/formats/{format}/upload-specs` | the fields of the form: `UploadDefinitionXO` with `format`, `multipleUpload`, `componentFields` and `assetFields`, each field `{name, type, description, optional, group}` |
| REST | `POST /service/rest/v1/components?repository={name}` | `multipart/form-data` |

Form fields of the upload, by format. `N` is 1, 2 or 3 where the specs say `multipleUpload`.

| Format | Component fields | Asset fields |
| --- | --- | --- |
| maven2 | `maven2.groupId`, `maven2.artifactId`, `maven2.version`, `maven2.generate-pom`, `maven2.packaging` | `maven2.assetN` (file), `maven2.assetN.extension`, `maven2.assetN.classifier` |
| npm | | `npm.asset` (file) |
| raw | `raw.directory` | `raw.assetN` (file), `raw.assetN.filename` |

The REST description declares no success status for the upload. The interface treats `200`, `201` and `204` as success and shows a link to the folder of the upload in S05.

### S08 Section Landing

Permission: a session. No request: the cards come from the navigation table, filtered by permission.

### S09 Repositories

Permission: `nexus:repository-admin:*:*:read`.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/repositorySettings` | full settings of every repository the user may administer: `name`, `format`, `type`, `url`, `online`, `storage.blobStoreName`, and the format attributes; `403` when the user may administer none |

### S10 New Repository

Permission: `nexus:repository-admin:*:*:add`. No request. Twelve choices: the formats maven2, npm, docker and raw, each as hosted, proxy and group.

### S11 Repository Editor

Permission: read `nexus:repository-admin:*:*:read`, create `...:add`, save `...:edit`, delete `...:delete`.

The REST paths name the format with its own segment: `maven` for maven2, `npm`, `docker`, `raw`.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/repositories/{name}` | format and type of a repository opened by name, `RepositoryXO` |
| REST | `GET /service/rest/v1/repositories/{segment}/{type}/{name}` | load |
| REST | `POST /service/rest/v1/repositories/{segment}/{type}` | create, `201` |
| REST | `PUT /service/rest/v1/repositories/{segment}/{type}/{name}` | save, `204`. The name cannot change |
| REST | `DELETE /service/rest/v1/repositories/{name}` | delete after the name is typed, `204` |
| REST | `POST /service/rest/v1/repositories/{name}/rebuild-index` | hosted and proxy, `204` |
| REST | `POST /service/rest/v1/repositories/{name}/invalidate-cache` | proxy and group, `204` |
| REST | `GET /service/rest/v1/blobstores` | choices of the blob store |
| Proposed | `GET /api/cleanup-policies` | choices of the cleanup policies, hosted and proxy |
| REST | `GET /service/rest/v1/routing-rules` | choices of the routing rule, proxy |
| REST | `GET /service/rest/v1/repositories` | choices of the members, group: repositories of the same format other than this one |

Request bodies, built from the form. A star marks a field the server requires.

| Part | Fields | Formats and types |
| --- | --- | --- |
| top | `name`*, `online`* | all |
| `storage` | `blobStoreName`*, `strictContentTypeValidation`* | all |
| `storage` | `writePolicy`* (`ALLOW`, `ALLOW_ONCE`, `DENY`) | hosted |
| `storage` | `latestPolicy` | docker hosted |
| `cleanup` | `policyNames` (list) | hosted, proxy |
| `component` | `proprietaryComponents` | hosted; the interface sends `false` and shows no field |
| `maven` | `versionPolicy`* (`RELEASE`, `SNAPSHOT`, `MIXED`), `layoutPolicy`* (`STRICT`, `PERMISSIVE`), `contentDisposition` (`INLINE`, `ATTACHMENT`) | maven2 |
| `raw` | `contentDisposition` (`INLINE`, `ATTACHMENT`) | raw |
| `npm` | `removeQuarantined`* | npm proxy; the interface sends `false` and shows no field |
| `docker` | `v1Enabled`*, `forceBasicAuth`*, `httpPort`, `httpsPort`, `subdomain`, `pathEnabled` | docker |
| `dockerProxy` | `indexType` (`HUB`, `REGISTRY`, `CUSTOM`), `indexUrl`, `cacheForeignLayers`, `foreignLayerUrlWhitelist` (list) | docker proxy |
| `proxy` | `remoteUrl`, `contentMaxAge`*, `metadataMaxAge`* (minutes) | proxy |
| `negativeCache` | `enabled`*, `timeToLive`* (minutes) | proxy |
| `httpClient` | `blocked`*, `autoBlock`*, `connection` (`retries`, `userAgentSuffix`, `timeout`, `enableCircularRedirects`, `enableCookies`, `useTrustStore`), `authentication` (`type`: `username`, `ntlm` or `bearerToken`; `username`, `password`, `ntlmHost`, `ntlmDomain`, `bearerToken`, `preemptive`) | proxy |
| `replication` | `preemptivePullEnabled`*, `assetPathRegex` | proxy |
| top | `routingRule` | proxy, in the request |
| `group` | `memberNames` (ordered list) | group |
| `group` | `writableMember` | npm and docker group; the interface sends nothing, because the server refuses writes to a group (plan decision P10) |

Names that differ between answer and request: the answer of a proxy names its rule `routingRuleName`, the request `routingRule`. The interface reads one and writes the other.

Fields of functions this server does not have are not offered: `docker.v1Enabled`, `httpClient.connection.useTrustStore`, the authentication type `ntlm` with `ntlmHost` and `ntlmDomain`, and `replication`. The interface sends back the values it read. The server stores such a field and refuses, with `400` at the field, a value that would switch on a function it does not have (server plan, decision A23).

The form shows "Allow anonymous pull" for docker as the opposite of `forceBasicAuth`. The password of the upstream authentication is never read back: the answer leaves it empty, and the form sends it only when the field was filled. The health check of a proxy is shown as not available; plan decision P12 keeps the server from calling an outside service.

### S12 Blob Stores

Permission: `nexus:blobstores:read`.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/blobstores` | `[GenericBlobStoreApiResponse]`: `name`, `type`, `unavailable`, `blobCount`, `totalSizeInBytes`, `availableSpaceInBytes`, `softQuota` (`type`, `limit`) |

### S13 Blob Store Editor

Permission: read `nexus:blobstores:read`, create `nexus:blobstores:create`, save `nexus:blobstores:update`, delete `nexus:blobstores:delete`. Type file only.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/blobstores/file/{name}` | `path`, `softQuota` |
| REST | `POST /service/rest/v1/blobstores/file` | create: `{"name", "path", "softQuota"}`, `204` |
| REST | `PUT /service/rest/v1/blobstores/file/{name}` | save: `{"path", "softQuota"}`, `204` |
| REST | `DELETE /service/rest/v1/blobstores/{name}` | delete after the name is typed, `204`; `409` while a repository uses it |
| REST | `GET /service/rest/v1/blobstores/{name}/quota-status` | `isViolation`, `message` |
| REST | `GET /service/rest/v1/blobstores` | the counts of this store |
| Proposed | `GET /api/blob-stores/{name}/deleted` | blobs marked for deletion: `{"name": string, "blobCount": number, "bytes": number}` |
| REST | `GET /service/rest/v1/repositorySettings` | which repositories use the store |

`softQuota` is `null` or `{"type": "spaceRemainingQuota" | "spaceUsedQuota", "limit": bytes}`. The form takes the limit in MB and sends bytes (`limit * 1048576`). A limit under 1048576 bytes is `400` with `id` `softQuota.limit`.

### S14 Cleanup Policies

Permission: `nexus:settings:read` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| Proposed | `GET /api/cleanup-policies` | `[CleanupPolicy]` |

### S15 Cleanup Policy Editor

Permission: read `nexus:settings:read`, create and save and delete `nexus:settings:update` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| Proposed | `GET /api/cleanup-policies/{name}` | load |
| Proposed | `POST /api/cleanup-policies` | create, `201` with the policy; `409` for a name that exists |
| Proposed | `PUT /api/cleanup-policies/{name}` | save, `204`. The name cannot change. `400` with `id` `format` when a repository of another format applies the policy |
| Proposed | `DELETE /api/cleanup-policies/{name}` | delete after confirmation, `204`; `409` while a repository uses it, with the names in the message |
| Proposed | `POST /api/cleanup-policies/preview` | what the policy would remove from one repository, nothing is removed |
| REST | `GET /service/rest/v1/repositories` | choices of the preview repository |

CleanupPolicy:

```json
{
  "name": "string",
  "format": "* | maven2 | npm | docker | raw",
  "notes": "string",
  "criteria": {
    "lastBlobUpdatedDays": 30,
    "lastDownloadedDays": null
  },
  "usedBy": ["repository name"]
}
```

`usedBy` is in answers only. At least one criterion is a whole number of 1 or more; a missing or `null` criterion is not applied. The server answers `400` with `id` `criteria` when both are missing.

Preview request `{"policy": CleanupPolicy, "repository": string}` (errors name `policy.<field>` or `repository` as `id`), answer `{"items": [{"repository", "group", "name", "version", "path"}], "total": number, "truncated": boolean}`. The answer holds at most 100 items; `total` counts all.

### S16 Routing Rules

Permission: `nexus:settings:read` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/routing-rules` | `[RoutingRuleXO]`: `name`, `description`, `mode` (`ALLOW`, `BLOCK`), `matchers` |
| REST | `GET /service/rest/v1/repositorySettings` | count of repositories per rule, from `routingRuleName` of each proxy |

### S17 Routing Rule Editor

Permission: read `nexus:settings:read`, create and save and delete `nexus:settings:update` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/routing-rules/{name}` | load |
| REST | `POST /service/rest/v1/routing-rules` | create, `RoutingRuleXO`, `204` |
| REST | `PUT /service/rest/v1/routing-rules/{name}` | save, `204` |
| REST | `DELETE /service/rest/v1/routing-rules/{name}` | delete after confirmation, `204`; `409` while a repository uses the rule |
| Proposed | `POST /api/routing-rules/test` | test one path against the rule in the form |
| REST | `GET /service/rest/v1/repositorySettings` | the repositories that use the rule |

Test request `{"mode": "ALLOW" | "BLOCK", "matchers": [string], "path": string}`, answer `{"allowed": boolean, "matcher": number | null}`, where `matcher` is the index of the first matcher that matched. Matchers apply to the whole path; a path without a leading `/` gets one. The server tests with its own expression engine, so the answer is the one a request would get. A matcher that does not compile gives `400` with `id` `matchers[<index>]`.

### S18 Routing Preview

Permission: `nexus:settings:read` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| Proposed | `POST /api/routing-rules/preview` | the verdict of every repository in scope for one path |

Request `{"scope": "all" | "group" | "proxy", "path": string}`, answer `{"items": [{"repository", "type", "format", "rule", "allowed"}]}`. `rule` is `null` for a repository without a rule; such a repository allows every path.

### S19 Privileges

Permission: `nexus:privileges:read`. Read only.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/security/privileges` | `[ApiPrivilege]`: `type`, `name`, `description`, `readOnly`, and the fields of the type |

Fields of the types, as the create requests of the old product name them: `application` has `domain`, `actions`; `repository-view` and `repository-admin` have `format`, `repository`, `actions`; `repository-content-selector` adds `contentSelector`; `wildcard` has `pattern`; `script` has `scriptName`, `actions`. The interface builds the permission string from these fields by the rules of the identity notes of the product: `nexus:<domain>:<actions>`, `nexus:repository-view:<format>:<repository>:<actions>`, the pattern as written for `wildcard`.

### S20 Privilege Detail

Permission: `nexus:privileges:read`. Read only.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/security/privileges/{name}` | one privilege |
| REST | `GET /service/rest/v1/security/roles` | the roles that apply it |

### S21 Roles

Permission: `nexus:roles:read`.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/security/roles` | `[RoleXOResponse]`: `id`, `source`, `name`, `description`, `readOnly`, `privileges`, `roles` |

### S22 Role Editor

Permission: read `nexus:roles:read`, create `nexus:roles:create`, save `nexus:roles:update`, delete `nexus:roles:delete`.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/security/roles/{id}` | load |
| REST | `POST /service/rest/v1/security/roles` | create, `RoleXORequest` (`id`, `name`, `description`, `privileges`, `roles`), `200` with `RoleXOResponse` |
| REST | `PUT /service/rest/v1/security/roles/{id}` | save, `204`. The id cannot change |
| REST | `DELETE /service/rest/v1/security/roles/{id}` | delete after confirmation, `204`. A change or delete of a built in role is `400` with `id` `*` |
| REST | `GET /service/rest/v1/security/privileges` | choices of the privileges |
| REST | `GET /service/rest/v1/security/roles` | choices of the contained roles |
| REST | `GET /service/rest/v1/security/user-sources` | sources for a mapped role: `[{"id", "name"}]` |
| REST | `GET /service/rest/v1/security/roles?source={source}` | groups of an external source, the choices of a mapped role |

A mapped role is an ordinary role whose `id` equals the name of an external group. The form picks a source other than `default` and a group of it, and sends the group name as `id`. A role with `readOnly` true opens read only.

### S23 Users

Permission: `nexus:users:read`.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/security/user-sources` | choices of the source; needs `nexus:users:read` or `nexus:roles:read` |
| REST | `GET /service/rest/v1/security/users?source={source}&userId={text}` | `[ApiUser]`: `userId`, `firstName`, `lastName`, `emailAddress`, `source`, `status` (`active`, `locked`, `disabled`, `changepassword`), `readOnly`, `roles`, `externalRoles`. `userId` is sent only when the filter is filled |

### S24 User Editor

Permission: read `nexus:users:read`, create `nexus:users:create`, save `nexus:users:update`, delete `nexus:users:delete`, password `nexus:userschangepw:create`.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/security/users?userId={id}&source={source}` | load; the interface keeps the entry whose `userId` equals the id |
| REST | `POST /service/rest/v1/security/users` | create, `ApiCreateUser` (`userId`, `firstName`, `lastName`, `emailAddress`, `password`, `status`, `roles`), `200` with `ApiUser` |
| REST | `PUT /service/rest/v1/security/users/{userId}` | save, the whole `ApiUser`, `204` |
| REST | `DELETE /service/rest/v1/security/users/{userId}?realm={realm}` | delete after confirmation, `204`; `409` for a directory user, for the own user and for the user anonymous access acts as |
| REST | `PUT /service/rest/v1/security/users/{userId}/change-password` | new password as the body, `Content-Type: text/plain`, `204` |
| REST | `GET /service/rest/v1/security/roles` | choices of the roles |
| Proposed | `GET /api/tokens?userId={id}` | tokens of the user, see S29 |
| Proposed | `DELETE /api/tokens/{id}` | revoke one after confirmation |

A user of the source `LDAP` keeps names, mail and state from the directory. The form shows them read only and lets the roles change; it offers no password and no delete.

### S25 Anonymous Access

Permission: read `nexus:settings:read`, save `nexus:settings:update`.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/security/anonymous` | `{"enabled", "userId", "realmName"}` |
| REST | `PUT /service/rest/v1/security/anonymous` | save the same shape, `200` with it |
| REST | `GET /service/rest/v1/security/realms/available` | choices of the realm |

### S26 LDAP Connections

Permission: `nexus:ldap:read` (unverified); order and cache `nexus:ldap:update`.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/security/ldap` | the connections with `id`, `name`, `order`, `protocol`, `host`, `port` and the rest of the read shape |
| REST | `POST /service/rest/v1/security/ldap/change-order` | the names in the new order, `204` |
| Proposed | `POST /api/ldap/clear-cache` | forget cached sign ins, `204` |

### S27 LDAP Editor

Permission: read `nexus:ldap:read`, create `nexus:ldap:create`, save `nexus:ldap:update`, delete `nexus:ldap:delete` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/security/ldap/{name}` | load; the answer holds no password |
| REST | `POST /service/rest/v1/security/ldap` | create, `201` without a body |
| REST | `PUT /service/rest/v1/security/ldap/{name}` | save, `204`. The body may carry a new `name`; the page then moves to it |
| REST | `DELETE /service/rest/v1/security/ldap/{name}` | delete after confirmation, `204` |
| Proposed | `POST /api/ldap/verify-connection` | connect and bind with the form values |
| Proposed | `POST /api/ldap/verify-user-mapping` | search users with the form values |
| Proposed | `POST /api/ldap/verify-login` | sign in one user with the form values |

Fields of the connection, the same in create and save: `name`*, `protocol`* (`ldap`, `ldaps`), `useTrustStore`, `host`*, `port`*, `searchBase`*, `authScheme`* (`NONE`, `SIMPLE`, `DIGEST_MD5`, `CRAM_MD5`), `authRealm`, `authUsername`, `authPassword`*, `connectionTimeoutSeconds`*, `connectionRetryDelaySeconds`*, `maxIncidentsCount`*, `userBaseDn`, `userSubtree`, `userObjectClass`, `userLdapFilter`, `userIdAttribute`, `userRealNameAttribute`, `userEmailAddressAttribute`, `userPasswordAttribute`, `ldapGroupsAsRoles`, `groupType`* (`static`, `dynamic`), `groupBaseDn`, `groupSubtree`, `groupObjectClass`, `groupIdAttribute`, `groupMemberAttribute`, `groupMemberFormat`, `userMemberOfAttribute`. Save adds `id`.

`useTrustStore` is not offered, for the same reason as in S11. The bind password is never read back. The form asks for it again on every save of a connection whose scheme is not `NONE`. Plan decision P9 keeps the stored password in a file that only the server reads.

Verify requests carry the form as a connection object. Answers:

| Request | Body | Answer |
| --- | --- | --- |
| `POST /api/ldap/verify-connection` | connection | `{"ok": boolean, "message": string}` |
| `POST /api/ldap/verify-user-mapping` | connection | `{"ok": boolean, "message": string, "users": [{"userId", "name", "email", "groups": [string]}]}`, at most 20 users |
| `POST /api/ldap/verify-login` | `{"connection": connection, "username": string, "password": string}` | `{"ok": boolean, "message": string}` |

A failed check is `200` with `ok` false; a malformed form is `400`, and for the sign in check its field ids start with `connection.`. The three checks need `nexus:ldap:create` or `nexus:ldap:update`.

### S28 Realms

Permission: read `nexus:settings:read`, save `nexus:settings:update`.

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/security/realms/available` | `[{"id", "name"}]` |
| REST | `GET /service/rest/v1/security/realms/active` | ids in their order |
| REST | `PUT /service/rest/v1/security/realms/active` | the new order of the active ids, `204` |

The interface refuses to save an empty list and warns when the local authenticating realm is not active, because no local user could sign in.

### S29 Tokens

Permission: `nexus:apikey:*` for the own tokens; the tokens of another user need `nexus:users:update` as well.

| Kind | Request | Use |
| --- | --- | --- |
| Proposed | `GET /api/tokens` | the own tokens |
| Proposed | `GET /api/tokens?userId={id}` | the tokens of one user |
| Proposed | `POST /api/tokens` | issue one: `{"kind": "npm" | "docker" | "user", "name": string}`, `201` with the token; `400` with `id` `kind` when the realm of the kind is not active |
| Proposed | `DELETE /api/tokens/{id}` | revoke after confirmation, `204` |
| REST | `GET /service/rest/v1/security/realms/active` | the kinds the active realms accept. The old product needs `nexus:settings:read` for this path; the proposal is that the server also answers it for a session with `nexus:apikey:*`, as the mock does |

Token in a list: `{"id", "kind", "name", "userId", "created", "lastUsed", "publicPart"}`. The answer of `POST` adds `"secret": string`, the whole token for the client. The server keeps only a digest of it (plan section Identity), so the interface shows it once, with a copy button, and never again.

Kinds and realms: `npm` needs the npm token realm, `docker` the Docker token realm, `user` the user token realm. A kind whose realm is not active is not offered.

### S30 Logging

Permission: read `nexus:logging:read`, change `nexus:logging:update` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| Proposed | `GET /api/logging/loggers` | `[{"name": string, "level": "DEBUG" | "INFO" | "WARN" | "ERROR", "default": "INFO" | null}]`; `default` is `null` for a logger an administrator added |
| Proposed | `PUT /api/logging/loggers/{name}` | set or add: `{"level"}`, `204` |
| Proposed | `DELETE /api/logging/loggers/{name}` | an added logger is removed, a built in one goes back to its default, `204` |
| Proposed | `POST /api/logging/reset` | every logger back to its default and every added one removed, `204` |

The four levels are those of the structured log of plan phase 7. The server serves the loggers it has; it may refuse a new name with `400` at `name`, and the screen then shows that message beside the field.

### S31 Log Files

Permission: `nexus:logging:read` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| Proposed | `GET /api/logs` | `[{"name": string, "size": number, "lastModified": string}]`; `name` is relative to the log directory, for example `tasks.log`. The server keeps one task log for all task runs |

### S32 Log Viewer

Permission: `nexus:logging:read` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| Proposed | `GET /api/logs/{name}?bytes={n}` | the last `n` bytes as `text/plain; charset=utf-8`; `n` is 25600, 51200 or 102400 |
| Proposed | `GET /api/logs/{name}?download=1` | the whole file with `Content-Disposition: attachment` |

`{name}` is the path with each segment URL encoded and `/` kept. A name with `..` or a leading `/` is `400`. The viewer refreshes on a button, or every 20 seconds, minute, 2 or 5 minutes when picked.

### S33 Status

Permission: `nexus:atlas:read` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/status/check` | `{"<check name>": {"healthy", "message", "error", "details", "time", "duration", "timestamp"}}` |
| REST | `GET /service/rest/v1/status/writable` | `200` writable, `503` not; not asked while the interface knows the server is read only (section Areas), because it then answers `503` |

### S34 System Information

Permission: `nexus:atlas:read` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/beta/system/information` | `{"<section>": {"<key>": value}}`; the interface shows each section as a list and offers the whole answer as a JSON download made in the browser |

The sections are the server's own. The REST description declares no answer body, so only the two levels of maps are pinned. The interface expects the sections `version` and `schema` and shows them first when present.

### S35 Capabilities

Permission: `nexus:capabilities:read` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/capabilities` | `[CapabilityDTO]`: `id`, `type`, `notes`, `enabled`, `properties` (map of strings) |
| Proposed | `GET /api/capabilities/support` | what this server does with each type: `{"<type>": {"supported": boolean, "reason": string}}`; a type missing from the map counts as not supported |

### S36 Capability Editor

Permission: read `nexus:capabilities:read`, save `nexus:capabilities:update`, delete `nexus:capabilities:delete` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/capabilities` | load; the interface keeps the entry with the id |
| REST | `PUT /service/rest/v1/capabilities/{id}` | save notes, properties and `enabled`, the whole `CapabilityDTO`; any `2xx` is success |
| REST | `DELETE /service/rest/v1/capabilities/{id}` | delete after confirmation; any `2xx` is success |
| Proposed | `GET /api/capabilities/support` | whether the type has an effect here |

Enable and disable are a save with `enabled` changed.

### S37 Node

Permission: read `nexus:settings:read`, freeze and release `nexus:settings:update` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/system/node` | `{"nodeId"}` |
| REST | `GET /service/rest/v1/read-only` | `{"frozen", "systemInitiated", "summaryReason"}` |
| REST | `POST /service/rest/v1/read-only/freeze` | after confirmation, `204` |
| REST | `POST /service/rest/v1/read-only/release` | `204` |
| REST | `POST /service/rest/v1/read-only/force-release` | only when `systemInitiated` is true, after confirmation, `204` |

### S38 Tasks

Permission: `nexus:tasks:read`; run `nexus:tasks:start`, stop `nexus:tasks:stop` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/tasks` | `PageTaskXO`: `{"items": [TaskXO], "continuationToken"}`; `TaskXO`: `id`, `name`, `type`, `message`, `currentState`, `lastRunResult`, `nextRun`, `lastRun`, `schedule` |
| REST | `POST /service/rest/v1/tasks/{id}/run` | `204`; `405` when the task is disabled |
| REST | `POST /service/rest/v1/tasks/{id}/stop` | `204`; `409` when it is not running |

`currentState` values the interface knows: `WAITING`, `RUNNING`, `CANCELED`; `lastRunResult`: `OK`, `FAILED`, `CANCELED`, `null` before the first run. Others are shown as they come. `TaskXO` carries no `enabled` field, so S38 offers Run for every task and shows the `405` of a disabled one as a message; S40 reads `enabled` from the settings and hides Run. S38 shows the type id, because it may call only the task list.

### S39 New Task

Permission: `nexus:tasks:create` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| Proposed | `GET /api/task-types` | the types this server runs; needs `nexus:tasks:read`, because S40 also needs it to show an existing task |

TaskType:

```json
{
  "id": "string",
  "name": "string",
  "description": "string",
  "fields": [
    {
      "id": "string",
      "label": "string",
      "type": "string | number | boolean | select | repository | blobstore",
      "required": true,
      "default": null,
      "options": [{"value": "string", "label": "string"}]
    }
  ]
}
```

`options` is present for `select`. For `repository` the interface offers the repositories of `GET /service/rest/v1/repositories` plus `*` for all; for `blobstore` the stores of `GET /service/rest/v1/blobstores`.

### S40 Task Editor

Permission: read `nexus:tasks:read`, create `nexus:tasks:create`, save `nexus:tasks:update`, delete `nexus:tasks:delete`, run `nexus:tasks:start`, stop `nexus:tasks:stop` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| REST | `GET /service/rest/v1/tasks/{id}` | the summary, `TaskXO` |
| Proposed | `GET /api/tasks/{id}` | the settings, `TaskSettings` |
| Proposed | `POST /api/tasks` | create, `TaskSettings` without `id`, `201` with `{"id"}` |
| Proposed | `PUT /api/tasks/{id}` | save, `204`. The type cannot change |
| Proposed | `DELETE /api/tasks/{id}` | delete after confirmation, `204` |
| REST | `POST /service/rest/v1/tasks/{id}/run` | run now |
| REST | `POST /service/rest/v1/tasks/{id}/stop` | stop |
| Proposed | `GET /api/task-types` | the fields of the type |

TaskSettings:

```json
{
  "id": "string",
  "typeId": "string",
  "name": "string",
  "enabled": true,
  "schedule": {
    "type": "manual | once | hourly | daily | weekly | monthly | cron",
    "startAt": "2026-11-12T03:00:00Z",
    "weekDays": ["MON"],
    "monthDays": [1],
    "cron": "0 0 3 * * ?"
  },
  "properties": {"<field id>": "string"}
}
```

`startAt` is required for every schedule type but `manual` and `cron`; a `cron` expression is read in UTC; `weekDays` (`MON` to `SUN`) for `weekly`; `monthDays` (1 to 31) for `monthly`; `cron` for `cron`. Property values travel as strings, as in the old product; booleans are `"true"` and `"false"`. The interface shows times in the time zone of the browser and sends UTC.

### S41 Account

Permission: a session.

| Kind | Request | Use |
| --- | --- | --- |
| Proposed | `GET /api/session` | the own user |
| Proposed | `PUT /api/account` | save the own names and mail: `{"firstName", "lastName", "emailAddress"}`, `204` |
| Proposed | `POST /api/account/password` | change the own password: `{"currentPassword", "newPassword"}`, `204`; `400` with `id` `currentPassword` when it is wrong |

A user of the source `LDAP` gets the own data read only and no password change; the server answers `409` to both requests for such a user.

### S42 About

Permission: none.

| Kind | Request | Use |
| --- | --- | --- |
| API | `GET /api/status` | `name`, `version` |

### S43 Metrics

Permission: `nexus:metrics:read` (unverified).

| Kind | Request | Use |
| --- | --- | --- |
| Proposed | `GET /api/metrics/requests?from={date}&to={date}` | `{"from", "to", "days": [{"day": "2026-11-12", "requests": number, "bytesOut": number}]}` |
| Proposed | `GET /api/metrics/downloads?from={date}&to={date}` | `{"from", "to", "rows": [{"repository", "day", "count"}]}`; per repository and day, without user or address (plan decision P5) |

Dates are `YYYY-MM-DD` in UTC, `to` included; the interface asks for the last 7, 30 or 90 days. The REST path `/service/rest/v1/monthly-metrics` is not used: its description declares no answer body.

### S44 No Access

No request. Shown in place of a screen the session does not have the permission for, with a link to the overview.

### S45 Not Found

No request. Shown for a route that does not exist and for a `404` of the object a screen loads.

## Frame

On every screen.

| Kind | Request | Use |
| --- | --- | --- |
| Proposed | `GET /api/session` | once at load; `user` `null` opens S01 |
| Proposed | `DELETE /api/session` | sign out |
| Proposed | `GET /api/areas` | once after the sign in or at load: the requests the server refuses and its read only state (section Areas) |
| REST | `GET /service/rest/v1/read-only` | a mark in the top bar while the server is read only, and no control for a change on any screen except the release on S37; with `nexus:settings:read`, for other users from `GET /api/areas` |

## Preview

The first build of the server answers only part of this contract: the reads of status, session, repositories with their members, tree, search, file detail, users, roles, privileges, LDAP settings, blob stores and the read only state, which is frozen by the system. Every change answers `501` in the form of section Errors, and every other read answers `501` too (the mock answers `404`). The interface stays usable against such a server:

- After the sign in it sends one read request per area the user may open, listed in `ui/web/js/preview.js` (`PROBES`). An area whose request answers `501`, or `404` on a path of this contract, is not in this preview. With the answer of section Areas, the interface answers every refused probe itself, so only reads the server serves leave the browser.
- The navigation and the section landings mark such an area with "Not in preview". Its screens show a calm state "Not in this preview" with a link to the overview, never an error.
- While the server reports itself read only, no screen offers a control for a change: buttons and links for changes are hidden as they are for a user whose roles do not allow the change, forms are read only with a notice that gives the reason of the server, and a screen that only makes changes (S07, S10, S11 new, S13 new and the other "new" screens) shows a calm state "The server takes no changes". The release on S37 stays, because it ends the read only state. Navigation follows the roles only.
- An action that answers `501` shows a calm notice "Not in this preview" and keeps the form as it was. From then on, until the page is loaded again, the interface treats the server as taking no changes, as above, and a band "No changes" says so. Previews, tests and verifications (S15, S17, S18, S27) change nothing; their `501` does not count. A `404` on a path whose probe failed counts as "not in this preview" anywhere, for example the tokens section of S24.
- Areas that answer stay as they are. The band "Example data" appears only while answers carry the mock header, so it never shows against the server.

The probe paths are reads without side effects. A server that answers every path of the contract makes the preview invisible: no area is marked.

## Areas

Proposed to the server session on 2026-11-12. The interface and the mock answer it; the server does not have it yet. The browser writes every request that fails into the console as an error, and a page cannot suppress these lines. So that a walk through the interface leaves none, the server names in one answer what it refuses:

| Kind | Request | Use |
| --- | --- | --- |
| Proposed | `GET /api/areas` | `{"readOnly": {"frozen", "reason"}, "refused": [{"method", "path", "area"}]}` for every signed in user, `401` without a session |

- `refused` holds every request of this contract the server answers with `501`, one entry per method and path, in the spelling of this file. `{name}` stands for exactly one path segment, and a path with alternatives such as `(run|stop)` gets one entry per alternative. `area` is the field of the `501`; the interface does not read it.
- The interface asks once, after the sign in or at load. It answers a request that matches an entry itself, as a `501`, and does not send it. Probes, screens and actions then behave as in section Preview without a failed request.
- `readOnly` carries `frozen` and, as `reason`, the `summaryReason` of `GET /service/rest/v1/read-only`. Users without `nexus:settings:read` see the band and no control for a change as well. Administrators still read the full state for S37.
- While `readOnly.frozen` is true, S33 does not ask `GET /service/rest/v1/status/writable`.
- A server that answers `404` to this path keeps the behaviour of section Preview: probes, one console line per refused area, and one for this path.

## Mock

`ui/mock/server.py` implements every row of this file with invented data in memory. With `--preview` it plays the first build of the server: the reads listed under Preview answer, every other read answers `404`, and every change `501`. `GET /api/areas` names all of these as refused, the read only state starts frozen by the system, and the example data header is left out. These things exist only in the mock and are not part of the contract:

- Every answer carries `X-Pipesmith-Example-Data: 1`. The interface shows a band "Example data" on every screen while it sees this header. The server never sends it.
- `POST /__mock/reset` restores the fixtures, so that every check starts from the same state.
- `--no-example-header` leaves the header out while every path answers, and `--public-url <address>` names that address in repository and download addresses instead of the address of the request. `marketing/render.py` uses both, with `https://registry.example.com`.
- `GET /__mock/scenario/{name}?then=<route>` sets the read only state to `writable`, `frozen`, `frozen-by-system` or `preview` and answers `302` to `/#<route>`. Five screen checks and two flows of the system area and two flows of the preview use it; they do not work against a real server.
- `GET /__mock/no-areas?then=<route>` makes `GET /api/areas` answer `404` until the next reset and answers `302` to `/#<route>`. One flow of the preview uses it to play a server without section Areas.

## Proposed Paths

The paths the server does not have yet, in one list for the port session.

| Request | Screen |
| --- | --- |
| `POST /api/session`, `GET /api/session`, `DELETE /api/session` | S01, frame, S41 |
| field `asset_id` in `GET /api/asset/{repository}` | S05 |
| `GET /api/areas` | frame, S33 |
| `GET /api/cleanup-policies`, `GET`, `PUT`, `DELETE /api/cleanup-policies/{name}`, `POST /api/cleanup-policies`, `POST /api/cleanup-policies/preview` | S11, S14, S15 |
| `GET /api/blob-stores/{name}/deleted` | S13 |
| `POST /api/routing-rules/test`, `POST /api/routing-rules/preview` | S17, S18 |
| `POST /api/ldap/clear-cache`, `POST /api/ldap/verify-connection`, `POST /api/ldap/verify-user-mapping`, `POST /api/ldap/verify-login` | S26, S27 |
| `GET /api/tokens`, `POST /api/tokens`, `DELETE /api/tokens/{id}` | S24, S29 |
| `GET /api/logging/loggers`, `PUT`, `DELETE /api/logging/loggers/{name}`, `POST /api/logging/reset` | S30 |
| `GET /api/logs`, `GET /api/logs/{name}` | S31, S32 |
| `GET /api/capabilities/support` | S35, S36 |
| `GET /api/task-types`, `GET`, `PUT`, `DELETE /api/tasks/{id}`, `POST /api/tasks` | S39, S40 |
| `PUT /api/account`, `POST /api/account/password` | S41 |
| `GET /api/metrics/requests`, `GET /api/metrics/downloads` | S43 |
