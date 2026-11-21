package server

import (
	"net/http"

	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/logs/audit"
	rtable "github.com/iogentic/pipesmith-registry/internal/route"
)

// The audit line of a request (L5, task 7.15): dispatch starts it for a
// route that took a state changing method, the recorder writes it when the
// answer starts, before the header goes out, with the result of the status.
// A deploy, a write below /repository/ other than DELETE, writes none: the
// request log holds it (L5). A handler names action, user or object through
// the audit.Detail in the context of the request.

// pendingAudit is the line of one request until its answer starts.
type pendingAudit struct {
	req    *request
	detail *audit.Detail
	event  logs.AuditEvent
	done   bool
}

// deploy reports a write of content below /repository/ through the client
// route: PUT, POST or PATCH.
func deploy(rt *route, method string) bool {
	return rt.Rule.kind == ruleClient && method != http.MethodDelete
}

// auditRequest starts the audit line of a request that rt took, when it
// changes state and is no deploy. The function it returns writes the line
// with 200 once the handler returned without error and without an answer;
// an error leaves it to the answer of fail. A request that a handler passes
// on to another table keeps the line of its first route.
func (s *Server) auditRequest(w *recorder, req *request, rt *route, err *error) func() {
	if s.audit == nil || rt == nil || w.audit != nil || !rtable.ChangesState([]string{req.r.Method}) || deploy(rt, req.r.Method) {
		return noop
	}
	ctx, detail := audit.With(req.r.Context())
	req.r = req.r.WithContext(ctx)
	action, objectType, objectName := audit.Default(req.r.Method, logs.MaskTarget(maskToken(req.path)))
	if rt.Rule.audit != "" {
		action = rt.Rule.audit
	}
	w.audit = &pendingAudit{req: req, detail: detail, event: logs.AuditEvent{Action: action, ObjectType: objectType,
		ObjectName: objectName}}
	return func() {
		if *err == nil {
			w.writeAudit(http.StatusOK)
		}
	}
}

// writeAudit writes the pending audit line once, with the result of status.
// A line that does not reach its file is counted by the files (L6).
func (w *recorder) writeAudit(status int) {
	p := w.audit
	if p == nil || p.done {
		return
	}
	p.done = true
	e := p.event
	if n := noteOf(w.r); n != nil {
		e.RequestID = n.id
	} else {
		e.RequestID = newRequestID()
	}
	e.Client = clientHost(w.client, w.r.RemoteAddr)
	if p.req.subject != nil {
		e.Subject, e.Realm = p.req.subject.User.ID, p.req.subject.Realm
	}
	p.detail.Fill(&e)
	e.Result = audit.Result(status)
	_ = w.s.audit(audit.Capped(e))
}
