package rest

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/cleanup"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The cleanup policies of leg 6-api-b (contract S11, S14 and S15, rulings
// CP2 to CP13): the list, one policy, create, save and delete in the shape
// CleanupPolicy. A change goes through admin.Change with the checks of
// rulings R1, R2, R4, R6, R11 and R11b. The server checks the permission
// and the request token before it calls a function here.

// policyXO is CleanupPolicy with usedBy, the repositories that name it.
type policyXO struct {
	Name     string         `json:"name"`
	Format   string         `json:"format"`
	Notes    string         `json:"notes"`
	Criteria map[string]any `json:"criteria"`
	UsedBy   []string       `json:"usedBy"`
}

// policyView is the answer of a stored policy: both known criteria, null
// for an absent one, and a stored criterion of another name as stored
// (ruling CP2).
func policyView(s *model.Snapshot, p model.CleanupPolicy) policyXO {
	criteria := maps.Clone(p.Criteria)
	if criteria == nil {
		criteria = map[string]any{}
	}
	for _, name := range []string{admin.CriterionLastBlobUpdatedDays, admin.CriterionLastDownloadedDays} {
		if _, ok := criteria[name]; !ok {
			criteria[name] = nil
		}
	}
	usedBy := admin.PolicyUsers(s, p.Name)
	if usedBy == nil {
		usedBy = []string{}
	}
	return policyXO{Name: p.Name, Format: p.Format, Notes: p.Notes, Criteria: criteria, UsedBy: usedBy}
}

// storedPolicies are the policies of the snapshot, never nil.
func storedPolicies(s *model.Snapshot) []model.CleanupPolicy {
	if s == nil {
		return nil
	}
	return s.CleanupPolicies
}

// CleanupPolicies is GET /api/cleanup-policies: every policy, sorted by
// name.
func CleanupPolicies(env *Env) (Answer, error) {
	list := slices.Clone(storedPolicies(env.Snapshot))
	slices.SortFunc(list, func(a, b model.CleanupPolicy) int { return strings.Compare(a.Name, b.Name) })
	out := make([]policyXO, 0, len(list))
	for _, p := range list {
		out = append(out, policyView(env.Snapshot, p))
	}
	return ok(out)
}

// CleanupPolicy is GET /api/cleanup-policies/{name}: the policy, or 404.
func CleanupPolicy(env *Env, name string) (Answer, error) {
	list := storedPolicies(env.Snapshot)
	at := slices.IndexFunc(list, func(p model.CleanupPolicy) bool { return p.Name == name })
	if at < 0 {
		return refused(policyNotFound(name))
	}
	return ok(policyView(env.Snapshot, list[at]))
}

func policyNotFound(name string) error {
	return &admin.Refusal{Code: admin.CodeNotFound, Message: "no cleanup policy named " + name}
}

// policyKinds are the kinds a change of a cleanup policy writes.
var policyKinds = []state.Kind{state.KindCleanupPolicies}

// maxNotes is the bound of the notes of a cleanup policy, in characters
// (ruling CP12, the column of the old product).
const maxNotes = 400

// policyBody is the body of a create or save. A key the body lacks is nil;
// usedBy and every other key are ignored.
type policyBody struct {
	Name     *string        `json:"name"`
	Format   *string        `json:"format"`
	Notes    *string        `json:"notes"`
	Criteria map[string]any `json:"criteria"`
}

// checkNotes refuses new notes above maxNotes characters with 400 at notes;
// the stored notes are never refused (ruling R6).
func checkNotes(notes string, stored *string) error {
	if utf8.RuneCountInString(notes) <= maxNotes || stored != nil && notes == *stored {
		return nil
	}
	return &admin.Refusal{Code: admin.CodeNotesBound, Field: "notes", Message: fmt.Sprintf(
		"the notes of a cleanup policy hold at most %d characters", maxNotes)}
}

