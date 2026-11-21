package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/rest"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The second check of the action under the lock of the path (finding S3 of
// review 4, row Second check of the action of the 0.2.0 plan): the route
// asked add or edit from the row of its moment, and the write asks again
// from the row it finds under the lock, in both directions.

const asEditOnly = "demo-edit-only:edit-only-pw" // edit, read and browse of demo-raw and demo-docker, no add

// withEditOnly gives the fake security of a running server the user
// asEditOnly.
func (w *writing) withEditOnly() {
	w.srv.sec.(*fakeSecurity).addBasic(asEditOnly, subject("demo-edit-only", "",
		rest.ViewPermission("raw", "demo-raw", "edit"), rest.ViewPermission("raw", "demo-raw", "read"),
		rest.ViewPermission("docker", "demo-docker", "edit"), rest.ViewPermission("docker", "demo-docker", "read")))
}

// deleteMeanwhile removes the row of a path and marks its blob, as erase
// does, for a test that holds the lock of the path.
func (w *writing) deleteMeanwhile(t *testing.T, repo, path string) {
	t.Helper()
	row, err := w.srv.rp.index.Lookup(repo, path)
	if err != nil || row == nil {
		t.Fatalf("the row of %s/%s: %+v %v", repo, path, row, err)
	}
	if _, err := w.srv.rp.index.DeleteWithin(repo, path, row.Store, indexWaitOf(), func() error {
		return w.srv.writes.mark(row.Store, row.Location, reasonDeleted)
	}); err != nil {
		t.Fatal(err)
	}
}

// demo-edit-only may replace a file of demo-raw, so the route of a PUT over
// it asks edit and lets it through. While the PUT waits for the lock of its
// path, a delete removes the path: under the lock the write would add it,
// which the caller may not, so it is refused with 403 and the path stays
// deleted. Without the race the same user replaces the file.
func TestAnEditOnlyPutDoesNotAddAPathDeletedMeanwhile(t *testing.T) {
	w := startREST(t, restAttributes())
	w.withEditOnly()
	if r := do(t, w.addr(), "PUT", "/repository/demo-raw/race/b.txt", []byte("first"), "basic", asAdmin); r.status != http.StatusCreated {
		t.Fatalf("the first PUT: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "PUT", "/repository/demo-raw/race/b.txt", []byte("second"), "basic", asEditOnly); r.status != http.StatusCreated {
		t.Fatalf("a PUT over a held path with edit only: %d %q", r.status, r.body)
	}
	if r := w.fetch("demo-raw", "race/b.txt"); string(r.body) != "second" {
		t.Fatalf("%d %q", r.status, r.body)
	}
	unlock := w.srv.writes.locks.lock("demo-raw", "race/b.txt")
	answers := sendAsync(w.addr(), "PUT", "/repository/demo-raw/race/b.txt", asEditOnly, []byte("mine"))
	waitFor(t, "the PUT at the lock of its path", func() bool { return w.waitsAt("demo-raw", "race/b.txt") })
	w.deleteMeanwhile(t, "demo-raw", "race/b.txt")
	unlock()
	if r := received(t, answers); r.status != http.StatusForbidden {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if r := w.fetch("demo-raw", "race/b.txt"); r.status != http.StatusNotFound {
		t.Fatalf("the deleted path came back: %d %q", r.status, r.body)
	}
	if files := w.workFiles(); len(files) != 0 {
		t.Fatalf("work files %v", files)
	}
}

// The tag of a manifest push asks the action again under the lock of the
// tag path, in both directions. demo-adder pushes a new tag, which the
// route asks add of; another push writes the tag meanwhile, and the push
// is refused with 403 DENIED, the tag keeping the other bytes. demo-edit-only
// pushes over a tag the route finds, which it asks edit of; a delete removes
// the tag meanwhile, and the push is refused with 403 DENIED, the tag
// staying deleted.
func TestTheDockerTagAsksItsActionAgainUnderTheLock(t *testing.T) {
	const tagPath = "v2/app/manifests/1.0"
	push := func(w *writing, who string, manifest []byte) <-chan *answer {
		return sendAsync(w.port("demo-docker"), "PUT", "/v2/app/manifests/1.0", who, manifest, "Content-Type", dockerManifestType)
	}
	t.Run("add only", func(t *testing.T) {
		w := startOCI(t, nil)
		mine, theirs := w.pushImage(t, "mine"), w.pushImage(t, "theirs")
		unlock := w.srv.writes.locks.lock("demo-docker", tagPath)
		answers := push(w, asAdder, mine)
		waitFor(t, "the push at the lock of its tag", func() bool { return w.waitsAt("demo-docker", tagPath) })
		written, err := w.srv.writes.writer.Write(strings.NewReader(string(theirs)), stores.Attributes{Repo: "demo-docker", Path: tagPath,
			ContentType: dockerManifestType, CreatedBy: "demo-admin"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.srv.rp.index.Put("local-write", written.Entry); err != nil {
			t.Fatal(err)
		}
		unlock()
		if r := received(t, answers); r.status != http.StatusForbidden || errorCodeOf(t, r.body) != "DENIED" {
			t.Fatalf("%d %q", r.status, r.body)
		}
		if r := do(t, w.port("demo-docker"), "GET", "/v2/app/manifests/1.0", nil, "basic", asAdmin,
			"Accept", dockerManifestType); r.status != http.StatusOK || string(r.body) != string(theirs) {
			t.Fatalf("%d %q", r.status, r.body)
		}
	})
	t.Run("edit only", func(t *testing.T) {
		w := startOCI(t, nil)
		w.withEditOnly()
		first, second := w.pushImage(t, "first"), w.pushImage(t, "second")
		if r := do(t, w.port("demo-docker"), "PUT", "/v2/app/manifests/1.0", first, "basic", asAdmin, "Content-Type", dockerManifestType); r.status != http.StatusCreated {
			t.Fatalf("the first push: %d %q", r.status, r.body)
		}
		unlock := w.srv.writes.locks.lock("demo-docker", tagPath)
		answers := push(w, asEditOnly, second)
		waitFor(t, "the push at the lock of its tag", func() bool { return w.waitsAt("demo-docker", tagPath) })
		w.deleteMeanwhile(t, "demo-docker", tagPath)
		unlock()
		if r := received(t, answers); r.status != http.StatusForbidden || errorCodeOf(t, r.body) != "DENIED" {
			t.Fatalf("%d %q", r.status, r.body)
		}
		if row, _ := w.srv.rp.index.Lookup("demo-docker", tagPath); row != nil {
			t.Fatalf("the deleted tag came back: %+v", row)
		}
		if row, _ := w.srv.rp.index.Lookup("demo-docker", "v2/app/manifests/"+sha256Digest(second)); row == nil {
			t.Fatal("the digest blob of the push is missing")
		}
	})
}
