package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/version"
)

// The names of the session cookie and of the request token header, as
// internal/auth has them. The server package does not import auth, so they
// stand here a second time, and a test compares them with auth.
const (
	sessionCookie = "pipesmith_session"
	csrfHeader    = "X-Pipesmith-Csrf"
)

// The OpenAPI document of the server, built from the route tables and the
// words of openapi_words.go.

// apiRoute is one method of one route as the document shows it.
type apiRoute struct {
	method   string
	template string // generic template of a route, or the path of a refusal
	pattern  *regexp.Regexp
	rule     rule
	secured  bool   // exists only with security on
	writes   bool   // exists only with writes on (contentWriteRoutes)
	refused  bool   // a refusal of refusalTable, or of writeRefusals that no route of the writes serves
	area     string // of a refusal

	// offArea and offPattern are the refusal of writeRefusals that answers
	// the same method and path while writes are off; empty and nil when no
	// route answers it then.
	offArea    string
	offPattern *regexp.Regexp
}

// key is the key of the route in operationWords.
func (r apiRoute) key() string { return r.method + " " + r.template }

// apiRoutes is the one reader of the route tables: every route and method of
// routes, secureRoutes and contentWriteRoutes, then every refusal of
// writeRefusals and refusalTable per method and path. These are the parts
// that securedRoutes and writingRoutes are made of (secure.go), so the
// document covers each table that (*Server).table() returns. A route of the
// writes carries the refusal of writeRefusals with the same method and
// pattern, which answers in its place while writes are off; such a refusal is
// not listed a second time. Leg F2 moves the tables to internal/route; this
// function is the one place to change then.
func apiRoutes() ([]apiRoute, error) {
	var out []apiRoute
	add := func(table []route, secured, writes bool) error {
		for _, rt := range table {
			template, err := genericTemplate(rt.Pattern)
			if err != nil {
				return err
			}
			for _, method := range rt.Methods {
				r := apiRoute{method: method, template: template, pattern: rt.Pattern, rule: rt.Rule, secured: secured, writes: writes}
				if writes {
					r.offArea, r.offPattern = writeRefusalOf(method, rt.Pattern)
				}
				out = append(out, r)
			}
		}
		return nil
	}
	for _, t := range []struct {
		table           []route
		secured, writes bool
	}{{routes, false, false}, {secureRoutes, true, false}, {identityRoutes, true, false}, {systemRoutes, true, false}, {statusAreaRoutes, true, false}, {figureRoutes, true, false}, {logRoutes, true, false}, {storeRoutes, true, false}, {proxyRoutes, true, false}, {contentWriteRoutes, true, true}} {
		if err := add(t.table, t.secured, t.writes); err != nil {
			return nil, err
		}
	}
	for _, d := range slices.Concat(writeRefusals, refusalTable) {
		rt := d.route()
		for _, method := range d.methods {
			if servedWrite(out, method, rt.Pattern) {
				continue
			}
			for _, path := range d.paths() {
				out = append(out, apiRoute{method: method, template: path, pattern: rt.Pattern, rule: rt.Rule,
					secured: true, refused: true, area: d.area})
			}
		}
	}
	return out, nil
}

// writeRefusalOf is the area and pattern of the declaration of writeRefusals
// with the method and the pattern of a route of the writes.
func writeRefusalOf(method string, p *regexp.Regexp) (string, *regexp.Regexp) {
	for _, d := range writeRefusals {
		if rp := d.pattern(); rp.String() == p.String() && slices.Contains(d.methods, method) {
			return d.area, rp
		}
	}
	return "", nil
}

// servedWrite reports a route of the writes with the method and pattern of
// a refusal.
func servedWrite(read []apiRoute, method string, p *regexp.Regexp) bool {
	for _, r := range read {
		if r.writes && r.method == method && r.pattern.String() == p.String() {
			return true
		}
	}
	return false
}

// genericTemplate is the path of a pattern with {} for a group of one
// segment, ([^/]+), {*} for a group of the rest of the path, (.*), and {+}
// for a group of one character or more, (.+). Any other group or regular
// expression form is an error, so that it cannot reach the document as
// regular expression text.
func genericTemplate(p *regexp.Regexp) (string, error) {
	s := strings.TrimPrefix(p.String(), "^")
	s = strings.TrimSuffix(s, "$")
	s = strings.TrimSuffix(s, `\n?`)
	var b strings.Builder
	for s != "" {
		known := false
		for _, form := range [][2]string{{segment, "{}"}, {"(.*)", "{*}"}, {"(.+)", "{+}"}, {`\.`, "."}} {
			if strings.HasPrefix(s, form[0]) {
				b.WriteString(form[1])
				s, known = s[len(form[0]):], true
				break
			}
		}
		if known {
			continue
		}
		if strings.ContainsRune(`\()[]{}?*+|^$.`, rune(s[0])) {
			return "", fmt.Errorf("the route pattern %s holds %q, a form the route reader does not know", p, s)
		}
		b.WriteByte(s[0])
		s = s[1:]
	}
	return b.String(), nil
}