// keptCriteria are the criteria a create or save stores: every sent
// criterion but a known one that is null (ruling CP2). A criterion of
// another name reaches this only when CheckCriteria let it pass, that is
// with its stored value (R11b).
func keptCriteria(sent map[string]any) map[string]any {
	out := map[string]any{}
	for name, value := range sent {
		if value == nil && (name == admin.CriterionLastBlobUpdatedDays || name == admin.CriterionLastDownloadedDays) {
			continue
		}
		out[name] = value
	}
	return out
}

// savedCriteria are the criteria of a save: the sent ones and every stored
// criterion of another name the body does not send (ruling C1), so a save
// that sends only the known keys keeps a criterion of an import and the
// policy stays not runnable (A9).
func savedCriteria(sent, stored map[string]any) map[string]any {
	out := maps.Clone(sent)
	if out == nil {
		out = map[string]any{}
	}
	for name, value := range stored {
		if _, ok := out[name]; !ok && name != admin.CriterionLastBlobUpdatedDays && name != admin.CriterionLastDownloadedDays {
			out[name] = value
		}
	}
	return out
}

func decodePolicy(body []byte) (policyBody, error) {
	var b policyBody
	err := decode(body, '{', &b)
	return b, err
}

func valueOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// CreateCleanupPolicy is POST /api/cleanup-policies: 201 with the view of
// the new policy, of origin local. The name, the format, the notes and the
// criteria are checked together; a name that exists answers 409.
func CreateCleanupPolicy(w IdentityWrite, body []byte) (Answer, error) {
	b, err := decodePolicy(body)
	if err != nil {
		return refused(err)
	}
	created := model.CleanupPolicy{Name: valueOf(b.Name), Format: valueOf(b.Format), Notes: valueOf(b.Notes),
		Criteria: keptCriteria(b.Criteria), Origin: model.OriginLocal}
	after, err := admin.Change(w.Store, policyKinds, func(s *model.Snapshot) error {
		exists := slices.ContainsFunc(s.CleanupPolicies, func(p model.CleanupPolicy) bool { return p.Name == created.Name })
		if err := errors.Join(admin.CheckCreate(admin.KindCleanupPolicy, created.Name, exists),
			admin.CheckFormatValue(created.Format, nil), checkNotes(created.Notes, nil),
			admin.CheckCriteria(b.Criteria, nil)); err != nil {
			return err
		}
		s.CleanupPolicies = append(slices.Clone(s.CleanupPolicies), created)
		return nil
	})
	if err != nil {
		return refused(err)
	}
	return Answer{Status: http.StatusCreated, Body: policyView(after, created)}, nil
}

