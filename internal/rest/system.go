package rest

import (
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/admin"
)

// checkXO is one entry of GET /v1/status/check.
type checkXO struct {
	Healthy   bool    `json:"healthy"`
	Message   *string `json:"message"`
	Error     any     `json:"error"`
	Details   any     `json:"details"`
	Time      int64   `json:"time"`
	Duration  int64   `json:"duration"`
	Timestamp string  `json:"timestamp"`
}

func timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func check(start time.Time, healthy bool, message string) checkXO {
	return checkXO{Healthy: healthy, Message: &message, Time: start.UnixMilli(), Duration: time.Since(start).Milliseconds(),
		Timestamp: timestamp(start)}
}

// StatusChecks is GET /v1/status/check with the two checks this server
// can make: every attached store can be read, and the index answers.
func StatusChecks(env *Env) (Answer, error) {
	start := time.Now()
	var missing []string
	for _, s := range env.Stores {
		if !s.Available {
			missing = append(missing, s.Name)
		}
	}
	stores := check(start, len(missing) == 0, "every attached store can be read")
	if len(missing) > 0 {
		stores = check(start, false, "not available: "+strings.Join(missing, ", "))
	}
	start = time.Now()
	index := check(start, true, "the index answers")
	if _, err := env.Index.StoreStats(); err != nil {
		index = check(start, false, "the index does not answer: "+err.Error())
	}
	return ok(map[string]checkXO{"Blob Stores": stores, "Index": index})
}

type readOnlyXO struct {
	Frozen          bool   `json:"frozen"`
	SystemInitiated bool   `json:"systemInitiated"`
	SummaryReason   string `json:"summaryReason"`
}

// writesOffNote ends the reason of a released server that runs with writes
// off: its content writes stay refused.
const writesOffNote = " Uploads and other content writes stay refused: this server runs with writes off."

func readOnlyOf(s admin.ReadOnlyState, writesOff bool) readOnlyXO {
	reason := s.Reason
	if !s.Frozen && writesOff {
		reason += writesOffNote
	}
	return readOnlyXO{Frozen: s.Frozen, SystemInitiated: s.SystemInitiated, SummaryReason: reason}
}

// ReadOnly is GET /v1/read-only: the state of the read only mode (ruling
// SB2). A server with writes off is not frozen; its reason says that its
// content writes stay refused.
func ReadOnly(s admin.ReadOnlyState, writesOff bool) Answer {
	return Answer{Status: 200, Body: readOnlyOf(s, writesOff)}
}

// FreezeChange is a change of the read only mode: Freeze, Release or
// ForceRelease of admin.ReadOnlyMode.
type FreezeChange func() error

// ChangeReadOnly is POST /v1/read-only/freeze, /release and /force-release:
// 204 after the change, 409 for a change the state does not allow. The
// change ends in the hook of a change (admin.Act).
func ChangeReadOnly(store admin.IdentityStore, change FreezeChange) (Answer, error) {
	if err := admin.Act(store, change); err != nil {
		return refused(err)
	}
	return noContent()
}

// Refused is one request the server answers with 501: its method, its path
// in the spelling of docs/ui/API.md and the area of the 501.
type Refused struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Area   string `json:"area"`
}

type areasXO struct {
	ReadOnly areasReadOnlyXO `json:"readOnly"`
	Refused  []Refused       `json:"refused"`
}

type areasReadOnlyXO struct {
	Frozen bool   `json:"frozen"`
	Reason string `json:"reason"`
}

// Areas is GET /api/areas (docs/ui/API.md, section Areas): the read only
// state of ReadOnly and the requests the server refuses.
func Areas(s admin.ReadOnlyState, writesOff bool, refused []Refused) Answer {
	x := readOnlyOf(s, writesOff)
	return Answer{Status: 200, Body: areasXO{ReadOnly: areasReadOnlyXO{Frozen: x.Frozen, Reason: x.SummaryReason}, Refused: refused}}
}
