package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/npm"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The right a write asks of the view permission, add for a new path and edit
// for one the repository holds, is decided again under the lock of the path
// from the row that exists then (finding S3 of review 4): a subject with add
// and without edit never replaces a path that another write made while its
// body ran. The black box proof of the client PUT is
// test_add_without_edit_never_replaces_a_path_that_appeared_meanwhile in
// tests/write/test_refusals.py.

// waitsAt reports a write that waits for the lock of a path a test holds.
func (w *writing) waitsAt(repo, path string) bool {
	locks := &w.srv.writes.locks
	locks.mu.Lock()
	defer locks.mu.Unlock()
	held := locks.held[[2]string{repo, path}]
	return held != nil && held.users > 1
}

// sendAsync sends one request on a goroutine of its own and gives its
// answer, or nil after a failure of the transport, on the channel.
func sendAsync(addr, method, target, user string, body []byte, headers ...string) <-chan *answer {
	out := make(chan *answer, 1)
	go func() {
		r, err := http.NewRequest(method, "http://"+addr+target, bytes.NewReader(body))
		if err != nil {
			out <- nil
			return
		}
		name, password, _ := strings.Cut(user, ":")
		r.SetBasicAuth(name, password)
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		resp, err := noRedirect.Do(r)
		if err != nil {
			out <- nil
			return
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		out <- &answer{status: resp.StatusCode, header: resp.Header, body: data}
	}()
	return out
}

func received(t *testing.T, answers <-chan *answer) *answer {
	t.Helper()
	r := <-answers
	if r == nil {
		t.Fatal("the request failed in its transport")
	}
	return r
}

// The REST upload of demo-adder passed its check of the rights after its
// body; while it waits for the lock of its path, another write makes the
// path. Under the lock the upload finds the row and is refused with 403;
// the path keeps the other bytes, and no work file stays.
func TestAnAddOnlyUploadDoesNotReplaceAPathThatAppeared(t *testing.T) {
	w := startREST(t, restAttributes())
	unlock := w.srv.writes.locks.lock("demo-raw", "race/a.txt")
	form, contentType := multipartBody(t, formField("raw.directory", "race"), formFile("raw.asset1", "a.txt", []byte("mine")),
		formField("raw.asset1.filename", "a.txt"))
	answers := sendAsync(w.addr(), "POST", componentsPath+"?repository="+url.QueryEscape("demo-raw"), asAdder, form, "Content-Type", contentType)
	waitFor(t, "the upload at the lock of its path", func() bool { return w.waitsAt("demo-raw", "race/a.txt") })
	theirs, err := w.srv.writes.writer.Write(strings.NewReader("theirs"), stores.Attributes{Repo: "demo-raw", Path: "race/a.txt",
		CreatedBy: "demo-admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.rp.index.Put("local-write", theirs.Entry); err != nil {
		t.Fatal(err)
	}
	unlock()
	if r := received(t, answers); r.status != http.StatusForbidden {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if r := w.fetch("demo-raw", "race/a.txt"); string(r.body) != "theirs" {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if counts, _ := w.scan(); counts.Live != 1 || len(w.workFiles()) != 0 {
		t.Fatalf("%+v, work files %v", counts, w.workFiles())
	}
}

// A publish of demo-adder asked add, since the package had no document; while
// it waits for the lock of the package, another publish writes the document.
// Under the lock the publish finds the document and is refused with 403: a
// second version asks edit. No tarball of it is stored.
func TestAnAddOnlyPublishDoesNotChangeADocumentThatAppeared(t *testing.T) {
	w := startREST(t, restAttributes())
	repo := w.repo("demo-npm")
	unlock := w.srv.npmLock(repo, "demo-race")
	answers := sendAsync(w.addr(), "PUT", "/repository/demo-npm/demo-race", asAdder,
		npmPublishBody("demo-race", "1.0.0", npmTarballOf(t, "mine")), "Content-Type", jsonType)
	waitFor(t, "the publish at the lock of its package", func() bool { return w.waitsAt("demo-npm", "\x00npm\x00demo-race") })
	// the publish of admin, as npmPut makes it under the lock this test holds
	var theirs bytes.Buffer
	document, attachments, err := npm.ReadPublish(bytes.NewReader(npmPublishBody("demo-race", "0.9.0", npmTarballOf(t, "theirs"))),
		func(string) (io.Writer, error) { return &theirs, nil })
	if err != nil {
		t.Fatal(err)
	}
	published, err := npm.CheckPublish(document, "demo-race", []string{attachments[0].Name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.publishVersion(repo, "demo-race", nil, document, published, bytes.NewReader(theirs.Bytes()), int64(theirs.Len()),
		subject("demo-admin", "", "nexus:*"), "192.0.2.7"); err != nil {
		t.Fatal(err)
	}
	unlock()
	if r := received(t, answers); r.status != http.StatusForbidden {
		t.Fatalf("%d %q", r.status, r.body)
	}
	var served struct {
		Versions map[string]any `json:"versions"`
	}
	if r := w.fetch("demo-npm", "demo-race"); json.Unmarshal(r.body, &served) != nil || len(served.Versions) != 1 || served.Versions["0.9.0"] == nil {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if row, _ := w.srv.rp.index.Lookup("demo-npm", "demo-race/-/demo-race-1.0.0.tgz"); row != nil {
		t.Fatalf("the tarball of the refused publish %+v", row)
	}
	if files := w.workFiles(); len(files) != 0 {
		t.Fatalf("work files %v", files)
	}
}

// The REST npm upload asks edit for the package document once it exists, as
// the publish of the client route does: demo-adder may upload the first
// version of a package, not a second one.
func TestARESTNpmUploadAsksEditOfAnExistingPackage(t *testing.T) {
	w := startREST(t, restAttributes())
	if r := w.upload("demo-npm", asAdder, formFile("npm.asset", "package.tgz", packageTarball(t, "demo-two", "1.0.0"))); r.status != http.StatusNoContent {
		t.Fatalf("the first version: %d %q", r.status, r.body)
	}
	if r := w.upload("demo-npm", asAdder, formFile("npm.asset", "package.tgz", packageTarball(t, "demo-two", "1.1.0"))); r.status != http.StatusForbidden {
		t.Fatalf("a second version with add only: %d %q", r.status, r.body)
	}
	if row, _ := w.srv.rp.index.Lookup("demo-npm", "demo-two/-/demo-two-1.1.0.tgz"); row != nil {
		t.Fatalf("the tarball of the refused upload %+v", row)
	}
	if r := w.upload("demo-npm", asAdmin, formFile("npm.asset", "package.tgz", packageTarball(t, "demo-two", "1.1.0"))); r.status != http.StatusNoContent {
		t.Fatalf("a second version with edit: %d %q", r.status, r.body)
	}
	if files := w.workFiles(); len(files) != 0 {
		t.Fatalf("work files %v", files)
	}
}

// A dist-tag and a deprecate of demo-adder asked add, since the package had
// no document; while each waits for the lock of the package, a publish
// writes the document. Under the lock both find it and are refused with 403:
// a change of an existing document asks edit.
func TestAnAddOnlyChangeOfADocumentThatAppeared(t *testing.T) {
	for _, c := range []struct{ name, target, body string }{
		{"dist-tag", "/repository/demo-npm/-/package/demo-tagged/dist-tags/beta", `"0.9.0"`},
		{"deprecate", "/repository/demo-npm/demo-tagged", `{"name": "demo-tagged", "versions": {"0.9.0": {"deprecated": "use 1.x"}}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := startREST(t, restAttributes())
			repo := w.repo("demo-npm")
			unlock := w.srv.npmLock(repo, "demo-tagged")
			answers := sendAsync(w.addr(), "PUT", c.target, asAdder, []byte(c.body), "Content-Type", jsonType)
			waitFor(t, "the change at the lock of its package", func() bool { return w.waitsAt("demo-npm", "\x00npm\x00demo-tagged") })
			var theirs bytes.Buffer
			document, attachments, err := npm.ReadPublish(bytes.NewReader(npmPublishBody("demo-tagged", "0.9.0", npmTarballOf(t, "theirs"))),
				func(string) (io.Writer, error) { return &theirs, nil })
			if err != nil {
				t.Fatal(err)
			}
			published, err := npm.CheckPublish(document, "demo-tagged", []string{attachments[0].Name})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.srv.publishVersion(repo, "demo-tagged", nil, document, published, bytes.NewReader(theirs.Bytes()), int64(theirs.Len()),
				subject("demo-admin", "", "nexus:*"), "192.0.2.7"); err != nil {
				t.Fatal(err)
			}
			before, _ := w.srv.rp.index.Lookup("demo-npm", "demo-tagged")
			unlock()
			if r := received(t, answers); r.status != http.StatusForbidden {
				t.Fatalf("%d %q", r.status, r.body)
			}
			if after, _ := w.srv.rp.index.Lookup("demo-npm", "demo-tagged"); before == nil || after == nil || after.Location != before.Location {
				t.Fatalf("the document changed: %+v %+v", before, after)
			}
		})
	}
}
