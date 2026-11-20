package server

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// Bodies in parallel (row Bodies in parallel of the 0.2.0 plan): the free
// space is measured once per body, so the room above the floor must count
// the bytes of the other bodies in flight. Two bodies that each fit the
// room alone stop at the floor together, with a declared length and
// without one; once a body ends, its bytes no longer hold room.
func TestTwoBodiesTogetherStopAtTheFloor(t *testing.T) {
	w := startREST(t, restAttributes())
	const room = 4096
	floor := int64(repos.DefaultFreeSpaceFloor)
	w.srv.writes.freeSpace = func(string) (uint64, error) { return uint64(floor) + room, nil }
	threeQuarters := strings.Repeat("q", room*3/4)
	noSpace := func(t *testing.T, what string, err error) {
		t.Helper()
		var refusal *writeRefusal
		if !errors.As(err, &refusal) || refusal.status != http.StatusInsufficientStorage || refusal.rule != ruleNoSpace {
			t.Fatalf("%s: %v, want the refusal 507 %s", what, err, ruleNoSpace)
		}
	}

	t.Run("without a length", func(t *testing.T) {
		first, err := w.srv.checkSpace(-1, floor)
		if err != nil {
			t.Fatal(err)
		}
		second, err := w.srv.checkSpace(-1, floor)
		if err != nil {
			t.Fatal(err)
		}
		if data, err := io.ReadAll(first.reader(strings.NewReader(threeQuarters))); err != nil || len(data) != len(threeQuarters) {
			t.Fatalf("the first body alone: %d bytes, %v", len(data), err)
		}
		_, err = io.ReadAll(second.reader(strings.NewReader(threeQuarters)))
		noSpace(t, "the second body beside the first", err)
		first.release()
		second.release()
		third, err := w.srv.checkSpace(-1, floor)
		if err != nil {
			t.Fatal(err)
		}
		defer third.release()
		if data, err := io.ReadAll(third.reader(strings.NewReader(threeQuarters))); err != nil || len(data) != len(threeQuarters) {
			t.Fatalf("a body after both ended: %d bytes, %v", len(data), err)
		}
	})

	t.Run("with a length", func(t *testing.T) {
		first, err := w.srv.checkSpace(room*3/4, floor)
		if err != nil {
			t.Fatal(err)
		}
		_, err = w.srv.checkSpace(room*3/4, floor)
		noSpace(t, "the second body beside the first", err)
		first.release()
		second, err := w.srv.checkSpace(room*3/4, floor)
		if err != nil {
			t.Fatalf("the second body after the first ended: %v", err)
		}
		second.release()
	})

	if held := w.srv.writes.space.inFlight(); held != 0 {
		t.Fatalf("%d bytes held after every body ended", held)
	}
	// a write through ingest gives its room back once it ends
	if r := do(t, w.addr(), "PUT", "/repository/demo-raw/space/a.bin", []byte(threeQuarters), "basic", asAdmin); r.status != http.StatusCreated {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if r := chunked(t, w.addr(), "PUT", "/repository/demo-raw/space/b.bin", []byte(threeQuarters), "basic", asAdmin); r.status != http.StatusCreated {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if held := w.srv.writes.space.inFlight(); held != 0 {
		t.Fatalf("%d bytes held after two writes", held)
	}
}
