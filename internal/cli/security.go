package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

const securityLogger = "pipesmith_registry.security"

// askFunc asks the operator for a secret; the answer is never echoed.
type askFunc func(prompt string) (string, error)

var _ auth.Store = (*state.Store)(nil)

// security is the service of serve with the state file it follows.
type security struct {
	*auth.Service
	store *state.Store
}

// Close closes the state file when serve ends.
func (s *security) Close() error { return s.store.Close() }

// closeSecurity closes the state file of a service of buildSecurity.
func closeSecurity(sec model.Security) {
	if c, ok := sec.(io.Closer); ok {
		_ = c.Close() // the state file is only read and written in transactions; nothing is left to save
	}
}

// buildSecurity turns security on from state.sqlite of the data directory
// (decision T1 of the thin slice): without that file it returns nil and the
// server answers as in port step 2. The file stays open: the service reads
// its generation and loads it again after another process changed it, until
// closeSecurity. The bind password of an LDAP connection comes from its
// environment variable, else from its secret file below secrets/ of the data
// directory, else from ask when it is not nil (T12, hardening line 11); a
// reload reads the variable and the file again, takes a password asked at
// start and asks nobody. A connection without a usable setting is left out
// with a warning, so the local users still sign in. The forwarded headers of
// proxies count for the sign in limits and the session cookie (decision E8).
func buildSecurity(dataDir string, proxies auth.TrustedProxies, ask askFunc, log *slog.Logger) (model.Security, error) {
	st, err := state.Open(dataDir)
	if errors.Is(err, state.ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sec, err := securityOf(st, dataDir, proxies, ask, log)
	if err != nil {
		_ = st.Close() // the start failed; its error is the one that counts
		return nil, err
	}
	return sec, nil
}

func securityOf(st *state.Store, dataDir string, proxies auth.TrustedProxies, ask askFunc, log *slog.Logger) (*security, error) {
	// the generation before the records: a write between the two is seen at
	// the first reload
	gen, err := st.Generation()
	if err != nil {
		return nil, err
	}
	snap, err := st.Load()
	if err != nil {
		return nil, err
	}
	builds := newLDAPBuilds(dataDir, log)
	dirs, err := builds.build(snap.LDAP, ask)
	if err != nil {
		return nil, err
	}
	svc, err := auth.New(auth.Options{Snapshot: snap, Generation: gen, Store: st, Directories: dirs, Logger: log,
		TrustedProxies: proxies, DirectoriesFor: builds.directories})
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
	if local == 0 && len(snap.LDAP) == 0 {
		// decision N1: a data directory without identity rows starts closed
		log.Warn("nobody can sign in: add the first user with: pipesmith-registry user add <name> --roles nx-admin")
	}
	if len(proxies) > 0 {
		names := make([]string, len(proxies))
		for i, p := range proxies {
			names[i] = p.String()
			if p.IsSingleIP() {
				names[i] = p.Addr().String()
			}
		}
		log.Info("forwarded headers count from the trusted proxies " + strings.Join(names, ", "))
	}
	return &security{Service: svc, store: st}, nil
}

// ordered are the LDAP connections in their stored order.
func ordered(servers []model.LDAPServer) []model.LDAPServer {
	servers = slices.Clone(servers)
	slices.SortStableFunc(servers, func(a, b model.LDAPServer) int { return a.Order - b.Order })
	return servers
}

// passwordSource is the bind password of a connection, where it came from,
// and the options its directory is built with: the certificates of its CA
// file (decision N18).
type passwordSource struct {
	password string
	from     string // the variable, the secret file or the prompt; empty when none gave one
	ca       []byte
	options  []ldapauth.Option
}

// bindPassword is the bind password of a connection and the certificates of
// its CA file. The password is its variable, else its secret file
// (state.LDAPSecretPath), else the password asked at start for its id. A
// secret file that state.ReadSecret refuses and a CA file that cannot be read
// are errors, and the connection is left out: neither the variable and the
// prompt nor the system roots alone are taken instead.
func bindPassword(dataDir string, server model.LDAPServer, asked map[string]string) (passwordSource, error) {
	src, err := passwordOf(dataDir, server, asked)
	if err != nil || server.CAFile == "" {
		return src, err
	}
	if src.ca, err = readCA(server.CAFile); err != nil {
		return passwordSource{}, fmt.Errorf("the CA file %s cannot be read: %w", server.CAFile, err)
	}
	src.options = append(src.options, ldapauth.WithCACertificates(src.ca))
	return src, nil
}

func passwordOf(dataDir string, server model.LDAPServer, asked map[string]string) (passwordSource, error) {
	if value, ok := ldapauth.PasswordFromEnv(server.ID); ok {
		return passwordSource{password: value, from: ldapauth.PasswordVariable(server.ID)}, nil
	}
	path := state.LDAPSecretPath(dataDir, server.ID)
	data, err := state.ReadSecret(path)
	switch {
	case err == nil:
		value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
		return passwordSource{password: value, from: path}, nil
	case !errors.Is(err, state.ErrNoSecret):
		return passwordSource{}, err
	}
	if value, ok := asked[server.ID]; ok {
		return passwordSource{password: value, from: "the prompt at start"}, nil
	}
	return passwordSource{}, nil
}

// ldapBuilds builds the LDAP connections of serve and keeps each directory
// with what it was built from: a reload builds a connection again only when
// its row or its bind password changed, so an unchanged one keeps its
// incident counter. A line is logged when the outcome of a connection
// changes, not at every reload.
type ldapBuilds struct {
	dataDir string
	log     *slog.Logger
	mu      sync.Mutex
	asked   map[string]string // bind passwords asked at start, by connection id
	built   map[string]ldapBuild
}

type ldapBuild struct {
	server   model.LDAPServer
	password string
	from     string
	ca       []byte
	refusal  string         // why the connection is left out; empty when it is built
	dir      auth.Directory // nil when left out
}

func newLDAPBuilds(dataDir string, log *slog.Logger) *ldapBuilds {
	return &ldapBuilds{dataDir: dataDir, log: log, asked: map[string]string{}, built: map[string]ldapBuild{}}
}

// directories gives the connections of a snapshot loaded again, without a
// prompt (auth.Options.DirectoriesFor).
func (b *ldapBuilds) directories(servers []model.LDAPServer) []auth.Directory {
	dirs, _ := b.build(servers, nil) // without a prompt nothing fails
	return dirs
}

// build gives the directories of the connections in their order. A
// connection of the simple scheme that has no bind password otherwise gets
// the answer of ask when ask is not nil; a failed prompt stops the start.
func (b *ldapBuilds) build(servers []model.LDAPServer, ask askFunc) ([]auth.Directory, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var dirs []auth.Directory
	next := map[string]ldapBuild{}
	for _, server := range ordered(servers) {
		simple := strings.EqualFold(strings.TrimSpace(server.AuthScheme), "SIMPLE")
		src, err := bindPassword(b.dataDir, server, b.asked)
		if err == nil && src.from == "" && simple && ask != nil {
			password, aerr := ask("bind password of LDAP connection " + server.Name + ": ")
			if aerr != nil {
				return nil, fmt.Errorf("LDAP connection %s: %w", server.Name, aerr)
			}
			b.asked[server.ID] = password
			src.password, src.from = password, "the prompt at start"
		}
		last, known := b.built[server.ID]
		now := ldapBuild{server: server, password: src.password, from: src.from, ca: src.ca}
		if err == nil && known && last.dir != nil && reflect.DeepEqual(last.server, server) && last.password == src.password &&
			bytes.Equal(last.ca, src.ca) {
			now.dir = last.dir
		} else if err == nil {
			d, nerr := ldapauth.New(server, src.password, src.options...)
			if nerr != nil {
				now.refusal = nerr.Error()
				if simple && src.from == "" {
					now.refusal += "; or write it with: pipesmith-registry ldap password " + server.Name
				}
			} else {
				now.dir = directory{d}
			}
		} else {
			now.refusal = err.Error()
		}
		next[server.ID] = now
		b.report(server, last, known, now, simple)
		if now.dir != nil {
			dirs = append(dirs, now.dir)
		}
	}
	b.built = next
	return dirs, nil
}

// report logs what changed for a connection since its last build.
func (b *ldapBuilds) report(server model.LDAPServer, last ldapBuild, known bool, now ldapBuild, simple bool) {
	if now.refusal != "" {
		if !known || last.refusal != now.refusal {
			b.log.Warn(fmt.Sprintf("LDAP connection %s left out: %s", server.Name, now.refusal))
		}
		return
	}
	if known && last.dir == now.dir {
		return
	}
	if simple {
		b.log.Info(fmt.Sprintf("LDAP connection %s: bind password from %s", server.Name, now.from))
		if strings.EqualFold(strings.TrimSpace(server.Protocol), "ldap") {
			b.log.Warn(fmt.Sprintf("LDAP connection %s sends its bind password in clear: protocol ldap with the simple scheme", server.Name))
		}
	}
}

type login interface {
	Login(ctx context.Context, username, password string) (model.User, []string, error)
	Lookup(ctx context.Context, username string) (model.User, []string, error)
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
	case errors.Is(err, ldapauth.ErrBusy):
		return model.User{}, nil, fmt.Errorf("%w: %w", auth.ErrBusy, err)
	}
	return model.User{}, nil, err
}

func (d directory) Lookup(ctx context.Context, username string) (model.User, []string, error) {
	user, groups, err := d.d.Lookup(ctx, username)
	switch {
	case err == nil:
		return user, groups, nil
	case errors.Is(err, ldapauth.ErrNoEntry):
		return model.User{}, nil, auth.ErrNoUser
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
