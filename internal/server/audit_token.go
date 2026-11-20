package server

import (
	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/logs/audit"
)

// auditDockerToken starts the audit line of the token route of the Docker
// flow for a request with credentials (L5, finding 7b-4): one line per
// issued token and per refused pair, with the action token.issue, the
// repository as object, realm and address. The route is a GET, which
// dispatch gives no line. A request without credentials, an anonymous
// pull, writes none. The subject is audit.Unknown until the service names
// the user.
func (s *Server) auditDockerToken(w *recorder, req *request, repository string) {
	if s.audit == nil || w.audit != nil || req.r.Header.Get("Authorization") == "" {
		return
	}
	ctx, detail := audit.With(req.r.Context())
	req.r = req.r.WithContext(ctx)
	audit.SetSubject(ctx, audit.Unknown, "")
	w.audit = &pendingAudit{req: req, detail: detail, event: logs.AuditEvent{Action: "token.issue", ObjectType: "repository",
		ObjectName: repository}}
}
