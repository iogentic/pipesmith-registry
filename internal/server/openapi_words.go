package server

// The words of the OpenAPI document. Each entry of operationWords is keyed
// by method and the generic template of its route (see genericTemplate)
// and was checked against the handler of that route.

// param is a query or header parameter.
type param struct {
	name, in, typ, meaning string
	def                    any
	min, max               *int
	maxLength              int
	required               bool
	enum                   []string // the values the handler takes, when it takes a fixed set
}

// reply is one answer: a status, its content type and one line. Without a
// text it is the shared answer of components.responses for its status.
type reply struct {
	status, ctype, text, schema string
}

// bodyWords is a JSON request body of string fields.
type bodyWords struct {
	ctype, text string
	fields      []string
}

// words describe one served operation.
type words struct {
	path        string // as the document shows it, with named parameters
	tag         string
	summary     string // at most ten words
	description string
	caller      string // what the handler decides by the caller itself, beyond the rule
	pathWords   map[string]string
	params      []param
	body        *bodyWords
	answers     []reply
}

const (
	maxSearch = 1024
)

func number(n int) *int { return &n }

func query(name, typ, meaning string) param {
	return param{name: name, in: "query", typ: typ, meaning: meaning}
}

func searchParam(name, meaning string) param {
	return param{name: name, in: "query", typ: "string", meaning: meaning, maxLength: maxSearch}
}

// tagWords are the tags in their order, with one line each.
var tagWords = [][2]string{
	{"Status", "Health, version, index scan and the description of the API."},
	{"Content", "Files of the repositories for Maven, npm, Docker and raw clients, and with writes on their uploads and deletes."},
	{"Browse", "Folders, file details and assets for the web interface."},
	{"Repositories", "Repositories and their settings."},
	{"Search", "Search of components."},
	{"Blob Stores", "Blob stores, their sizes and quotas."},
	{"Security", "Privileges, roles, users, realms, anonymous access and LDAP connections."},
	{"Session", "Sign in and sign out of the web interface."},
	{"Tokens", "npm login, whoami and logout, and Docker bearer tokens."},
	{"Not Served", "Operations of the interface contract that this server does not serve yet."},
}

var infoDescription = "Pipesmith Registry serves the files of existing blob stores, with a JSON and REST API for its web interface. " +
	"Every address in this document is relative to the main port of the server. " +
	"A Docker repository also answers the registry API under /v2/ on a port of its own, and on the main port under /repository/<name>/v2/. " +
	"With security on (the data directory holds state.sqlite) a client sends HTTP Basic credentials of a local or LDAP user, a user token as password, or a token in the header Authorization: Bearer; the web interface signs in through POST /api/session and then sends the session cookie. " +
	"npm login and the Docker token route issue tokens; npm logout revokes one. " +
	"Writes are off unless serve starts with --writes or config.json sets writes, and they need state.sqlite; while they are off a write below /repository/ answers 405 and a REST write answers 501 or 404. " +
	"Operations marked with x-pipesmith-needs exist only with what it names: state.sqlite for security on, writes for writes on; without security no permission is checked. " +
	"An operation of the tag Not Served answers 501 with the area it belongs to, after sign in; a method that a path does not list answers 405 with the header Allow. " +
	"Permission strings are shown without the prefix that the imported data carries. " +
	"Answer bodies are described in words in this version."

// pathWords are the meanings of path parameters an entry does not name.
var pathWords = map[string]string{
	"repository": "Name of the repository.",
	"path":       "Path below the repository. It may hold slashes; a folder address ends in a slash.",
	"name":       "Name of the object.",
	"id":         "Id of the object.",
	"userId":     "Id of the user.",
	"segment":    "Format segment of the address.",
	"type":       "Type of the object.",
	"format":     "Name of the format.",
}

