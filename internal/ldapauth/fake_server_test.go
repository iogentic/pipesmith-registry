package ldapauth_test

import (
	"context"
	"errors"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/go-ldap/ldap/v3"

	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// fakeServer is a loopback LDAP server of a few messages: every bind
// succeeds, a search below the user base finds the entry of the uid of its
// filter, and a search for a name of fail ends with the result code
// failCode. Other searches find nothing. It checks no password.
type fakeServer struct {
	ln       net.Listener
	mu       sync.Mutex
	entries  map[string]fakeEntry
	fail     map[string]bool
	failCode int64
}

type fakeEntry struct {
	dn    string
	attrs map[string][]string
}

var uidOfFilter = regexp.MustCompile(`\(uid=([^)]*)\)`)

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{ln: ln, entries: map[string]fakeEntry{}, fail: map[string]bool{}, failCode: ldap.LDAPResultOther}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				f.serve(c)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return f
}

func (f *fakeServer) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeServer) put(uid string, attrs map[string][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	attrs["uid"] = []string{uid}
	f.entries[uid] = fakeEntry{dn: "uid=" + uid + ",ou=people,dc=example,dc=com", attrs: attrs}
}

func (f *fakeServer) failName(uid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[uid] = true
}

func (f *fakeServer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	for {
		p, err := ber.ReadPacket(c)
		if err != nil || len(p.Children) < 2 {
			return
		}
		id, _ := p.Children[0].Value.(int64)
		op := p.Children[1]
		var out []*ber.Packet
		switch op.Tag {
		case ldap.ApplicationBindRequest:
			out = append(out, fakeResult(id, ldap.ApplicationBindResponse, ldap.LDAPResultSuccess))
		case ldap.ApplicationSearchRequest:
			out = f.search(id, op)
		case ldap.ApplicationUnbindRequest:
			return
		default:
			continue
		}
		for _, o := range out {
			if _, err := c.Write(o.Bytes()); err != nil {
				return
			}
		}
	}
}

func (f *fakeServer) search(id int64, op *ber.Packet) []*ber.Packet {
	base, _ := op.Children[0].Value.(string)
	filter, _ := ldap.DecompileFilter(op.Children[6])
	m := uidOfFilter.FindStringSubmatch(filter)
	if m == nil || !strings.HasSuffix(strings.ToLower(base), "ou=people,dc=example,dc=com") {
		return []*ber.Packet{fakeResult(id, ldap.ApplicationSearchResultDone, ldap.LDAPResultSuccess)}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[m[1]] {
		return []*ber.Packet{fakeResult(id, ldap.ApplicationSearchResultDone, f.failCode)}
	}
	e, ok := f.entries[m[1]]
	if !ok {
		return []*ber.Packet{fakeResult(id, ldap.ApplicationSearchResultDone, ldap.LDAPResultSuccess)}
	}
	msg := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
	msg.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, ""))
	entry := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ldap.ApplicationSearchResultEntry, nil, "")
	entry.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, e.dn, ""))
	attrs := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
	names := make([]string, 0, len(e.attrs))
	for name := range e.attrs {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		a := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
		a.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, name, ""))
		vals := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "")
		for _, v := range e.attrs[name] {
			vals.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, v, ""))
		}
		a.AppendChild(vals)
		attrs.AppendChild(a)
	}
	entry.AppendChild(attrs)
	msg.AppendChild(entry)
	return []*ber.Packet{msg, fakeResult(id, ldap.ApplicationSearchResultDone, ldap.LDAPResultSuccess)}
}

func fakeResult(id int64, app ber.Tag, code int64) *ber.Packet {
	msg := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
	msg.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, ""))
	op := ber.Encode(ber.ClassApplication, ber.TypeConstructed, app, nil, "")
	op.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, code, ""))
	op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", ""))
	op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "fake answer", ""))
	msg.AppendChild(op)
	return msg
}

