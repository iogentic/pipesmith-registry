package server

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Task 10 of the phase 5 plan: the sign in of a proxy to its remote
// (decisions 5.12 and 5.13). Type and user name are attributes of the
// repository; the secret half is the file secrets/remote/<name> of the data
// directory.

const (
	canaryPassword = "CANARY-SIGNIN-PASSWORD-5b10"
	canaryToken    = "CANARY-SIGNIN-TOKEN-5b10"
	canaryWrong    = "CANARY-SIGNIN-WRONG-5b10"
	remoteUser     = "demo-remote-user"
)

// authentication is the attribute httpclient.authentication of a proxy.
func authentication(kind string, preemptive bool) map[string]any {
	auth := map[string]any{"type": kind, "preemptive": preemptive}
	if kind != "bearerToken" {
		auth["username"] = remoteUser
	}
	return map[string]any{"httpclient": map[string]any{"authentication": auth}}
}

// leaked names every canary or its Basic form that text holds.
func leaked(text string) []string {
	var found []string
	for _, secret := range []string{canaryPassword, canaryToken, canaryWrong} {
		if strings.Contains(text, secret) || strings.Contains(text, base64.StdEncoding.EncodeToString([]byte(remoteUser+":"+secret))) {
			found = append(found, secret)
		}
	}
	return found
}

func TestProxySignsInToItsRemote(t *testing.T) {
	log := captureLog(t)
	u, _ := blockApp(t, []proxyDef{
		{name: "demo-basic-proxy", format: "raw", attributes: authentication("username", false)},
		{name: "demo-basic-once", format: "raw", attributes: authentication("username", true)},
		{name: "demo-bearer-proxy", format: "raw", attributes: authentication("bearerToken", false)},
		{name: "demo-bearer-once", format: "raw", attributes: authentication("bearerToken", true)},
		{name: "demo-wrong-proxy", format: "raw", attributes: authentication("username", false)},
		{name: "demo-nosecret-proxy", format: "raw", attributes: authentication("username", true)},
		{name: "demo-ntlm-proxy", format: "raw", attributes: authentication("ntlm", true)},
	})
	for name, secret := range map[string]string{"demo-basic-proxy": canaryPassword, "demo-basic-once": canaryPassword,
		"demo-bearer-proxy": canaryToken, "demo-bearer-once": canaryToken, "demo-wrong-proxy": canaryWrong, "demo-ntlm-proxy": canaryPassword} {
		if _, err := proxy.WriteSecret(u.cfg.DataDir, name, secret); err != nil {
			t.Fatal(err)
		}
	}
	basic := testkit.BasicChallenge(remoteUser, canaryPassword, testkit.OK(rawBytes))
	bearer := testkit.BearerChallenge(canaryToken, testkit.OK(rawBytes))
	for name, answer := range map[string]testkit.Answer{"demo-basic-proxy": basic, "demo-basic-once": basic, "demo-bearer-proxy": bearer,
		"demo-bearer-once": bearer, "demo-wrong-proxy": basic, "demo-nosecret-proxy": basic, "demo-ntlm-proxy": basic} {
		for _, path := range []string{"docs/a.txt", "docs/b.txt"} {
			u.remote.Script(remoteBase(name)+path, answer)
		}
	}
	var bodies []string
	get := func(repo, path string, status, requests int) {
		t.Helper()
		before := len(u.remote.Requests())
		r := u.get(repo, path)
		bodies = append(bodies, fmt.Sprint(r.header), string(r.body))
		if r.status != status || status == 200 && string(r.body) != rawBytes {
			t.Fatalf("%s/%s: %d %q, want %d", repo, path, r.status, r.body, status)
		}
		if n := len(u.remote.Requests()) - before; n != requests {
			t.Fatalf("%s/%s: %d requests at the remote, want %d", repo, path, n, requests)
		}
	}
	// with the secret: on challenge one request more than at once
	get("demo-basic-proxy", "docs/a.txt", 200, 2)
	get("demo-basic-once", "docs/a.txt", 200, 1)
	get("demo-bearer-proxy", "docs/a.txt", 200, 2)
	get("demo-bearer-once", "docs/a.txt", 200, 1)
	for _, req := range u.remote.Requests() {
		got := req.Header.Get("Authorization")
		switch {
		case strings.HasPrefix(req.Target, "/demo-basic-") && got != "" && got != basic.Authorization,
			strings.HasPrefix(req.Target, "/demo-bearer-") && got != "" && got != bearer.Authorization:
			t.Fatalf("%s carried %q", req.Target, got)
		}
	}
	// a wrong secret: one 401 to the credentials, then needs secret and no request
	get("demo-wrong-proxy", "docs/a.txt", 404, 2)
	if s := u.state("demo-wrong-proxy"); s.Status != proxy.NeedsSecret || !strings.Contains(s.Reason, "repos remote-secret demo-wrong-proxy") {
		t.Fatalf("after a refused secret: %+v", s)
	}
	get("demo-wrong-proxy", "docs/b.txt", 404, 0)
	// a changed secret file leaves needs secret
	if _, err := proxy.WriteSecret(u.cfg.DataDir, "demo-wrong-proxy", canaryPassword); err != nil {
		t.Fatal(err)
	}
	get("demo-wrong-proxy", "docs/b.txt", 200, 2)
	if s := u.state("demo-wrong-proxy"); s.Status != proxy.Ready {
		t.Fatalf("after the new secret: %+v", s)
	}
	// no secret file, and a type this server does not build: no request
	get("demo-nosecret-proxy", "docs/a.txt", 404, 0)
	get("demo-ntlm-proxy", "docs/a.txt", 404, 0)
	for name, want := range map[string]string{"demo-nosecret-proxy": "secrets/remote/demo-nosecret-proxy", "demo-ntlm-proxy": "ntlm"} {
		if s := u.state(name); s.Status != proxy.NeedsSecret || !strings.Contains(s.Reason, want) {
			t.Fatalf("%s: %+v", name, s)
		}
	}
	text := strings.Join(append(log.all(), bodies...), "\n")
	if found := leaked(text); len(found) != 0 {
		t.Fatalf("the log or an answer holds %v:\n%s", found, text)
	}
	if !strings.Contains(text, "demo-wrong-proxy") {
		t.Fatalf("the log does not name the refused secret:\n%s", text)
	}
}
