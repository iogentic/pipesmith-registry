package auth

import (
	"context"

	"github.com/iogentic/pipesmith-registry/internal/logs/audit"
)

// auditName is the subject of the audit line of a sign in that failed: the
// id of the local user the typed name names, else audit.Unknown. A user of
// a directory is not looked up, so its failed sign in is unknown as well.
func auditName(v *view, typed string) string {
	if u, known := v.localUser(userName(typed)); known {
		return u.ID
	}
	return audit.Unknown
}

// setRefused names the subject of the audit line of a refused pair.
func setRefused(ctx context.Context, v *view, typed string) {
	audit.SetSubject(ctx, auditName(v, typed), "")
}