// permissionPrefix is the product prefix of the imported permission
// strings, read from one of them; the document shows the strings without it.
var permissionPrefix = strings.SplitAfter(settingsRead, ":")[0]

// permissionText is what the rule asks of the caller, in words.
func permissionText(r rule) string {
	switch r.kind {
	case rulePublic:
		return "public"
	case ruleSignedIn:
		return "signed in"
	case ruleApp:
		shown := make([]string, len(r.permissions))
		for i, p := range r.permissions {
			shown[i] = strings.TrimPrefix(p, permissionPrefix)
			if shown[i] == "*" {
				shown[i] = "* (every permission)"
			}
		}
		return strings.Join(shown, " or ")
	case ruleView:
		return "view of the repository (" + r.action + ")"
	case ruleAdmin:
		if r.action != "" {
			return "administration of the repository (" + r.action + ")"
		}
		return "administration of the repository"
	case ruleAssetID:
		return "view of the repository of the asset (browse)"
	case ruleCreate:
		return "administration of any repository of the format of the path (add)"
	case ruleClient:
		return "view of the repository (read below /repository/, browse below the browse route)"
	case ruleContent, ruleListing:
		if r.group > 0 {
			return "view of the repository of the id (" + r.action + ")"
		}
		return "view of the repository of the query parameter repository (" + r.action + ")"
	}
	return ""
}

// kv and obj are a JSON object with its keys in a fixed order.
type kv struct {
	k string
	v any
}

type obj []kv