var (
	readOffset = param{name: "offset", in: "query", typ: "integer", meaning: "Entries of a folder listing to skip.", def: 0, min: number(0)}

	readParams = []param{readOffset,
		{name: "n", in: "query", typ: "integer", meaning: "Docker tag list below v2/: the most tags to answer; all when absent.", min: number(0)},
		query("last", "string", "Docker tag list below v2/: answer the tags after this one."),
	}

	clientPathWords = map[string]string{"repository": "Name of the repository.",
		"path": "Path below the repository. It may hold slashes; a folder address ends in a slash."}

	clientDenied = []reply{
		{status: "401", ctype: textType, text: "No credentials, and anonymous access does not grant the request. The challenge is Basic; on a Docker path it is Bearer with the token route of the repository while the realm DockerToken is active and the repository takes Docker tokens, and the body is the registry error UNAUTHORIZED."},
		{status: "403", ctype: textType, text: "The caller lacks the view permission of the repository; Docker paths answer the registry error DENIED."},
		{status: "429", text: "Too many failed sign ins of these credentials; the header Retry-After names the seconds to wait."},
	}

	readAnswers = append([]reply{
		{status: "200", text: "The file with its stored content type, ETag and Last-Modified; an npm package document; a registry answer below v2/; or an HTML listing for a folder address. " +
			"With security on a stored file carries X-Content-Type-Options: nosniff and Content-Security-Policy: sandbox. With writes on, GET of -/package/<name>/dist-tags of an npm repository answers the dist-tags of the package."},
		{status: "204", text: "Docker, with writes on: GET of an upload session below blobs/uploads/<uuid> answers its state in Location, Range and Docker-Upload-UUID."},
		{status: "206", text: "Part of the file for a Range request."},
		{status: "302", text: "Redirect to the folder address with a trailing slash."},
		{status: "304", text: "The file has not changed since the time or entity tag of the request."},
		{status: "400", ctype: textType, text: "The path is not UTF-8, holds a NUL byte or an empty, . or .. segment, or the offset is not a number of at least 0; a bad n answers the registry error PAGINATION_NUMBER_INVALID."},
		{status: "404", ctype: textType, text: "No such repository or file."},
		{status: "416", text: "The range lies outside the file."},
		{status: "503", ctype: textType, text: "The blob store of the repository is not available."},
	}, clientDenied...)

	browseAnswers = append([]reply{
		{status: "200", ctype: htmlType, text: "HTML listing of the folder."},
		{status: "302", text: "Redirect to the folder address with a trailing slash."},
		{status: "400", ctype: textType, text: "The path is not UTF-8, holds a NUL byte or an empty, . or .. segment, or the offset is not a number of at least 0."},
		{status: "404", ctype: textType, text: "No such repository or folder; a file path, and a Docker repository, answer 404 as well."},
		{status: "503", ctype: textType, text: "The blob store of the repository is not available."},
	}, clientDenied...)

	sessionAnswer = "{user, permissions, csrfToken, expiresInSeconds}"

	notAllowed = []reply{{status: "405", ctype: jsonType, schema: "Message", text: "Method not allowed. The header Allow names GET, POST, DELETE."}}
)

func readWords(summary string) words {
	return words{path: "/repository/{repository}/{path}", tag: "Content", summary: summary,
		description: "Maven, npm and raw repositories answer files by path. A Maven group merges maven-metadata.xml of its members, " +
			"an npm repository answers package documents whose tarball addresses point at this server, and a Docker repository answers the registry API below v2/. " +
			"A folder address answers an HTML listing of at most 10,000 entries.",
		pathWords: clientPathWords, params: readParams, answers: readAnswers}
}

func browseWords(summary string) words {
	return words{path: "/service/rest/repository/browse/{repository}/{path}", tag: "Browse", summary: summary,
		description: "Folders only. The listing has at most 10,000 entries per page.",
		pathWords:   clientPathWords, params: []param{readOffset}, answers: browseAnswers}
}

func sessionNotAllowed(method string) words {
	return words{path: "/api/session", tag: "Session", summary: method + " is not allowed, answers 405", answers: notAllowed}
}

const restV1 = "/service/rest/v1"

func ok200(text string) reply { return reply{status: "200", ctype: jsonType, text: text} }

func message(status, text string) reply {
	return reply{status: status, ctype: jsonType, schema: "Message", text: text}
}

func errorBody(status, text string) reply { return reply{status: status, ctype: jsonType, text: text} }

