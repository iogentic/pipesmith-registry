package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

const securityLogger = "pipesmith_registry.security"

// askFunc asks the operator for a secret; the answer is never echoed.
type askFunc func(prompt string) (string, error)

// buildSecurity turns security on from state.sqlite of the data directory
// (decision T1 of the thin slice): without that file it returns nil and the
// server answers as in port step 2. The bind password of an LDAP connection
// comes from its environment variable, else from ask when it is not nil
// (T12). A connection without a usable setting is left out with a warning,
// so the local users still sign in.
func buildSecurity(dataDir string, ask askFunc, log *slog.Logger) (model.Security, error) {
	st, err := state.Open(dataDir)
	if errors.Is(err, state.ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	snap, err := st.Load()
	_ = st.Close() // read only; the load error is the one that counts
	if err != nil {
		return nil, err
	}
	dirs, err := directories(snap.LDAP, ask, log)
	if err != nil {
		return nil, err
	}
	sec, err := auth.New(auth.Options{Snapshot: snap, Directories: dirs, Logger: log})
	if err != nil {
		return nil, err
	}
	local := 0
	for _, u := range snap.Users {
		if u.Source == auth.SourceLocal {
			local++
		}
	}
	anonymous := "off"
	if snap.Anonymous.Enabled {
		anonymous = "on"
	}
	log.Info(fmt.Sprintf("security on: %d local users, %d of %d LDAP connections, anonymous access %s",
		local, len(dirs), len(snap.LDAP), anonymous))
	return sec, nil
}

// directories builds the LDAP connections in their order.
func directories(servers []model.LDAPServer, ask askFunc, log *slog.Logger) ([]auth.Directory, error) {
	servers = slices.Clone(servers)
	slices.SortStableFunc(servers, func(a, b model.LDAPServer) int { return a.Order - b.Order })
	var dirs []auth.Directory
	for _, server := range servers {
		simple := strings.EqualFold(strings.TrimSpace(server.AuthScheme), "SIMPLE")
		password, found := ldapauth.PasswordFromEnv(server.ID)
		if !found && simple && ask != nil {
			var err error
			if password, err = ask("bind password of LDAP connection " + server.Name + ": "); err != nil {
				return nil, fmt.Errorf("LDAP connection %s: %w", server.Name, err)
			}
		}
		d, err := ldapauth.New(server, password)
		if err != nil {
			log.Warn(fmt.Sprintf("LDAP connection %s left out: %v", server.Name, err))
			continue
		}
		if simple && strings.EqualFold(strings.TrimSpace(server.Protocol), "ldap") {
			log.Warn(fmt.Sprintf("LDAP connection %s sends its bind password in clear: protocol ldap with the simple scheme", server.Name))
		}
		dirs = append(dirs, directory{d})
	}
	return dirs, nil
}

type login interface {
	Login(ctx context.Context, username, password string) (model.User, []string, error)
}

// directory gives the errors of ldapauth the meaning auth reads from them.
type directory struct{ d login }

var _ auth.Directory = directory{}

func (d directory) Login(ctx context.Context, username, password string) (model.User, []string, error) {
	user, groups, err := d.d.Login(ctx, username, password)
	switch {
	case err == nil:
		return user, groups, nil
	case errors.Is(err, ldapauth.ErrInvalidCredentials):
		return model.User{}, nil, auth.ErrWrongPair
	case errors.Is(err, ldapauth.ErrUnavailable):
		return model.User{}, nil, fmt.Errorf("%w: %w", auth.ErrUnreachable, err)
	}
	return model.User{}, nil, err
}

// fieldHandler writes the attributes of a record after its message as
// key=value, for the log handler of serve that prints the message alone.
type fieldHandler struct{ slog.Handler }

func (h fieldHandler) Handle(ctx context.Context, r slog.Record) error {
	var fields []string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key != "logger" {
			fields = append(fields, a.Key+"="+a.Value.String())
		}
		return true
	})
	if len(fields) == 0 {
		return h.Handler.Handle(ctx, r)
	}
	out := slog.NewRecord(r.Time, r.Level, r.Message+": "+strings.Join(fields, " "), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "logger" {
			out.AddAttrs(a)
		}
		return true
	})
	return h.Handler.Handle(ctx, out)
}

func (h fieldHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return fieldHandler{h.Handler.WithAttrs(attrs)}
}

func (h fieldHandler) WithGroup(name string) slog.Handler {
	return fieldHandler{h.Handler.WithGroup(name)}
}