func (o obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := plainJSON(p.k)
		if err != nil {
			return nil, err
		}
		value, err := plainJSON(p.v)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func plainJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// methodOrder is the order of the methods in a path item.
var methodOrder = []string{"GET", "PUT", "POST", "DELETE", "OPTIONS", "HEAD", "PATCH"}

// sharedAnswers are the answers in components.responses, by status.
var sharedAnswers = map[string]string{"401": "Unauthorized", "403": "Forbidden", "404": "NotFound", "429": "TooManyRequests", "501": "NotServed"}

// sharedTexts are the descriptions of the shared answers, by status.
var sharedTexts = map[string]string{
	"401": "No credentials, wrong credentials or a session that has ended. The header WWW-Authenticate names Basic, or Session for /api/ and browser requests.",
	"403": "The caller lacks the permission, or a write of a session lacks the request token.",
	"404": "The named object does not exist.",
	"429": "Too many failed sign ins of these credentials; the header Retry-After names the seconds to wait, and the body is empty.",
	"501": "This server does not serve the operation yet. The body names the area.",
}

var nonWord = regexp.MustCompile(`[^A-Za-z0-9]+`)

func operationID(method, path string) string {
	name := strings.Trim(nonWord.ReplaceAllString(path, "_"), "_")
	if name == "" {
		name = "root"
	}
	return strings.ToLower(method) + "_" + name
}

func schemaRef(name string) obj { return obj{{"$ref", "#/components/schemas/" + name}} }

func answerRef(status string) obj {
	return obj{{"$ref", "#/components/responses/" + sharedAnswers[status]}}
}

func (a reply) build() any {
	if a.text == "" {
		return answerRef(a.status)
	}
	out := obj{{"description", a.text}}
	if a.ctype != "" {
		media := obj{}
		if a.schema != "" {
			media = obj{{"schema", schemaRef(a.schema)}}
		}
		out = append(out, kv{"content", obj{{a.ctype, media}}})
	}
	return out
}

func (p param) build() obj {
	schema := obj{{"type", p.typ}}
	if p.def != nil {
		schema = append(schema, kv{"default", p.def})
	}
	if p.min != nil {
		schema = append(schema, kv{"minimum", *p.min})
	}
	if p.max != nil {
		schema = append(schema, kv{"maximum", *p.max})
	}
	if p.maxLength > 0 {
		schema = append(schema, kv{"maxLength", p.maxLength})
	}
	if len(p.enum) > 0 {
		schema = append(schema, kv{"enum", p.enum})
	}
	out := obj{{"name", p.name}, {"in", p.in}, {"description", p.meaning}}
	if p.required || p.in == "path" {
		out = append(out, kv{"required", true})
	}
	return append(out, kv{"schema", schema})
}

// pathParams declares every {name} of a path.
func pathParams(path string, words map[string]string) []param {
	var out []param
	for _, m := range regexp.MustCompile(`\{([^/{}]+)\}`).FindAllStringSubmatch(path, -1) {
		meaning := words[m[1]]
		if meaning == "" {
			meaning = pathWords[m[1]]
		}
		if meaning == "" {
			meaning = "One segment of the path."
		}
		out = append(out, param{name: m[1], in: "path", typ: "string", meaning: meaning})
	}
	return out
}

// operation is the operation object of one route and method.
func operation(r apiRoute, w words) obj {
	tag, summary, description := w.tag, w.summary, w.description
	answers := w.answers
	if r.refused {
		tag, summary = "Not Served", "Not served yet"
		description = "Part of the area " + r.area + ". After sign in the server answers 501 with the area."
		answers = []reply{{status: "501"}}
	}
	if r.writes {
		description = strings.TrimSpace(description + " " + writesOnly(r))
		answers = withWritesOff(answers, writesOffAnswer(r))
	}
	if w.caller != "" {
		description = strings.TrimSpace(description + " " + w.caller)
	}
	op := obj{{"tags", []string{tag}}, {"summary", summary}}
	if description != "" {
		op = append(op, kv{"description", description})
	}
	op = append(op, kv{"operationId", operationID(r.method, w.path)})
	var params []obj
	for _, p := range append(pathParams(w.path, w.pathWords), w.params...) {
		params = append(params, p.build())
	}
	if len(params) > 0 {
		op = append(op, kv{"parameters", params})
	}
	if w.body != nil {
		props := obj{}
		for _, f := range w.body.fields {
			props = append(props, kv{f, obj{{"type", "string"}}})
		}
		op = append(op, kv{"requestBody", obj{{"description", w.body.text}, {"required", true},
			{"content", obj{{w.body.ctype, obj{{"schema", obj{{"type", "object"}, {"required", w.body.fields}, {"properties", props}}}}}}}}})
	}
	typed := map[string]bool{}
	for _, a := range answers {
		typed[a.status] = true
	}
	if !r.refused {
		switch r.rule.kind {
		case ruleSignedIn, ruleApp, ruleView, ruleAdmin, ruleAssetID, ruleContent, ruleCreate, ruleListing:
			if !typed["401"] {
				answers = append(answers, reply{status: "401"})
			}
		}
		switch r.rule.kind {
		case ruleApp, ruleView, ruleAdmin, ruleAssetID, ruleContent, ruleCreate, ruleListing:
			if !typed["403"] {
				answers = append(answers, reply{status: "403"})
			}
		}
	} else if r.rule.kind != rulePublic {
		// allow runs before the refusal: no credentials get 401.
		answers = append(answers, reply{status: "401"})
	}
	if r.rule.kind != rulePublic && !typed["429"] {
		answers = append(answers, reply{status: "429"})
	}
	sort.SliceStable(answers, func(i, j int) bool { return answers[i].status < answers[j].status })
	responses := obj{}
	for _, a := range answers {
		responses = append(responses, kv{a.status, a.build()})
	}
	op = append(op, kv{"responses", responses})
	if r.rule.kind != rulePublic {
		op = append(op, kv{"security", []obj{{{"basic", []string{}}}, {{"session", []string{}}}}})
	}
	op = append(op, kv{"x-pipesmith-permission", permissionText(r.rule)})
	if w.caller != "" {
		op = append(op, kv{"x-pipesmith-caller", w.caller})
	}
	switch {
	case r.writes:
		op = append(op, kv{"x-pipesmith-needs", "state.sqlite, writes"})
	case r.secured:
		op = append(op, kv{"x-pipesmith-needs", "state.sqlite"})
	}
	if r.refused {
		op = append(op, kv{"x-pipesmith-refused", true}, kv{"x-pipesmith-area", r.area})
	}
	return op
}

// writesOnly is the sentence of an operation that exists only with writes
// on, and what the same request gets with writes off.
func writesOnly(r apiRoute) string {
	off := "answers 404, as no route serves the path then"
	if r.offArea != "" {
		off = "answers 501 with the area " + r.offArea + " after sign in, as before writes existed"
	}
	return "This operation exists while writes are on (serve --writes, or writes in config.json, with state.sqlite). With writes off the same request " + off + "."
}

// writesOffAnswer is the answer of an operation of the writes while writes
// are off.
func writesOffAnswer(r apiRoute) reply {
	if r.offArea != "" {
		return reply{status: "501", ctype: jsonType, schema: "Refusal",
			text: "With writes off: not served, after sign in; the body names the area " + r.offArea + "."}
	}
	return reply{status: "404", ctype: jsonType, text: `With writes off: no route serves the path; the body is {"detail":"Not Found"}.`}
}

// withWritesOff adds the answer of writes off to the answers of writes on;
// a status both list gets one answer with both texts.
func withWritesOff(answers []reply, off reply) []reply {
	out := slices.Clone(answers)
	for i, a := range out {
		if a.status != off.status {
			continue
		}
		text := a.text
		if text == "" {
			text = sharedTexts[a.status]
		}
		out[i] = reply{status: a.status, ctype: off.ctype, text: "With writes on: " + text + " " + off.text}
		return out
	}
	return append(out, off)
}

// buildOpenAPI writes the document for a version: JSON indented by two
// spaces with a final newline, the same bytes on every call.
func buildOpenAPI(version string) ([]byte, error) {
	items := map[string]map[string]obj{}
	table, err := apiRoutes()
	if err != nil {
		return nil, err
	}
	for _, r := range table {
		w, ok := operationWords[r.key()]
		if r.refused {
			w = words{path: r.template}
		} else if !ok {
			return nil, fmt.Errorf("the route %s has no words", r.key())
		}
		if items[w.path] == nil {
			items[w.path] = map[string]obj{}
		}
		if _, twice := items[w.path][r.method]; twice {
			return nil, fmt.Errorf("%s %s is described twice", r.method, w.path)
		}
		items[w.path][r.method] = operation(r, w)
	}
	names := make([]string, 0, len(items))
	for path := range items {
		names = append(names, path)
	}
	sort.Strings(names)
	paths := obj{}
	for _, path := range names {
		item := obj{}
		for _, method := range methodOrder {
			if op, ok := items[path][method]; ok {
				item = append(item, kv{strings.ToLower(method), op})
			}
		}
		paths = append(paths, kv{path, item})
	}
	var tags []obj
	for _, t := range tagWords {
		tags = append(tags, obj{{"name", t[0]}, {"description", t[1]}})
	}
	doc := obj{
		{"openapi", "3.1.0"},
		{"info", obj{{"title", "Pipesmith Registry API"}, {"version", version}, {"description", infoDescription},
			{"license", obj{{"name", "Apache License 2.0"}, {"identifier", "Apache-2.0"}}}}},
		{"servers", []obj{{{"url", "/"}}}},
		{"tags", tags},
		{"paths", paths},
		{"components", obj{
			{"securitySchemes", obj{
				{"basic", obj{{"type", "http"}, {"scheme", "basic"}, {"description", "HTTP Basic credentials of a local user."}}},
				{"session", obj{{"type", "apiKey"}, {"in", "cookie"}, {"name", sessionCookie},
					{"description", "Cookie of POST /api/session. A write sends the csrfToken of the session in the header " + csrfHeader + "."}}},
			}},
			{"schemas", obj{
				{"Message", obj{{"type", "object"}, {"required", []string{"message"}},
					{"properties", obj{{"message", obj{{"type", "string"}}}}}}},
				{"Refusal", obj{{"type", "object"}, {"required", []string{"message", "area"}},
					{"properties", obj{{"message", obj{{"type", "string"}}}, {"area", obj{{"type", "string"}}}}}}},
			}},
			{"responses", obj{
				{"Unauthorized", sharedAnswer(sharedTexts["401"], "Message")},
				{"Forbidden", sharedAnswer(sharedTexts["403"], "Message")},
				{"NotFound", sharedAnswer(sharedTexts["404"], "Message")},
				{"TooManyRequests", obj{{"description", sharedTexts["429"]}}},
				{"NotServed", sharedAnswer(sharedTexts["501"], "Refusal")},
			}},
		}},
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func sharedAnswer(text, schema string) obj {
	return obj{{"description", text}, {"content", obj{{jsonType, obj{{"schema", schemaRef(schema)}}}}}}
}

// openAPIServed is the document of this build, built once at package start.
var openAPIServed struct {
	data []byte
	etag string
	err  error
}

// init builds the document; the route tables are complete by then. The
// handler cannot build it at its first call, because routes holds the
// handler and the document reads routes.
func init() {
	openAPIServed.data, openAPIServed.err = buildOpenAPI(version.Version)
	sum := sha256.Sum256(openAPIServed.data)
	openAPIServed.etag = `"` + hex.EncodeToString(sum[:16]) + `"`
}

// openAPI is GET /api/openapi.json: the document, 304 for a matching
// If-None-Match.
func (s *Server) openAPI(w *recorder, req *request, _ []string) error {
	if openAPIServed.err != nil {
		return openAPIServed.err
	}
	headers := [][2]string{{"X-Content-Type-Options", "nosniff"}, {"Cache-Control", "no-cache"}, {"ETag", openAPIServed.etag}}
	for _, tag := range strings.Split(req.r.Header.Get("If-None-Match"), ",") {
		if tag = strings.TrimPrefix(strings.TrimSpace(tag), "W/"); tag == openAPIServed.etag || tag == "*" {
			writeBody(w, req, http.StatusNotModified, headers, nil)
			return nil
		}
	}
	writeBody(w, req, http.StatusOK, append([][2]string{{"Content-Type", jsonType}}, headers...), openAPIServed.data)
	return nil
}
