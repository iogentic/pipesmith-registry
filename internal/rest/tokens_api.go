package rest

import (
	"errors"
	"net/http"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The tokens and the own account of the interface (leg 6-api, contract
// S29 and S41, rulings AP1 to AP4 and AP7). The server checks the rule of
// the route and the request token before it calls a function here.

// TokenService is what the tokens and the own password need of the
// security service; auth.Service fulfils it.
type TokenService interface {
	CheckTokenKind(kind string) error
	IssueToken(subject *model.Subject, kind, name string) (string, model.Token, error)
	ListTokens(userID, source string) ([]auth.TokenEntry, error)
	TokenOwner(id string) (userID, source string, found bool, err error)
	RemoveTokenID(id string) (bool, error)
}

// usersUpdate is the permission that the tokens of another user ask
// besides the one of the route (contract S29).
const usersUpdate = "nexus:users:update"

// tokenXO is a token in a list. lastUsed is null for a token never used;
// the times are those of the REST answers of the tasks.
type tokenXO struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Name       string  `json:"name"`
	UserID     string  `json:"userId"`
	Created    string  `json:"created"`
	LastUsed   *string `json:"lastUsed"`
	PublicPart string  `json:"publicPart"`
}

// issuedXO is the answer of the issue: the token and, once, its secret.
type issuedXO struct {
	tokenXO
	Secret string `json:"secret"`
}

func tokenOf(e auth.TokenEntry) tokenXO {
	x := tokenXO{ID: e.ID, Kind: e.Kind, Name: e.Name, UserID: e.UserID, Created: timestamp(e.Created), PublicPart: e.Public}
	if !e.LastUsed.IsZero() {
		used := timestamp(e.LastUsed)
		x.LastUsed = &used
	}
	return x
}

// tokenSources are the sources whose tokens GET /api/tokens?userId= lists.
var tokenSources = []string{localSource, auth.SourceLDAP}

// ListTokens is GET /api/tokens[?userId=]: the tokens of the subject, or
// those of the user userId in every source, which asks nexus:users:update
// as well (403 without it, ruling AP3).
func ListTokens(env *Env, svc TokenService, subject *model.Subject, userID string) (Answer, error) {
	type owner struct{ id, source string }
	owners := []owner{{subject.User.ID, sourceOf(subject.User.Source)}}
	if userID != "" && userID != subject.User.ID {
		if !env.permits(usersUpdate) {
			return refused(&admin.Refusal{Code: admin.CodeForbidden, Message: "the tokens of another user need " + usersUpdate})
		}
		owners = owners[:0]
		for _, source := range tokenSources {
			owners = append(owners, owner{userID, source})
		}
	}
	list := []tokenXO{}
	for _, o := range owners {
		entries, err := svc.ListTokens(o.id, o.source)
		if err != nil {
			return Answer{}, err
		}
		for _, e := range entries {
			list = append(list, tokenOf(e))
		}
	}
	return ok(list)
}

// tokenBody is the body of POST /api/tokens.
type tokenBody struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// CreateToken is POST /api/tokens: an npm or user token of the subject
// itself, 201 with the token and its secret, which no later answer holds
// (A11, AP4). A subject that signed in with a token answers 403. docker,
// an unknown kind and a kind whose realm is off answer 400 at kind (AP1),
// checked before the name; a name outside the pattern 400 at name, a name the user holds for the
// kind 409 (AP2), and a name beyond the 20 named tokens of the kind 409 as
// well.
func CreateToken(svc TokenService, subject *model.Subject, body []byte) (Answer, error) {
	if err := admin.CheckSignedInDirectly(subject); err != nil {
		return refused(err)
	}
	var b tokenBody
	if err := decode(body, '{', &b); err != nil {
		return refused(err)
	}
	var kind *auth.TokenKindError
	if err := svc.CheckTokenKind(b.Kind); errors.As(err, &kind) {
		return refused(admin.TokenKindRefusal(kind.Reason))
	} else if err != nil {
		return Answer{}, err
	}
	if err := admin.CheckTokenName(b.Name); err != nil {
		return refused(err)
	}
	text, row, err := svc.IssueToken(subject, b.Kind, b.Name)
	switch {
	case errors.As(err, &kind):
		return refused(admin.TokenKindRefusal(kind.Reason))
	case errors.Is(err, model.ErrTokenName):
		return refused(admin.TokenNameTaken(b.Name, b.Kind))
	case errors.Is(err, model.ErrTokenLimit):
		return refused(admin.TokenLimit(b.Kind, auth.MaxTokensPerKind))
	case err != nil:
		return Answer{}, err
	}
	x := tokenOf(auth.TokenEntry{ID: row.Public, Kind: row.Kind, Name: row.Name, UserID: row.UserID, Public: row.Public, Created: row.Created})
	return Answer{Status: http.StatusCreated, Body: issuedXO{tokenXO: x, Secret: text}}, nil
}

// DeleteToken is DELETE /api/tokens/{id}: 204 once the token or imported
// key is revoked; a subject that signed in with a token answers 403. A
// token of another user asks nexus:users:update as well;
// without it the token answers 404 as one that does not exist.
func DeleteToken(env *Env, svc TokenService, subject *model.Subject, id string) (Answer, error) {
	if err := admin.CheckSignedInDirectly(subject); err != nil {
		return refused(err)
	}
	userID, source, found, err := svc.TokenOwner(id)
	if err != nil {
		return Answer{}, err
	}
	own := found && userID == subject.User.ID && source == sourceOf(subject.User.Source)
	if !found || !own && !env.permits(usersUpdate) {
		return refused(notFound("token " + id))
	}
	removed, err := svc.RemoveTokenID(id)
	if err != nil {
		return Answer{}, err
	}
	if !removed {
		return refused(notFound("token " + id))
	}
	return noContent()
}
