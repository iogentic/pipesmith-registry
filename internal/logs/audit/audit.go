// Package audit holds what the lines of audit.log are made of: the detail a
// handler adds to the line of its request, the action, the object and the
// result a request gets by default, and the rule for secret fields.
package audit

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// Subject is the subject of the line of a command.
const Subject = "cli"

// The results of a line.
const (
	OK     = "ok"
	Denied = "denied"
	Failed = "failed"
)

// textCap bounds subject, realm, client, action, object type and object
// name of a line, as the small fields of a request line (docs/LOGGING.md).
const (
	textCap = 256
	cutMark = "[cut]"
)

// Detail is what a handler knows about its request and the chain does not:
// the action, the user of a sign in, the object. It is safe for use by
// several goroutines.
type Detail struct {
	mu                     sync.Mutex
	action                 string
	subject, realm         string
	hasSubject             bool
	objectType, objectName string
	hasObject              bool
}

type key struct{}

// With returns ctx with a new Detail, and the Detail.
func With(ctx context.Context) (context.Context, *Detail) {
	d := &Detail{}
	return context.WithValue(ctx, key{}, d), d
}

func from(ctx context.Context) *Detail {
	d, _ := ctx.Value(key{}).(*Detail)
	return d
}

// SetAction names the action of the line of the request of ctx, as
// <domain>.<verb>. Without a Detail in ctx it does nothing.
func SetAction(ctx context.Context, action string) {
	if d := from(ctx); d != nil {
		d.mu.Lock()
		d.action = action
		d.mu.Unlock()
	}
}

// SetSubject names the user and the realm of the line of the request of
// ctx, for a request that signs a user in.
func SetSubject(ctx context.Context, subject, realm string) {
	if d := from(ctx); d != nil {
		d.mu.Lock()
		d.subject, d.realm, d.hasSubject = subject, realm, true
		d.mu.Unlock()
	}
}

// SetObject names the object of the line of the request of ctx.
func SetObject(ctx context.Context, objectType, objectName string) {
	if d := from(ctx); d != nil {
		d.mu.Lock()
		d.objectType, d.objectName, d.hasObject = objectType, objectName, true
		d.mu.Unlock()
	}
}

// Fill puts what was set on d into e, over what e holds.
func (d *Detail) Fill(e *logs.AuditEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.action != "" {
		e.Action = d.action
	}
	if d.hasSubject {
		e.Subject, e.Realm = d.subject, d.realm
	}
	if d.hasObject {
		e.ObjectType, e.ObjectName = d.objectType, d.objectName
	}
}

// Result is the result of a line by the status of the answer: ok below 400,
// denied for 401, 403 and 429, failed for every other status.
func Result(status int) string {
	switch {
	case status < 400:
		return OK
	case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusTooManyRequests:
		return Denied
	}
	return Failed
}

// Verb is the verb of a method: create for POST, update for PUT and PATCH,
// delete for DELETE, else the method in lower case.
func Verb(method string) string {
	switch method {
	case http.MethodPost:
		return "create"
	case http.MethodPut, http.MethodPatch:
		return "update"
	case http.MethodDelete:
		return "delete"
	}
	return strings.ToLower(method)
}

// Default is the action, object type and object name of a request that no
// handler named: below /repository/ the domain content and the object
// <repository>/<path>; below /service/rest/<version>/ and /api/ the first
// segment as domain and the path as object; else the domain server.
func Default(method, path string) (action, objectType, objectName string) {
	domain := "server"
	switch rest, found := strings.CutPrefix(path, "/repository/"); {
	case found:
		return "content." + Verb(method), "path", rest
	case strings.HasPrefix(path, "/service/rest/"):
		parts := strings.SplitN(strings.TrimPrefix(path, "/service/rest/"), "/", 3)
		if len(parts) > 1 && parts[1] != "" {
			domain = parts[1]
		}
	case strings.HasPrefix(path, "/api/"):
		if first, _, _ := strings.Cut(strings.TrimPrefix(path, "/api/"), "/"); first != "" {
			domain = first
		}
	}
	return domain + "." + Verb(method), "path", path
}

// Capped is e with each text field cut to 256 bytes at a character
// boundary, the mark [cut] included, and with Fields never nil.
func Capped(e logs.AuditEvent) logs.AuditEvent {
	for _, field := range []*string{&e.RequestID, &e.Subject, &e.Realm, &e.Client, &e.Action, &e.ObjectType, &e.ObjectName} {
		*field = capText(*field)
	}
	if e.Fields == nil {
		e.Fields = []string{}
	}
	return e
}

func capText(text string) string {
	if len(text) <= textCap {
		return text
	}
	end := textCap - len(cutMark)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + cutMark
}
