package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"

	"github.com/iogentic/pipesmith-registry/internal/cleanup"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/rest"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// The cleanup policies of leg 6-api-b (contract S11, S14 and S15, ruling
// CP1) with security on, writes on or off: the reads with
// nexus:settings:read, the create, save and delete with
// nexus:settings:update through the identity store, as the writes of the
// area security. Every write stays behind freezeGate. The preview needs
// nexus:settings:read and the browse permission of its repository, writes
// nothing, and is served while the server is frozen (unfrozen).
// internal/rest builds the answers.

const policiesBase = "/api/cleanup-policies"

// policyRoutes are the requests of the cleanup policies in the table with
// security on, writes on or off.
var policyRoutes = []route{
	entry(exact(policiesBase+"/preview"), []string{"POST"}, audited(perm(settingsRead), "cleanup-policies.preview"),
		(*Server).policyPreview),
	get(policiesBase, perm(settingsRead), plain(rest.CleanupPolicies)),
	get(policiesBase+"/"+segment, perm(settingsRead), named(rest.CleanupPolicy)),
	identityRoute("POST", policiesBase, settingsUpdate,
		func(_ *rest.Env, w rest.IdentityWrite, _ []string, _ url.Values, body []byte) (rest.Answer, error) {
			return rest.CreateCleanupPolicy(w, body)
		}),
	identityRoute("PUT", policiesBase+"/"+segment, settingsUpdate,
		func(_ *rest.Env, w rest.IdentityWrite, m []string, _ url.Values, body []byte) (rest.Answer, error) {
			return rest.SaveCleanupPolicy(w, m[1], body)
		}),
	identityRoute("DELETE", policiesBase+"/"+segment, settingsUpdate,
		func(_ *rest.Env, w rest.IdentityWrite, m []string, _ url.Values, _ []byte) (rest.Answer, error) {
			return rest.DeleteCleanupPolicy(w, m[1])
		}),
}

// previewSlot is the one cleanup preview a server runs at a time (ruling
// CP8), the field preview of Server. run is set only by tests and then
// takes the place of CleanupPreview.
type previewSlot struct {
	busy atomic.Bool
	run  atomic.Pointer[previewRun]
}

// previewRun is the dry run a preview calls.
type previewRun func(ctx context.Context, policy model.CleanupPolicy, repository string) (cleanup.Preview, error)

func (s *Server) previewSlot() *previewSlot { return &s.preview }

// take reports whether the slot was free and is taken now.
func (p *previewSlot) take() bool { return p.busy.CompareAndSwap(false, true) }

// give frees the slot.
func (p *previewSlot) give() { p.busy.Store(false) }

// errPreviewBusy is the refusal of a preview while another one runs.
var errPreviewBusy = errors.New("another cleanup preview runs; try again")

// policyPreview is POST /api/cleanup-policies/preview: the dry run of the
// sent policy through CleanupPreview, one at a time per server (ruling
// CP8). Its audit line has the action cleanup-policies.preview, set at the
// route, so a refused preview has it too (CP11). The server without state
// answers 503, a failure of the run 500 with its fixed text; a request
// whose client left gets no answer.
func (s *Server) policyPreview(w *recorder, req *request, _ []string) error {
	body, err := io.ReadAll(io.LimitReader(req.r.Body, maxIdentityBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxIdentityBody {
		s.message(w, req, http.StatusRequestEntityTooLarge, "the body is larger than "+strconv.Itoa(maxIdentityBody)+" bytes")
		return nil
	}
	env, err := s.restEnv(req)
	if err != nil {
		return err
	}
	ctx := req.r.Context()
	a, err := rest.PreviewCleanupPolicy(env, body, func(policy model.CleanupPolicy, repository string) (cleanup.Preview, error) {
		slot := s.previewSlot()
		if !slot.take() {
			return cleanup.Preview{}, errPreviewBusy
		}
		defer slot.give()
		if run := slot.run.Load(); run != nil {
			return (*run)(ctx, policy, repository)
		}
		return s.CleanupPreview(ctx, policy, repository)
	})
	var failure *tasks.Failure
	switch {
	case err == nil:
		return s.writeAnswer(w, req, a)
	case ctx.Err() != nil:
		return nil
	case errors.Is(err, errPreviewBusy):
		s.message(w, req, http.StatusTooManyRequests, errPreviewBusy.Error(), [2]string{"Retry-After", "5"})
	case errors.Is(err, errCleanupNoState):
		s.message(w, req, http.StatusServiceUnavailable, errCleanupNoState.Error())
	case errors.As(err, &failure):
		s.message(w, req, http.StatusInternalServerError, failure.Text)
	default:
		return err
	}
	return nil
}