// operationWords are the words of every served route and method.
var operationWords = map[string]words{
	"GET /": {path: "/", tag: "Browse", summary: "Web interface or start page",
		description: "With security on the server answers the page of the web interface. Without it the server answers a single start page that reads /api/status and /api/repositories.",
		answers:     []reply{{status: "200", ctype: htmlType, text: "The HTML page."}}},
	"GET /api/health": {path: "/api/health", tag: "Status", summary: "Health of the process",
		answers: []reply{ok200(`{"status":"ok"}`)}},
	"GET /api/status": {path: "/api/status", tag: "Status", summary: "Name, version, stores and scan state",
		caller: "With security on, a caller without valid credentials gets name and version alone. " +
			"A signed in caller without the permission blobstores:read gets data_dir, bind and the root, repos and manifest of each store as null, " +
			"the Docker ports of the repositories it may browse, and a fixed text in place of the error of a failed scan.",
		answers: []reply{ok200("A caller without valid credentials, with security on: {name, version}. " +
			"Every other caller: {name, version, data_dir, bind, port, docker_ports, stores, totals, scan}, with the files and bytes of each store and the state of the index scan. " +
			"With security on, the folders, the bind address and the store settings are null for a caller without blobstores:read.")}},
	"GET /api/openapi.json": {path: "/api/openapi.json", tag: "Status", summary: "This OpenAPI document",
		description: "The document of this build. It is the same as openapi.json of the manual below /manual/.",
		answers: []reply{ok200("The OpenAPI 3.1.0 document, with an ETag."),
			{status: "304", text: "The document has the entity tag of If-None-Match."}}},
	"GET /api/repositories": {path: "/api/repositories", tag: "Repositories", summary: "Repositories with their file counts",
		caller:  "With security on, the list holds only the repositories whose view permission with the action browse the caller holds.",
		answers: []reply{ok200("{repositories}: per repository name, format, type, access kind, Docker port, members, files, bytes, the newest file time and the policies.")}},
	"GET /api/browse/{}": {path: "/api/browse/{repository}", tag: "Browse", summary: "Children of a folder of a repository",
		description: "A group lists the union of its members; on an equal name the first member wins. The parameters are checked before the repository.",
		params: []param{
			query("path", "string", "Folder below the repository, the root when absent. One leading and one trailing slash are dropped."),
			{name: "limit", in: "query", typ: "integer", meaning: "Entries per page.", def: browseLimit, min: number(1), max: number(maxBrowseLimit)},
			{name: "offset", in: "query", typ: "integer", meaning: "Entries to skip.", def: 0, min: number(0)},
		},
		answers: []reply{
			ok200("{repo, path, limit, offset, more, children}; each child has name, path, is_dir, size and created_ms."),
			errorBody("400", `{"error"}: the path holds a NUL byte or an empty, . or .. segment, or limit or offset is not a number in range.`),
			errorBody("404", `{"error"}: no such repository, or the path is not a folder of it.`),
		}},
	"GET /api/asset/{}": {path: "/api/asset/{repository}", tag: "Browse", summary: "Details of one file",
		description: "A group answers from the first member that holds the path.",
		params:      []param{{name: "path", in: "query", typ: "string", meaning: "Path of the file below the repository.", required: true}},
		answers: []reply{
			ok200("{repo, member, path, name, size, sha1, content_type, created_ms, store, access, download_url, maven, docker, npm}; asset_id with security on."),
			errorBody("400", `{"error"}: the path is missing, holds a NUL byte or an empty, . or .. segment.`),
			errorBody("404", `{"error"}: no such repository, or no such file in it.`),
		}},
	"GET /api/export/{}": {path: "/api/export/{repository}", tag: "Browse", summary: "Download a folder or a Docker image as one archive",
		description: "The form follows the format of the repository: maven2 gives a zip in repository layout, npm a zip of the package tarballs, " +
			"docker one image as a tar in OCI image layout, every other format a zip of the files below the folder. " +
			"An export holds at most 20,000 files and 2 GiB, counted from the index before the first byte. " +
			"The archive has no Content-Length; an archive that ends early breaks the transfer.",
		caller: "The server streams two exports at a time and one per account; a further export that is not dry answers 429. A dry call takes no place.",
		params: []param{
			query("path", "string", "Folder below the repository, the root when absent; for the form oci the manifest path v2/<image>/manifests/<tag or digest>. One leading and one trailing slash are dropped."),
			{name: "form", in: "query", typ: "string", meaning: "The form of the archive; it must be the form of the format of the repository, which is the default.",
				enum: []string{"maven", "zip", "npm", "oci"}},
			query("platform", "string", "Form oci with an index: one stored platform as os/arch or os/arch/variant."),
			{name: "dry", in: "query", typ: "string", meaning: "1 answers the counts as JSON and sends no archive; 0, empty or absent sends the archive.",
				enum: []string{"0", "1"}},
		},
		answers: []reply{
			{status: "200", text: "The archive as application/zip, or application/x-tar for oci, with Content-Disposition: attachment and Cache-Control: no-store. " +
				"With dry=1 the JSON {repository, path, form, name, files, bytes, exact, max_files, max_bytes, busy, own_export}, retry_after when busy, and for oci image and platforms."},
			errorBody("400", `{"message"}: a bad path, dry or form, a form that does not fit the repository, or for oci an index with several stored platforms and no platform, or a platform not stored; then also platforms.`),
			errorBody("404", `{"message"}: no such repository or folder, nothing the form exports, or for oci no manifest path, no stored manifest or no stored platform.`),
			errorBody("409", `{"message", "path"}: a stored path that no archive entry may carry, an entry name twice or beside a folder of its name, or for oci a manifest that gives no image or a blob that is not stored.`),
			errorBody("413", `{"message", "files", "bytes", "max_files", "max_bytes", "complete"}: the export passes a limit; the message names both numbers and both limits.`),
			errorBody("429", `{"message"}: an export of this account streams already, or two exports stream; the header Retry-After names 30 seconds.`),
		}},
	"POST /api/scan": {path: "/api/scan", tag: "Status", summary: "Start a scan of one blob store",
		description: "The scan reads the blob store and writes the index.",
		caller:      "The handler also requires the header X-Artifact-Repository: 1, a Host of this server with the port of the main listener, and an Origin of this server when one is sent.",
		params:      []param{{name: "X-Artifact-Repository", in: "header", typ: "string", meaning: "Must be 1.", required: true}},
		body:        &bodyWords{ctype: jsonType, text: "The blob store to scan, at most 64 KiB.", fields: []string{"store"}},
		answers: []reply{
			{status: "202", ctype: jsonType, text: "The scan started; the body is the scan state."},
			errorBody("400", `{"error"}: the body is not JSON or names no store.`),
			{status: "403", ctype: jsonType, text: `{"error"} for a missing header, a Host or an Origin of another server; {"message"} for a missing permission or request token.`},
			errorBody("404", `{"error"}: the store is not attached.`),
			errorBody("409", `{"error", "scan"}: a scan is running.`),
			errorBody("413", `{"error"}: the body is larger than 64 KiB. The server reads and drops up to 16 MiB of the body first; past that the connection closes.`),
		}},
	"GET /service/rest/v1/status": {path: restV1 + "/status", tag: "Status", summary: "Liveness probe",
		answers: []reply{{status: "200", text: "Empty body."}}},
	"GET /service/rest/v1/status/writable": {path: restV1 + "/status/writable", tag: "Status", summary: "Write probe",
		answers: []reply{{status: "200", text: "Empty body: writes are on and the write store takes a write now."},
			{status: "503", text: "Empty body: writes are off, a folder of the write store cannot be written, or the delete mark of a replaced blob waits to be tried again."}}},
	"GET /repository/{*}":  readWords("Download a file or list a folder"),
	"HEAD /repository/{*}": readWords("Headers of a file or folder"),
	"PUT /repository/{*}": writeWords("Deploy or upload a file, publish to npm, push to Docker", []reply{
		{status: "200", ctype: jsonType, text: `npm, with writes on: {"ok":true,"id","rev"} for a deprecate, a new revision of a document or a dist-tag.`},
		{status: "201", text: "With writes on: the file is stored; an npm publish answers {\"ok\":true,\"id\",\"rev\"}; Docker answers Location and Docker-Content-Digest."},
	}),
	"POST /repository/{*}": writeWords("Start or finish a Docker blob upload, or mount a blob", []reply{
		{status: "201", text: "Docker, with writes on: the blob of ?digest= or ?mount= is stored."},
		{status: "202", text: "Docker, with writes on: an upload session started; Location, Range and Docker-Upload-UUID name it."},
		{status: "429", ctype: jsonType, text: "Docker, with writes on: too many upload sessions are open, the registry error TOOMANYREQUESTS with Retry-After: 60; or too many failed sign ins, with Retry-After."},
	}),
	"DELETE /repository/{*}": writeWords("Delete a file, an npm version or tag, a Docker manifest", []reply{
		{status: "200", ctype: jsonType, text: `npm, with writes on: {"ok":true,...} for an unpublish or a dist-tag delete.`},
		{status: "202", text: "Docker, with writes on: the manifest or blob is marked deleted."},
		{status: "204", text: "With writes on: a raw or Maven file is marked deleted; Docker: the upload session ended."},
	}),
	"PATCH /repository/{*}": writeWords("Append a chunk to a Docker blob upload", []reply{
		{status: "202", text: "Docker, with writes on: the chunk is stored; Range names the bytes held."},
		{status: "416", ctype: jsonType, text: "Docker, with writes on: the chunk does not start at the bytes the upload holds; the registry error BLOB_UPLOAD_INVALID."},
	}),
	"GET /service/rest/repository/browse/{*}":  browseWords("HTML listing of a folder"),
	"HEAD /service/rest/repository/browse/{*}": browseWords("Headers of a folder listing"),

	"GET /api/session": {path: "/api/session", tag: "Session", summary: "Session of the cookie",
		description: "Only the cookie counts. Without a live session the answer holds no user and no permissions, and a cookie of no live session is cleared.",
		caller:      "The handler reads the session cookie itself.",
		answers:     []reply{ok200(sessionAnswer)}},
	"POST /api/session": {path: "/api/session", tag: "Session", summary: "Sign in and start a session",
		description: "Local users are tried first, then the LDAP directories. The answer sets the cookie " + sessionCookie +
			" (HttpOnly, SameSite=Strict) and holds the request token for the header " + csrfHeader + " of later writes.",
		caller: "The handler checks the user name and password itself.",
		body:   &bodyWords{ctype: jsonType, text: "User name and password, at most 8 KiB.", fields: []string{"username", "password"}},
		answers: []reply{
			ok200(sessionAnswer),
			message("400", "The body is not a JSON object with username and password."),
			message("401", "Wrong user name or password."),
			message("413", "The body is larger than 8 KiB."),
			message("415", "The body is not application/json."),
			{status: "429", text: "Too many failed attempts; the header Retry-After names the seconds to wait."},
			message("500", "No session could be started."),
		}},
	"DELETE /api/session": {path: "/api/session", tag: "Session", summary: "Sign out",
		description: "With a live session the header " + csrfHeader + " must carry its request token.",
		caller:      "The handler checks the request token of the session itself.",
		params:      []param{{name: csrfHeader, in: "header", typ: "string", meaning: "The csrfToken of the session."}},
		answers: []reply{
			{status: "204", text: "The session ended and the cookie is cleared."},
			message("403", "The request token is missing or wrong; the session stays."),
		}},
	"HEAD /api/session":    sessionNotAllowed("HEAD"),
	"PUT /api/session":     sessionNotAllowed("PUT"),
	"PATCH /api/session":   sessionNotAllowed("PATCH"),
	"OPTIONS /api/session": sessionNotAllowed("OPTIONS"),

	"GET /service/rest/v1/repositories": {path: restV1 + "/repositories", tag: "Repositories", summary: "Repositories the caller may browse",
		caller:  "The list holds only the repositories whose view permission with the action browse the caller holds.",
		answers: []reply{ok200("Array of {name, format, type, url, attributes}. The remote address of a proxy is shown without its user information.")}},
	"GET /service/rest/v1/repositories/{}": {path: restV1 + "/repositories/{name}", tag: "Repositories", summary: "One repository",
		pathWords: map[string]string{"name": "Name of the repository."},
		answers:   []reply{ok200("{name, format, type, url, attributes}. The remote address of a proxy is shown without its user information."), {status: "404"}}},
	"GET /service/rest/v1/repositories/{}/{}/{}": {path: restV1 + "/repositories/{segment}/{type}/{name}", tag: "Repositories", summary: "Settings of one repository",
		description: "Secret fields of the stored settings are left out, and every address is shown without its user information.",
		pathWords: map[string]string{"segment": "maven for a maven2 repository, else the format.", "type": "hosted, proxy or group.",
			"name": "Name of the repository."},
		answers: []reply{ok200("The settings with name, format, type, url and online."), {status: "404"}}},
	"GET /service/rest/v1/repositorySettings": {path: restV1 + "/repositorySettings", tag: "Repositories", summary: "Settings of every administered repository",
		caller: "The list holds the repositories whose administration permission the caller holds.",
		answers: []reply{ok200("Array of settings, as one repository answers them."),
			message("403", "The caller administers no repository.")}},
	"GET /service/rest/v1/search": {path: restV1 + "/search", tag: "Search", summary: "Search components",
		description: "50 components per page, ordered by repository, group, name and version. Each parameter may appear once, with at most 1024 bytes. " +
			"The parameters that are not served yet answer 501.",
		caller: "An unknown repository, and one the caller may not browse, give an empty list.",
		params: append([]param{
			searchParam("q", "Words that each occur in group, name, version or path; * is a wildcard; case is ignored."),
			searchParam("repository", "Name of a repository; a group searches its members."),
			searchParam("format", "Format of the repositories."),
			searchParam("group", "Group of the component."),
			searchParam("name", "Name of the component."),
			searchParam("version", "Version of the component."),
			searchParam("sha1", "SHA-1 of an asset."),
			searchParam("maven.groupId", "Maven group id."),
			searchParam("maven.artifactId", "Maven artifact id."),
			searchParam("maven.baseVersion", "Maven base version."),
			searchParam("maven.extension", "Extension of a Maven file."),
			searchParam("maven.classifier", "Classifier of a Maven file."),
			searchParam("docker.imageName", "Docker image name."),
			searchParam("docker.imageTag", "Docker image tag."),
			searchParam("npm.scope", "npm scope; a leading @ is dropped."),
			searchParam("continuationToken", "Token of the next page from the answer before."),
		}, deferredSearch...),
		answers: []reply{
			ok200("{items, continuationToken}; each item has id, repository, format, group, name, version and assets."),
			message("400", "Unknown, repeated or too long parameter, a value that is no UTF-8 or holds a NUL byte, or a continuationToken of another search."),
			{status: "501", ctype: jsonType, schema: "Refusal", text: "A parameter this server does not answer yet; the area is search."},
			message("503", "The search is not ready, or a page took longer than two minutes."),
		}},
	"GET /service/rest/v1/assets/{}": {path: restV1 + "/assets/{id}", tag: "Browse", summary: "One asset by id",
		pathWords: map[string]string{"id": "Asset id: base64url without padding of <repository>:<path>."},
		answers: []reply{ok200("{downloadUrl, path, id, repository, format, checksum, contentType, fileSize, blobCreated, blobStoreName}."),
			{status: "404"}, message("422", "The asset id is malformed.")}},
	"GET /service/rest/v1/blobstores": {path: restV1 + "/blobstores", tag: "Blob Stores", summary: "Blob stores with counts and sizes",
		answers: []reply{ok200("Array of {name, type, unavailable, blobCount, totalSizeInBytes, availableSpaceInBytes, softQuota}, sorted by name.")}},
	"GET /service/rest/v1/blobstores/file/{}": {path: restV1 + "/blobstores/file/{name}", tag: "Blob Stores", summary: "Path and quota of a file blob store",
		pathWords: map[string]string{"name": "Name of the blob store."},
		answers:   []reply{ok200("{path, softQuota}."), {status: "404"}}},
	"GET /service/rest/v1/blobstores/{}/quota-status": {path: restV1 + "/blobstores/{name}/quota-status", tag: "Blob Stores", summary: "Quota state of a blob store",
		pathWords: map[string]string{"name": "Name of the blob store."},
		answers:   []reply{ok200("{isViolation, message, blobStoreName}."), {status: "404"}}},
	"GET /service/rest/v1/security/privileges": {path: restV1 + "/security/privileges", tag: "Security", summary: "Privileges",
		answers: []reply{ok200("Array of privileges with type, name, description, readOnly, the fields of the type and actions, sorted by name.")}},
	"GET /service/rest/v1/security/privileges/{}": {path: restV1 + "/security/privileges/{name}", tag: "Security", summary: "One privilege",
		pathWords: map[string]string{"name": "Name of the privilege."},
		answers:   []reply{ok200("The privilege."), {status: "404"}}},
	"GET /service/rest/v1/security/roles": {path: restV1 + "/security/roles", tag: "Security", summary: "Roles",
		params: []param{query("source", "string", "default or absent for the local roles; LDAP answers 501.")},
		answers: []reply{ok200("Array of {id, source, name, description, readOnly, privileges, roles}, sorted by id."),
			{status: "404"},
			{status: "501", ctype: jsonType, schema: "Refusal", text: "The groups of an LDAP directory are not read yet; the area is security."}}},
	"GET /service/rest/v1/security/roles/{}": {path: restV1 + "/security/roles/{id}", tag: "Security", summary: "One role",
		pathWords: map[string]string{"id": "Id of the role."},
		answers:   []reply{ok200("The role."), {status: "404"}}},
	"GET /service/rest/v1/security/user-sources": {path: restV1 + "/security/user-sources", tag: "Security", summary: "Sources of users",
		answers: []reply{ok200("Array of {id, name}: the local users, and LDAP when a connection is stored.")}},
	"GET /service/rest/v1/security/users": {path: restV1 + "/security/users", tag: "Security", summary: "Users",
		params: []param{query("source", "string", "Source of the users, matched exactly."),
			query("userId", "string", "Part of the user id; case is ignored.")},
		answers: []reply{ok200("Array of {userId, firstName, lastName, emailAddress, source, status, readOnly, roles, externalRoles}.")}},
	"GET /service/rest/v1/security/anonymous": {path: restV1 + "/security/anonymous", tag: "Security", summary: "Anonymous access",
		answers: []reply{ok200("{enabled, userId, realmName}.")}},
	"GET /service/rest/v1/security/realms/available": {path: restV1 + "/security/realms/available", tag: "Security", summary: "Realms this server knows",
		answers: []reply{ok200("Array of {id, name}: local users and LDAP directories.")}},
	"GET /service/rest/v1/security/realms/active": {path: restV1 + "/security/realms/active", tag: "Security", summary: "Active realms",
		answers: []reply{ok200("Array of realm ids in their stored order.")}},
	"GET /service/rest/v1/security/ldap": {path: restV1 + "/security/ldap", tag: "Security", summary: "LDAP connections",
		answers: []reply{ok200("Array of connections in their order, without bind passwords.")}},
	"GET /service/rest/v1/security/ldap/{}": {path: restV1 + "/security/ldap/{name}", tag: "Security", summary: "One LDAP connection",
		pathWords: map[string]string{"name": "Name of the connection."},
		answers:   []reply{ok200("The connection, without its bind password."), {status: "404"}}},
	"GET /service/rest/v1/status/check": {path: restV1 + "/status/check", tag: "Status", summary: "Health checks of blob stores and index",
		answers: []reply{ok200("Object with the checks Blob Stores and Index, each {healthy, message, error, details, time, duration, timestamp}.")}},
	"GET /service/rest/v1/read-only": {path: restV1 + "/read-only", tag: "Status", summary: "Read only state",
		answers: []reply{ok200("{frozen, systemInitiated, summaryReason}; frozen is always true, with writes on as well.")}},
	"GET /api/areas": {path: "/api/areas", tag: "Status", summary: "Read only state and refused operations",
		answers: []reply{ok200("{readOnly, refused}; refused lists method, path and area of every operation of the tag Not Served, " +
			"and with writes off also of the three writes of the area content that answer 501 then.")}},

	// the routes of tokens (tokens.go); for a repository of another access
	// kind they hand the request to the routes of step 2 (passOn)
	"PUT /repository/{}/-/user/org.couchdb.user:{+}": {path: "/repository/{repository}/-/user/org.couchdb.user:{user}", tag: "Tokens",
		summary:     "npm login: check a password, issue an npm token",
		description: "The answer is the one place that shows the token. " + passOnNote,
		caller:      "The handler checks the user name and password itself, through the realms of the stored order, and counts a failure in the sign in throttle.",
		pathWords:   map[string]string{"repository": "Name of an npm repository.", "user": "User name; it must equal name of the body."},
		body:        &bodyWords{ctype: jsonType, text: "User name and password, at most 64 KiB.", fields: []string{"name", "password"}},
		answers: firstOfEach([]reply{
			{status: "201", ctype: jsonType, text: `npm: {"ok":true,"id":"org.couchdb.user:<name>","token":"<npm token>"}. Another repository: the answers of a write below /repository/.`},
			{status: "400", ctype: jsonType, text: `npm: {"ok":false,"error"}, the body is no JSON object with name and password, or its name is not the name of the path.`},
			{status: "401", ctype: jsonType, text: `npm: {"ok":false,"error":"wrong user name or password"}, with a Basic challenge.`},
			{status: "403", ctype: jsonType, text: `npm: {"ok":false,"error"}, npm tokens are off because the realm NpmToken is not active.`},
			{status: "413", ctype: jsonType, text: `npm: {"ok":false,"error"}, the body is larger than 64 KiB.`},
			{status: "429", ctype: jsonType, text: `npm: {"ok":false,"error"}, too many failed sign ins; the header Retry-After names the seconds to wait.`},
		}, passOnWrite)},
	"GET /repository/{}/-/whoami": {path: "/repository/{repository}/-/whoami", tag: "Tokens",
		summary:     "npm whoami: the user of the credentials",
		description: passOnNote,
		caller:      "The handler resolves the caller itself from Basic credentials, a bearer token or the session cookie. Without credentials and with anonymous access on it names the anonymous user.",
		pathWords:   map[string]string{"repository": "Name of an npm repository."},
		answers: firstOfEach([]reply{
			{status: "200", ctype: jsonType, text: `npm: {"username":"<user id>"}. Another repository: the answers of a read below /repository/.`},
			{status: "401", text: "npm: no credentials while anonymous access is off, or credentials that do not sign in."},
			{status: "429", text: "Too many failed sign ins of these credentials; the header Retry-After names the seconds to wait."},
		}, readAnswers)},
	"DELETE /repository/{}/-/user/token/{+}": {path: "/repository/{repository}/-/user/token/{token}", tag: "Tokens",
		summary:     "npm logout: revoke an npm or user token",
		description: "The access log shows - in place of the token. " + passOnNote,
		caller:      "The handler asks for a signed in caller and revokes a token of that caller only. With the session cookie the request token is checked.",
		// #nosec G101 -- the words that describe the path parameter token in the OpenAPI document, no credential.
		pathWords: map[string]string{"repository": "Name of an npm repository.", "token": "The whole npm or user token."},
		answers: firstOfEach([]reply{
			{status: "200", ctype: jsonType, text: `npm: {"ok":true}, the token is revoked.`},
			{status: "401", text: "No credentials, or credentials that do not sign in."},
			{status: "403", text: "npm: the request token of the session is missing or wrong. Another repository: as a write below /repository/."},
			{status: "404", text: `npm: {"ok":false,"error":"no such token"}, the token is malformed, a Docker token, of another user, or revoked. Another repository: as a write below /repository/.`},
		}, passOnWrite)},
	"GET /repository/{}/v2/token": {path: "/repository/{repository}/v2/token", tag: "Tokens",
		summary: "Docker bearer token for a repository",
		description: "A Docker port answers the same at /v2/token. The token is valid 300 seconds and lives in the memory of the process. " +
			"The query parameters scope and service decide nothing: the rights of the user are checked at every request. " + passOnNote,
		caller:    "The handler checks Basic credentials itself. Without an Authorization header it issues a token of the anonymous user while the repository allows anonymous pull.",
		pathWords: map[string]string{"repository": "Name of a Docker repository."},
		answers: firstOfEach([]reply{
			{status: "200", ctype: jsonType, text: `Docker: {"token", "access_token", "expires_in": 300, "issued_at"} with Cache-Control: no-store. Another repository: the answers of a read below /repository/.`},
			{status: "401", ctype: jsonType, text: "Docker: the registry error UNAUTHORIZED with a Basic challenge, for no credentials without anonymous pull, another scheme than Basic, or a wrong pair."},
			{status: "403", ctype: jsonType, text: "Docker: the registry error DENIED, Docker bearer tokens are off because the realm DockerToken is not active."},
			{status: "429", ctype: jsonType, text: "Docker: the registry error TOOMANYREQUESTS; the header Retry-After names the seconds to wait."},
		}, readAnswers)},

	// the REST writes of contentWriteRoutes (rest_write.go, rest_upload.go)
	"GET /service/rest/v1/formats/upload-specs": {path: restV1 + "/formats/upload-specs", tag: "Content", summary: "Upload fields of every format that takes uploads",
		answers: []reply{ok200("Array of {format, multipleUpload, componentFields, assetFields} for maven2, npm and raw.")}},
	"GET /service/rest/v1/formats/{}/upload-specs": {path: restV1 + "/formats/{format}/upload-specs", tag: "Content", summary: "Upload fields of one format",
		pathWords: map[string]string{"format": "maven2, npm or raw."},
		answers: []reply{ok200("{format, multipleUpload, componentFields, assetFields}."),
			message("404", "The format takes no upload.")}},
	"POST /service/rest/v1/components": {path: restV1 + "/components", tag: "Content", summary: "Upload a component of maven2, npm or raw",
		description: "The body is multipart/form-data with the fields <format>.<field> and the files <format>.assetN that GET /service/rest/v1/formats/upload-specs lists; " +
			"at most 3 files. Every file is read before one is written. A maven2 upload writes the .md5 and .sha1 files of each file and then the metadata of the artifact.",
		caller: "The handler asks the action edit as well for a path the repository holds already.",
		params: []param{{name: "repository", in: "query", typ: "string", meaning: "Name of a hosted repository of maven2, npm or raw.", required: true}},
		answers: []reply{
			{status: "204", text: "Every file is written."},
			{status: "400", ctype: jsonType, text: `A list [{"id","message"}] of field errors and refusals of a file, or {"message"} for a group, a proxy, an offline repository, a format without upload or a write policy that refuses.`},
			message("403", "The caller lacks the action, a write of a session lacks the request token, or the write comes from a page of another origin."),
			message("404", "The repository is not known."),
			message("408", "The body did not arrive in full; nothing is written."),
			message("409", "A path lies in a store of the existing install, or the repository names another write store."),
			message("413", "A file passes the limit of the repository."),
			message("415", "The body is not multipart/form-data with its boundary."),
			message("422", "The query parameter repository is missing, empty or given more than once."),
			message("503", "The index is busy; the header Retry-After names the seconds to wait."),
			message("507", "The disk of the write store would keep less than its free space floor."),
		}},
	"DELETE /service/rest/v1/assets/{}": {path: restV1 + "/assets/{id}", tag: "Content", summary: "Delete one asset",
		pathWords: map[string]string{"id": "Asset id: base64url without padding of <repository>:<path>."},
		answers: []reply{{status: "204", text: "The asset is marked deleted."},
			message("400", "The repository is a group, a proxy or offline, or a rule of the repository refuses the delete."),
			message("404", "Asset not found."),
			message("409", "The path lies in a store of the existing install."),
			message("422", "The asset id is malformed."),
			message("503", "The index is busy; the header Retry-After names the seconds to wait.")}},
	"DELETE /service/rest/v1/components/{}": {path: restV1 + "/components/{id}", tag: "Content", summary: "Delete one component",
		description: "maven2 and raw: every file of the component, and a maven2 component has the metadata of its artifact written again. npm: one version. Docker: a manifest by tag or digest.",
		pathWords:   map[string]string{"id": "Component id of the search."},
		answers: []reply{{status: "204", text: "Every asset of the component is marked deleted."},
			message("400", "The repository is a group, a proxy or offline, or a rule of the repository refuses the delete."),
			message("404", "Component not found."),
			message("409", "A path lies in a store of the existing install."),
			message("422", "The component id is malformed."),
			message("503", "The index is busy; the header Retry-After names the seconds to wait.")}},
}

