package server

import (
	"slices"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Review Focus 5 of the execution plan: a bearer token of the Docker flow
// issued for one repository arrives on the port of another, and a token of
// a user who lost a role arrives after the change. The permission of the
// repository of the request decides at request time, never the repository a
// token was issued for nor the scope text a client asked for.

func TestDockerTokenOfOneRepositoryOnThePortOfAnother(t *testing.T) {
	snap := tokenSnapshot(t)
	snap.Anonymous.Enabled = true
	snap.Repositories[0].Attributes = dockerAttributes(true) // demo-docker: no anonymous pull
	a := newTokenApp(t, snap)
	issue := func(repo, scope string, header ...string) string {
		t.Helper()
		return a.dockerTokenOf(t, a.on(repo, "GET", "/v2/token?service=Pipesmith+Registry&scope="+scope, header...)).Token
	}
	manifest := "/v2/app/manifests/1.0"

	// demo-puller may read demo-docker only: its token of that port, even
	// one that asked for push on both, reads nothing on the other port
	puller := issue("demo-docker", "repository:app:pull,push", "Authorization", basicPair(pullerUser, pullerPass))
	expect(t, a.on("demo-docker", "GET", manifest, "Authorization", "Bearer "+puller), 200)
	denied := a.on("demo-docker-b", "GET", manifest, "Authorization", "Bearer "+puller)
	if denied.status != 403 || errorCode(t, denied) != "DENIED" {
		t.Fatalf("token of demo-docker on the port of demo-docker-b: %s", denied)
	}
	expect(t, raw(t, a.addr(), "GET", "/repository/demo-docker-b/v2/app/manifests/1.0", "Authorization", "Bearer "+puller), 403)

	// demo-both reads both: its token of one port opens the other, whatever
	// the scope named
	both := issue("demo-docker", "repository:other:pull", "Authorization", basicPair(bothUser, bothPass))
	expect(t, a.on("demo-docker-b", "GET", manifest, "Authorization", "Bearer "+both), 200)

	// a token of the anonymous subject from the port that allows an
	// anonymous pull is anonymous on the port that does not
	anonymous := issue("demo-docker-b", "repository:app:pull")
	expect(t, a.on("demo-docker-b", "GET", manifest, "Authorization", "Bearer "+anonymous), 200)
	if r := a.on("demo-docker", "GET", manifest, "Authorization", "Bearer "+anonymous); r.status != 401 || errorCode(t, r) != "UNAUTHORIZED" {
		t.Fatalf("anonymous token on the port without an anonymous pull: %s", r)
	}
	if r := a.on("demo-docker", "GET", "/v2/token?scope=repository:app:pull"); r.status != 401 {
		t.Fatalf("anonymous token of the port without an anonymous pull: %s", r)
	}
}

func TestTokenOfAUserWhoLostARole(t *testing.T) {
	a := newTokenApp(t, tokenSnapshot(t))
	tarball := "/repository/demo-npm/widget/-/widget-1.2.3.tgz"
	manifest := "/v2/app/manifests/1.0"

	npm := a.npmToken(t, npmUser, npmPass)
	docker := a.dockerTokenOf(t, a.on("demo-docker", "GET", "/v2/token", "Authorization", basicPair(bothUser, bothPass))).Token
	text, row, err := auth.NewToken(auth.KindUser, pullerUser, auth.SourceLocal, a.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.AddToken(row, auth.MaxTokensPerKind); err != nil {
		t.Fatal(err)
	}
	a.change(t, func(s *model.Snapshot) { s.Realms = append(s.Realms, auth.RealmUserToken) })
	expect(t, raw(t, a.addr(), "GET", tarball, "Authorization", "Bearer "+npm), 200)
	expect(t, a.on("demo-docker-b", "GET", manifest, "Authorization", "Bearer "+docker), 200)
	expect(t, a.on("demo-docker", "GET", manifest, "Authorization", "Bearer "+text), 200)
	expect(t, a.on("demo-docker", "GET", manifest, "Authorization", basicPair(pullerUser, text)), 200)

	a.change(t, func(s *model.Snapshot) {
		for i := range s.Users {
			switch s.Users[i].ID {
			case npmUser:
				s.Users[i].Roles = []string{}
			case bothUser:
				s.Users[i].Roles = []string{"demo-docker-a"}
			case pullerUser:
				s.Users[i].Roles = slices.DeleteFunc(s.Users[i].Roles, func(r string) bool { return r == "demo-docker-a" })
			}
		}
	})
	// the tokens still name their users, who no longer hold the right
	expect(t, raw(t, a.addr(), "GET", tarball, "Authorization", "Bearer "+npm), 403)
	if r := a.on("demo-docker-b", "GET", manifest, "Authorization", "Bearer "+docker); r.status != 403 || errorCode(t, r) != "DENIED" {
		t.Fatalf("Docker token after its user lost the role of demo-docker-b: %s", r)
	}
	expect(t, a.on("demo-docker", "GET", manifest, "Authorization", "Bearer "+docker), 200)
	expect(t, a.on("demo-docker", "GET", manifest, "Authorization", "Bearer "+text), 403)
	expect(t, a.on("demo-docker", "GET", manifest, "Authorization", basicPair(pullerUser, text)), 403)
	who := raw(t, a.addr(), "GET", "/repository/demo-npm/-/whoami", "Authorization", "Bearer "+npm)
	if who.status != 200 || string(who.body) != `{"username":"`+npmUser+`"}` {
		t.Fatalf("whoami after the change: %s", who)
	}
}
