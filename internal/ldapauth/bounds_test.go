package ldapauth_test

// The bounds of a check of a form (ruling I24) against ldapfake, a
// directory that can send more than a client asks for. The values are
// invented.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth/ldapfake"
)

const fakeBind = "example-fake-bind-1"

func person(i int, extra ...string) ldapfake.Entry {
	id := fmt.Sprintf("demo-person-%02d", i)
	e := ldapfake.Entry{DN: "uid=" + id + ",ou=people,dc=example,dc=com",
		Attributes: map[string][]string{"uid": {id}, "cn": {"Demo Person"}, "mail": {id + "@example.com"}}}
	if len(extra) > 0 {
		e.Attributes["cn"] = extra
	}
	return e
}

func group(i int) ldapfake.Entry {
	return ldapfake.Entry{DN: fmt.Sprintf("cn=demo-group-%04d,ou=groups,dc=example,dc=com", i),
		Attributes: map[string][]string{"cn": {fmt.Sprintf("demo-group-%04d", i)}}}
}

func fakeDirectory(t *testing.T) *ldapfake.Server {
	t.Helper()
	s, err := ldapfake.Start(fakeBind)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func checkDirectoryOf(t *testing.T, fake *ldapfake.Server, opts ...ldapauth.Option) *ldapauth.Directory {
	t.Helper()
	s := validServer()
	s.Port, s.ConnectionTimeoutSeconds = fake.Port(), 5
	d, err := ldapauth.New(s, fakeBind, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func people(n int) []ldapfake.Entry {
	var list []ldapfake.Entry
	for i := range n {
		list = append(list, person(i))
	}
	return list
}

// Every search of a check asks a size limit and a time limit of the server.
func TestCheckSearchesAskLimits(t *testing.T) {
	fake := fakeDirectory(t)
	fake.Users, fake.Groups = people(3), []ldapfake.Entry{group(1)}
	d := checkDirectoryOf(t, fake, ldapauth.WithCheckBounds())
	if _, err := d.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	members, more, err := d.Users(context.Background(), 20)
	if err != nil || more || len(members) != 3 || len(members[0].Groups) != 1 {
		t.Fatalf("users %+v, more %v, %v", members, more, err)
	}
	searches := fake.Searches()
	if len(searches) != 5 {
		t.Fatalf("%d searches: %+v", len(searches), searches)
	}
	want := map[string]int{"dc=example,dc=com": 1, "ou=people,dc=example,dc=com": ldapauth.CheckUserEntries,
		"ou=groups,dc=example,dc=com": ldapauth.CheckGroupEntries}
	for _, s := range searches {
		if s.SizeLimit != want[s.Base] || s.TimeLimit != 5 {
			t.Errorf("search %+v, want the size limit %d and the time limit 5", s, want[s.Base])
		}
	}
	// a directory that keeps to the size limit: the first 20 and more
	fake.Users = people(30)
	members, more, err = d.Users(context.Background(), 20)
	if err != nil || !more || len(members) != 20 {
		t.Fatalf("a directory that keeps to the limit: %d users, more %v, %v", len(members), more, err)
	}
}

func TestCheckBounds(t *testing.T) {
	big := strings.Repeat("x", ldapauth.CheckReadBytes)
	for _, c := range []struct {
		name   string
		users  []ldapfake.Entry
		groups []ldapfake.Entry
		words  string
	}{
		{"users", people(40), nil, fmt.Sprintf("more than %d user entries", ldapauth.CheckUserEntries)},
		{"groups", people(1), func() []ldapfake.Entry {
			var list []ldapfake.Entry
			for i := range ldapauth.CheckGroupEntries + 50 {
				list = append(list, group(i))
			}
			return list
		}(), fmt.Sprintf("more than %d group entries", ldapauth.CheckGroupEntries)},
		{"bytes", []ldapfake.Entry{person(1, big)}, nil, fmt.Sprintf("more than %d bytes", ldapauth.CheckReadBytes)},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := fakeDirectory(t)
			fake.Users, fake.Groups, fake.IgnoreSizeLimit = c.users, c.groups, true
			d := checkDirectoryOf(t, fake, ldapauth.WithCheckBounds())
			members, _, err := d.Users(context.Background(), 20)
			if !errors.Is(err, ldapauth.ErrBound) || members != nil || !strings.Contains(err.Error(), c.words) {
				t.Fatalf("%d users, %v; want the bound %q", len(members), err, c.words)
			}
			// the sign in check reads with the same bounds
			if c.name != "users" {
				_, _, err = d.Login(context.Background(), "demo-person-01", fakeBind)
				if !errors.Is(err, ldapauth.ErrBound) {
					t.Fatalf("sign in: %v", err)
				}
			}
		})
	}
}

// Without the bounds of a check a sign in reads the groups as before: no
// size limit.
func TestSignInWithoutBounds(t *testing.T) {
	fake := fakeDirectory(t)
	fake.Users = []ldapfake.Entry{person(1)}
	for i := range 1200 {
		fake.Groups = append(fake.Groups, group(i))
	}
	d := checkDirectoryOf(t, fake)
	user, groups, err := d.Login(context.Background(), "demo-person-01", fakeBind)
	if err != nil || user.ID != "demo-person-01" || len(groups) != 1200 {
		t.Fatalf("%v %d %v", user, len(groups), err)
	}
	for _, s := range fake.Searches() {
		if strings.HasPrefix(s.Base, "ou=groups") && s.SizeLimit != 0 {
			t.Fatalf("a sign in asked the size limit %d of the groups", s.SizeLimit)
		}
	}
}