// passOnNote is what a route of tokens does for a repository of another
// access kind.
const passOnNote = "For a repository of another access kind, or a name no repository has, the request goes on to the routes of /repository/ and gets their answers."

// passOnWrite are the answers of a write below /repository/ that a route of
// tokens hands on.
var passOnWrite = firstOfEach(writeAnswers, clientDenied)

// firstOfEach keeps the first answer of each status, so that an entry can
// give a status of a shared list its own text.
func firstOfEach(lists ...[]reply) []reply {
	var out []reply
	seen := map[string]bool{}
	for _, list := range lists {
		for _, a := range list {
			if !seen[a.status] {
				seen[a.status] = true
				out = append(out, a)
			}
		}
	}
	return out
}

// writeAnswers are the answers of a write below /repository/ that every
// method shares: the refusal of writes off, and the refusals of writes on.
// Their 401 stands before the one of clientDenied: no write without
// credentials (allowClient, secure.go).
var writeAnswers = []reply{
	{status: "401", ctype: textType, text: "No credentials, and anonymous access does not grant the request; with writes on a write without credentials is always asked for them, whatever the anonymous user holds. " +
		"The challenge is Basic; on a Docker path it is Bearer with the token route of the repository while the realm DockerToken is active and the repository takes Docker tokens, " +
		"its scope names pull for GET and HEAD, pull,push for POST, PUT and PATCH, delete for DELETE, and pull of the source repository of a mount, and the body is the registry error UNAUTHORIZED."},
	{status: "400", text: "With writes on: a refusal of the path or of the repository, such as a folder path, a control character, " +
		"the write policy, the version or layout policy or a bad npm document; Docker paths answer a registry error such as DIGEST_INVALID, MANIFEST_INVALID or NAME_INVALID."},
	{status: "403", text: "The caller lacks the view permission of the repository with the action of the write (add for a new path, edit for one that exists, delete), " +
		"a write of a session lacks the request token, or the write comes from a page of another origin; Docker paths answer the registry error DENIED."},
	{status: "404", text: `Writes off: below the path - of an npm repository {"error":"not found"}. ` +
		"With writes on: npm, no such package document or tarball; Docker, an unknown upload, manifest or blob, or a path that is no call of the registry protocol; raw and Maven, a delete of a path no store holds."},
	{status: "405", text: "Writes are off, or the repository is a group, a proxy, offline, of a format without writes or not known, or the method does not fit the path. " +
		"The header Allow names what does, GET, HEAD for a repository without writes; the text Method not allowed: this server is read only, or below v2/ of a Docker repository the registry error UNSUPPORTED."},
	{status: "408", text: "With writes on: the body stopped arriving; nothing is written."},
	{status: "409", text: "With writes on: the path lies in a store of the existing install, which is read only, or the repository names another write store; npm, the write names a stale revision."},
	{status: "413", text: "With writes on: the body passes the limit of the repository, a Docker manifest its limit, or an npm document the size the read path serves."},
	{status: "503", text: "With writes on: the index is busy; the header Retry-After names the seconds to wait."},
	{status: "507", text: "With writes on: the disk of the write store would keep less than its free space floor."},
}

