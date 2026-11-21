package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const blobStoresPath = "/service/rest/v1/blobstores"

// storeNames are the names of GET /v1/blobstores.
func storeNames(t *testing.T, a *secureApp) map[string]map[string]any {
	t.Helper()
	r := a.send("GET", blobStoresPath, "basic", asAdmin)
	status(t, r, 200)
	out := map[string]map[string]any{}
	for _, item := range r.json(t).([]any) {
		m := item.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out
}

// The writes of the blob stores (leg 6-stores): with writes off and on, a
// file store is created below <data>/stores/, read, saved and deleted; the
// store of this server and a store of config.json are never deleted.
func TestBlobStoreWrites(t *testing.T) {
	for _, writes := range []bool{false, true} {
		sec := newFakeSecurity()
		a := &secureApp{app: startWriting(t, sec, writes), sec: sec}
		data := a.srv.rp.cfg.DataDir
		send := func(method, target, body string) *answer {
			return do(t, a.addr(), method, target, []byte(body), "basic", asAdmin, "Content-Type", jsonType)
		}
		if r := send("POST", blobStoresPath+"/file", `{"name":"demo-made","path":null,"softQuota":null}`); r.status != 204 || len(r.body) != 0 {
			t.Fatalf("writes %v: create: %d %q", writes, r.status, r.body)
		}
		real, _ := filepath.EvalSymlinks(data)
		made := filepath.Join(real, "stores", "demo-made")
		if info, err := os.Stat(made); err != nil || !info.IsDir() {
			t.Fatalf("the folder of the store: %v", err)
		}
		got := a.send("GET", blobStoresPath+"/file/demo-made", "basic", asAdmin)
		if got.status != 200 || got.object(t)["path"] != made {
			t.Fatalf("read: %d %q", got.status, got.body)
		}
		if m := storeNames(t, a)["demo-made"]; m == nil || m["unavailable"] != false || m["type"] != "File" {
			t.Fatalf("the list: %v", storeNames(t, a))
		}
		if r := send("POST", blobStoresPath+"/file", `{"name":"demo-second","path":"demo-second-files","softQuota":{"type":"spaceUsedQuota","limit":2097152}}`); r.status != 204 {
			t.Fatalf("create with a path: %d %q", r.status, r.body)
		}
		if r := send("PUT", blobStoresPath+"/file/demo-made", `{"path":"`+made+`","softQuota":{"type":"spaceRemainingQuota","limit":1048576}}`); r.status != 204 {
			t.Fatalf("save: %d %q", r.status, r.body)
		}
		if q := a.send("GET", blobStoresPath+"/file/demo-made", "basic", asAdmin).object(t)["softQuota"]; q == nil {
			t.Fatal("the saved quota is not read back")
		}
		for _, c := range []struct {
			method, target, body string
			status               int
		}{
			{"POST", blobStoresPath + "/file", `{"name":"demo-made"}`, 409},
			{"POST", blobStoresPath + "/file", `{"name":"local-write"}`, 409},
			{"POST", blobStoresPath + "/file", `{"name":"demo-out","path":"` + filepath.Join(data, "outside") + `"}`, 400},
			{"POST", blobStoresPath + "/file", `{"name":"demo-in","path":"demo-made"}`, 400},
			{"POST", blobStoresPath + "/file", `{"name":"demo-q","softQuota":{"type":"spaceUsedQuota","limit":5}}`, 400},
			{"POST", blobStoresPath + "/file", `{"name":"../x"}`, 400},
			{"PUT", blobStoresPath + "/file/demo-made", `{"path":"/elsewhere","softQuota":null}`, 400},
			{"PUT", blobStoresPath + "/file/demo-none", `{"softQuota":null}`, 404},
			{"DELETE", blobStoresPath + "/local-write", "", 409},
			{"DELETE", blobStoresPath + "/store-a", "", 409},
			{"DELETE", blobStoresPath + "/demo-none", "", 404},
		} {
			r := send(c.method, c.target, c.body)
			if r.status != c.status {
				t.Errorf("writes %v: %s %s %s: %d %q, want %d", writes, c.method, c.target, c.body, r.status, r.body, c.status)
			}
		}
		if _, err := os.Stat(filepath.Join(data, "outside")); err == nil {
			t.Fatal("a refused create made its folder")
		}
		for _, name := range []string{"demo-made", "demo-second"} {
			if r := send("DELETE", blobStoresPath+"/"+name, ""); r.status != 204 {
				t.Fatalf("delete %s: %d %q", name, r.status, r.body)
			}
		}
		if names := storeNames(t, a); names["demo-made"] != nil || names["demo-second"] != nil || names["store-a"] == nil {
			t.Fatalf("after the deletes: %v", names)
		}
	}
}

// A store that a repository names is in use (409); the reader and nobody
// get 403 and change nothing.
func TestBlobStoreWritesNeedTheirRight(t *testing.T) {
	sec := newFakeSecurity()
	a := newSecureApp(t, sec)
	if r := do(t, a.addr(), "DELETE", blobStoresPath+"/store-a", nil, "basic", asAdmin); r.status != 409 {
		t.Fatalf("a store config.json attaches: %d %q", r.status, r.body)
	}
	for _, who := range []string{asReader, asNobody} {
		for _, c := range [][3]string{
			{"POST", blobStoresPath + "/file", `{"name":"demo-x"}`},
			{"PUT", blobStoresPath + "/file/store-a", `{"softQuota":null}`},
			{"DELETE", blobStoresPath + "/store-a", ""},
			{"GET", "/api/blob-stores/store-a/deleted", ""},
		} {
			r := do(t, a.addr(), c[0], c[1], []byte(c[2]), "basic", who, "Content-Type", jsonType)
			if r.status != 403 {
				t.Errorf("%s %s %s: %d %q", who, c[0], c[1], r.status, r.body)
			}
		}
	}
	if updates, _ := sec.writes(); updates != 0 {
		t.Fatalf("%d writes", updates)
	}
	if r := do(t, a.addr(), "POST", blobStoresPath+"/file", []byte(`{"name":"demo-x"}`), "Cookie", "fake-session=s1"); r.status != 403 ||
		!strings.Contains(string(r.body), "request token") {
		t.Fatalf("a session without its token: %d %q", r.status, r.body)
	}
}

// GET /api/blob-stores/{name}/deleted counts the blobs of local-write that
// carry the delete mark: a raw PUT and DELETE leave one.
func TestDeletedBlobsOfTheWriteStore(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	count := func() map[string]any {
		r := do(t, w.addr(), "GET", "/api/blob-stores/local-write/deleted", nil, "basic", asAdmin)
		if r.status != 200 {
			t.Fatalf("%d %q", r.status, r.body)
		}
		return (&answer{status: r.status, body: r.body}).object(t)
	}
	if c := count(); c["name"] != "local-write" || c["blobCount"].(interface{ String() string }).String() != "0" || c["counting"] != false || c["countedAt"] == nil {
		t.Fatalf("before: %v", c)
	}
	if r := do(t, w.addr(), "PUT", "/repository/demo-raw/a.txt", []byte("seven b"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("PUT %d", r.status)
	}
	if r := do(t, w.addr(), "DELETE", "/repository/demo-raw/a.txt", nil, "basic", asAdmin); r.status != 204 {
		t.Fatalf("DELETE %d", r.status)
	}
	if c := count(); c["blobCount"].(interface{ String() string }).String() != "1" || c["bytes"].(interface{ String() string }).String() != "7" {
		t.Fatalf("after: %v", c)
	}
	if r := do(t, w.addr(), "GET", "/api/blob-stores/demo-none/deleted", nil, "basic", asAdmin); r.status != 404 {
		t.Fatalf("an unknown store: %d", r.status)
	}
}