// SaveCleanupPolicy is PUT /api/cleanup-policies/{name}: 204 after the
// save. The name cannot change; an absent format keeps the stored one; a
// value the stored policy holds is never refused (R6, R11b); a stored
// criterion of another name the body does not send is kept (C1). The
// criteria are compared with the stored ones as stored, a known one that
// is null as absent, so the view of GET sent back is never refused (F1).
// The saved policy is of origin local, so a later import keeps it (ruling
// CP5).
func SaveCleanupPolicy(w IdentityWrite, name string, body []byte) (Answer, error) {
	b, err := decodePolicy(body)
	if err != nil {
		return refused(err)
	}
	_, err = admin.Change(w.Store, policyKinds, func(s *model.Snapshot) error {
		at := slices.IndexFunc(s.CleanupPolicies, func(p model.CleanupPolicy) bool { return p.Name == name })
		if at < 0 {
			return policyNotFound(name)
		}
		stored := s.CleanupPolicies[at]
		format := stored.Format
		if b.Format != nil {
			format = *b.Format
		}
		notes := valueOf(b.Notes)
		criteria := savedCriteria(b.Criteria, stored.Criteria)
		formatErr := admin.CheckFormatValue(format, &stored.Format)
		if formatErr == nil {
			formatErr = admin.CheckPolicyFormat(s, name, stored.Format, format)
		}
		if err := errors.Join(admin.CheckRename(admin.KindCleanupPolicy, name, b.Name), formatErr,
			checkNotes(notes, &stored.Notes), admin.CheckCriteria(keptCriteria(criteria), keptCriteria(stored.Criteria))); err != nil {
			return err
		}
		s.CleanupPolicies = slices.Clone(s.CleanupPolicies)
		s.CleanupPolicies[at] = model.CleanupPolicy{Name: name, Format: format, Notes: notes,
			Criteria: keptCriteria(criteria), Origin: model.OriginLocal}
		return nil
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}

// DeleteCleanupPolicy is DELETE /api/cleanup-policies/{name}: 204 after the
// delete, of any origin, 409 while a repository names the policy (R4). A
// cleanup task that runs keeps the policies it read at its start.
func DeleteCleanupPolicy(w IdentityWrite, name string) (Answer, error) {
	_, err := admin.Change(w.Store, policyKinds, func(s *model.Snapshot) error {
		if !slices.ContainsFunc(s.CleanupPolicies, func(p model.CleanupPolicy) bool { return p.Name == name }) {
			return policyNotFound(name)
		}
		if err := admin.CheckPolicyDelete(s, name); err != nil {
			return err
		}
		s.CleanupPolicies = slices.DeleteFunc(slices.Clone(s.CleanupPolicies), func(p model.CleanupPolicy) bool { return p.Name == name })
		return nil
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}

// previewBody is the body of POST /api/cleanup-policies/preview: the sent
// policy and the name of one repository.
type previewBody struct {
	Policy     policyBody `json:"policy"`
	Repository string     `json:"repository"`
}

// PreviewRun is the dry run of a policy over one repository:
// (*server.Server).CleanupPreview.
type PreviewRun func(policy model.CleanupPolicy, repository string) (cleanup.Preview, error)

// PreviewCleanupPolicy is POST /api/cleanup-policies/preview: what a run
// of the sent policy removes from the repository, at most
// cleanup.PreviewLimit items with the total (ruling CL11). The checks of
// the policy answer at policy.<field>, the check of the target at
// repository, together in one 400 list (ruling CP6). A repository the
// subject may not browse is checked as one that does not exist (ruling
// CP7). The name of the policy is not checked, and no stored policy is
// read. An error of run that holds no refusal is returned.
func PreviewCleanupPolicy(env *Env, body []byte, run PreviewRun) (Answer, error) {
	var b previewBody
	if err := decode(body, '{', &b); err != nil {
		return refused(err)
	}
	policy := model.CleanupPolicy{Name: valueOf(b.Policy.Name), Format: valueOf(b.Policy.Format), Criteria: b.Policy.Criteria}
	if policy.Criteria == nil {
		policy.Criteria = map[string]any{}
	}
	visible := model.Snapshot{}
	if env.Snapshot != nil {
		visible.Repositories = slices.DeleteFunc(slices.Clone(env.Snapshot.Repositories), func(r model.Repository) bool {
			return !env.permits(ViewPermission(r.Format, r.Name, "browse"))
		})
	}
	err := errors.Join(admin.PreviewPolicy(errors.Join(admin.CheckFormatValue(policy.Format, nil), admin.CheckCriteria(policy.Criteria, nil))),
		admin.CheckPreviewTarget(&visible, policy.Format, b.Repository))
	if err != nil {
		return refused(err)
	}
	preview, err := run(policy, b.Repository)
	if err != nil {
		return refused(previewRefusal(err))
	}
	return ok(preview)
}

// previewRefusal gives a refusal of the dry run in the form of a preview:
// the one at repository as it is, those of the policy with the prefix
// policy.. Any other error passes unchanged.
func previewRefusal(err error) error {
	var r *admin.Refusal
	if errors.As(err, &r) && r.Field == "repository" {
		return err
	}
	return admin.PreviewPolicy(err)
}
