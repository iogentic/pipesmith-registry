package auth

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strconv"

	"github.com/mirosso25/pipesmith-registry/internal/model"
)

// maxSignInBody bounds the body of POST /api/session (8 KiB).
const maxSignInBody = 8 << 10

// Messages of the session answers. The refusal of a sign in is one text for
// every reason.
const (
	msgWrongPair  = "wrong user name or password"
	msgCSRF       = "missing or wrong request token"
	msgBadBody    = "the body must be a JSON object with username and password"
	msgMediaType  = "the body must be application/json"
	msgTooLarge   = "the body is larger than 8 KiB"
	msgNotAllowed = "method not allowed"
)

type sessionUser struct {
	UserID       string `json:"userId"`
	FirstName    string `json:"firstName"`
	LastName     string `json:"lastName"`
	EmailAddress string `json:"emailAddress"`
	Source       string `json:"source"`
	Realm        string `json:"realm"`
}

// sessionAnswer is the Session of the interface contract.
type sessionAnswer struct {
	User             *sessionUser `json:"user"`
	Permissions      []string     `json:"permissions"`
	CSRFToken        *string      `json:"csrfToken"`
	ExpiresInSeconds int          `json:"expiresInSeconds"`
}

// SessionHandler serves POST, GET and DELETE /api/session.
func (s *Service) SessionHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		switch r.Method {
		case http.MethodPost:
			s.serveSignIn(w, r)
		case http.MethodGet:
			s.serveSession(w, r)
		case http.MethodDelete:
			s.serveSignOut(w, r)
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			writeMessage(w, http.StatusMethodNotAllowed, msgNotAllowed)
		}
	})
}

func (s *Service) serveSignIn(w http.ResponseWriter, r *http.Request) {
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
		writeMessage(w, http.StatusUnsupportedMediaType, msgMediaType)
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSignInBody)).Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeMessage(w, http.StatusRequestEntityTooLarge, msgTooLarge)
		} else {
			writeMessage(w, http.StatusBadRequest, msgBadBody)
		}
		return
	}
	subject, ok, wait := s.tryPair(body.Username, clientAddress(r), func() (model.Subject, bool) {
		return s.signIn(r.Context(), body.Username, body.Password)
	})
	if wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if !ok {
		writeMessage(w, http.StatusUnauthorized, msgWrongPair)
		return
	}
	if old, err := r.Cookie(CookieName); err == nil {
		s.sessions.remove(old.Value)
	}
	created, err := s.sessions.create(subject)
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "no session could be started")
		return
	}
	// #nosec G124 -- Secure only over TLS (decision T3, interface contract); HttpOnly and SameSite=Strict are set.
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: created.id, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	writeJSON(w, http.StatusOK, answerOf(created))
}

func (s *Service) serveSession(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(CookieName)
	if err == nil {
		if live, ok := s.sessions.get(cookie.Value); ok {
			writeJSON(w, http.StatusOK, answerOf(&live))
			return
		}
		clearCookie(w, r)
	}
	writeJSON(w, http.StatusOK, sessionAnswer{Permissions: []string{}})
}

func (s *Service) serveSignOut(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(CookieName); err == nil {
		if live, ok := s.sessions.get(cookie.Value); ok {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(live.csrf)) != 1 {
				writeMessage(w, http.StatusForbidden, msgCSRF)
				return
			}
			s.sessions.remove(cookie.Value)
		}
		clearCookie(w, r)
	}
	w.WriteHeader(http.StatusNoContent)
}

func answerOf(live *session) sessionAnswer {
	u := live.subject.User
	token := live.csrf
	permissions := live.subject.Permissions
	if permissions == nil {
		permissions = []string{}
	}
	return sessionAnswer{
		User: &sessionUser{UserID: u.ID, FirstName: u.FirstName, LastName: u.LastName, EmailAddress: u.Email,
			Source: u.Source, Realm: live.subject.Realm},
		Permissions:      permissions,
		CSRFToken:        &token,
		ExpiresInSeconds: int(IdleTime.Seconds()),
	}
}

func clearCookie(w http.ResponseWriter, r *http.Request) {
	// #nosec G124 -- Secure only over TLS (decision T3, interface contract); HttpOnly and SameSite=Strict are set.
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeMessage(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}