// fakeDirectoryOf is a directory of the fake server: anonymous service bind,
// dynamic groups by memberOf, group base ou=groups.
func fakeDirectoryOf(t *testing.T, f *fakeServer, change func(*model.LDAPServer)) *ldapauth.Directory {
	t.Helper()
	s := validServer()
	s.Port, s.AuthScheme, s.AuthUsername = f.port(), "NONE", ""
	s.GroupType, s.UserMemberOfAttribute = "dynamic", "memberOf"
	if change != nil {
		change(&s)
	}
	d, err := ldapauth.New(s, "")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Review 3-61: with a group base, a memberOf value outside it names no
// group, so a group of the same name elsewhere maps to no role; inside the
// base, at any depth and in any letter case, it names its group as before.
// Without a group base every value names its group, as before.
func TestMemberOfOutsideTheGroupBaseNamesNoGroup(t *testing.T) {
	f := newFakeServer(t)
	f.put("demo-ldap-person", map[string][]string{"memberOf": {
		"cn=demo-admins,ou=other,dc=example,dc=com",
		"cn=demo-readers,OU=Groups,DC=example,DC=com",
		"cn=demo-deep,ou=team,ou=groups,dc=example,dc=com",
	}})
	for _, c := range []struct {
		base string
		want []string
	}{
		{"ou=groups", []string{"cn=demo-deep,ou=team,ou=groups,dc=example,dc=com", "cn=demo-readers,OU=Groups,DC=example,DC=com",
			"demo-deep", "demo-readers"}},
		{"", []string{"cn=demo-admins,ou=other,dc=example,dc=com", "cn=demo-deep,ou=team,ou=groups,dc=example,dc=com",
			"cn=demo-readers,OU=Groups,DC=example,DC=com", "demo-admins", "demo-deep", "demo-readers"}},
	} {
		d := fakeDirectoryOf(t, f, func(s *model.LDAPServer) { s.GroupBaseDN = c.base })
		for _, how := range []string{"login", "lookup"} {
			var groups []string
			var err error
			if how == "login" {
				_, groups, err = d.Login(context.Background(), "demo-ldap-person", "demo-any-pass")
			} else {
				_, groups, err = d.Lookup(context.Background(), "demo-ldap-person")
			}
			if err != nil || !slices.Equal(groups, c.want) {
				t.Errorf("group base %q, %s: groups %q %v, want %q", c.base, how, groups, err, c.want)
			}
		}
	}
}

// Review 3-61: a sign in name whose search the directory answers with an
// error counts no incident of the connection, so names a client chooses
// cannot rest it for everybody. A busy or unavailable directory still
// counts.
func TestAFailingNameCountsNoIncident(t *testing.T) {
	f := newFakeServer(t)
	f.put("demo-ldap-person", map[string][]string{})
	f.failName("demo-fail")
	d := fakeDirectoryOf(t, f, nil) // MaxIncidentsCount 3
	for range 5 {
		if _, _, err := d.Login(context.Background(), "demo-fail", "demo-any-pass"); !errors.Is(err, ldapauth.ErrUnavailable) {
			t.Fatalf("a name whose search fails: %v", err)
		}
		if _, _, err := d.Lookup(context.Background(), "demo-fail"); !errors.Is(err, ldapauth.ErrUnavailable) {
			t.Fatalf("a lookup whose search fails: %v", err)
		}
	}
	if n := ldapauth.Failures(d); n != 0 {
		t.Fatalf("%d incidents after names whose search failed, want 0", n)
	}
	if _, _, err := d.Login(context.Background(), "demo-ldap-person", "demo-any-pass"); err != nil {
		t.Fatalf("a good name after the failing ones: %v", err)
	}
	f.mu.Lock()
	f.failCode = ldap.LDAPResultBusy
	f.mu.Unlock()
	busy := fakeDirectoryOf(t, f, nil)
	for range 2 {
		_, _, _ = busy.Login(context.Background(), "demo-fail", "demo-any-pass")
	}
	if n := ldapauth.Failures(busy); n != 2 {
		t.Fatalf("%d incidents after two answers busy, want 2", n)
	}
}