// writeWords are the words of a write below /repository/, with the answers
// of its method first.
func writeWords(summary string, own []reply) words {
	return words{path: "/repository/{repository}/{path}", tag: "Content", summary: summary,
		description: "With writes off every write is refused as before writes existed. With writes on, a hosted repository of maven2, raw, npm or docker " +
			"that is online takes the write into the write store local-write of the data directory; other repositories keep the refusal. The rule of the read path applies first.",
		pathWords: clientPathWords,
		params: []param{
			query("digest", "string", "Docker, with writes on: digest of the whole blob, on the closing PUT or a monolithic POST."),
			query("mount", "string", "Docker, with writes on: digest of a blob to mount from another repository."),
			query("from", "string", "Docker, with writes on: the repository the mount reads from."),
		},
		answers: firstOfEach(own, writeAnswers, clientDenied)}
}

// deferredSearch are the search parameters the server reads and refuses
// with 501.
var deferredSearch = func() []param {
	var out []param
	for _, name := range []string{"md5", "sha256", "sha512", "npm.author", "npm.description", "npm.keywords", "npm.license",
		"docker.layerId", "docker.contentDigest", "sort", "direction", "prerelease", "timeout", "gavec", "lastBlobUpdated"} {
		out = append(out, query(name, "string", "Not served yet: answers 501 with the area search."))
	}
	return out
}()
