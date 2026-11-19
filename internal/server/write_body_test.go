package server

import (
	"bytes"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// chunked sends one request whose body declares no length, as crane and
// skopeo send a PATCH and curl sends a body from a pipe: the transport sends
// it with Transfer-Encoding chunked. headers are pairs as do takes them.
func chunked(t *testing.T, addr, method, target string, body []byte, headers ...string) *answer {
	t.Helper()
	r, err := http.NewRequest(method, "http://"+addr+target, io.MultiReader(bytes.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "basic" {
			user, password, _ := strings.Cut(headers[i+1], ":")
			r.SetBasicAuth(user, password)
			continue
		}
		r.Header.Set(headers[i], headers[i+1])
	}
	resp, err := noRedirect.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return &answer{status: resp.StatusCode, header: resp.Header, body: data}
}

// The free space floor of W14 bounds a body without a declared length while
// it streams (finding S2 and assessment A5 of review 4): the check before
// the body had no size to add to the floor. On every write route the stream
// stops once it passes the room the store has above the floor: 507 with the
// text of that bound, no blob, no work file. The npm publish and the REST
// upload spooled the whole body first and refused it only afterwards, with
// the text of the check of a known size. A body inside the room is taken.
func TestABodyWithoutLengthStopsAtTheFloor(t *testing.T) {
	w := startREST(t, restAttributes())
	const room = 4096
	const bound = "the body passes the 4096 bytes"
	w.srv.writes.freeSpace = func(string) (uint64, error) { return uint64(repos.DefaultFreeSpaceFloor) + room, nil }
	big, small := bytes.Repeat([]byte("b"), 16*room), bytes.Repeat([]byte("s"), room/8)
	upload := "/repository/demo-docker/v2/app/blobs/uploads/"
	session := func() string {
		r := do(t, w.addr(), "POST", upload, nil, "basic", asAdmin)
		if r.status != http.StatusAccepted {
			t.Fatalf("POST of a session: %d %q", r.status, r.body)
		}
		return r.header.Get("Location")
	}
	routes := []struct {
		name string
		send func(body []byte) *answer
		ok   int
	}{
		{"raw PUT", func(body []byte) *answer {
			return chunked(t, w.addr(), "PUT", "/repository/demo-raw/room/a.bin", body, "basic", asAdmin)
		}, http.StatusCreated},
		{"maven2 PUT", func(body []byte) *answer {
			return chunked(t, w.addr(), "PUT", "/repository/demo-releases/com/example/demo-room/1.0.0/demo-room-1.0.0.jar", body, "basic", asAdmin)
		}, http.StatusCreated},
		{"npm publish", func(body []byte) *answer {
			return chunked(t, w.addr(), "PUT", "/repository/demo-npm/demo-room", npmPublishBody("demo-room", "1.0.0", body), "basic", asAdmin,
				"Content-Type", jsonType)
		}, http.StatusCreated},
		{"REST upload", func(body []byte) *answer {
			form, contentType := multipartBody(t, formField("raw.directory", "rest"), formFile("raw.asset1", "r.bin", body),
				formField("raw.asset1.filename", "r.bin"))
			return chunked(t, w.addr(), "POST", componentsPath+"?repository="+url.QueryEscape("demo-raw"), form, "basic", asAdmin,
				"Content-Type", contentType)
		}, http.StatusNoContent},
		{"Docker PATCH", func(body []byte) *answer {
			return chunked(t, w.addr(), "PATCH", session(), body, "basic", asAdmin)
		}, http.StatusAccepted},
		{"Docker closing PUT", func(body []byte) *answer {
			return chunked(t, w.addr(), "PUT", session()+digestQuery(body), body, "basic", asAdmin)
		}, http.StatusCreated},
	}
	for _, route := range routes {
		r := route.send(big)
		if files := w.workFiles(); len(files) != 0 {
			t.Errorf("%s: work files %v", route.name, files)
		}
		if r.status != http.StatusInsufficientStorage || !strings.Contains(string(r.body), bound) {
			t.Errorf("%s of %d bytes with %d bytes of room: %d %q", route.name, len(big), room, r.status, r.body)
			continue
		}
		if strings.HasPrefix(route.name, "Docker") && errorCodeOf(t, r.body) != "BLOB_UPLOAD_INVALID" {
			t.Errorf("%s: %q", route.name, r.body)
		}
	}
	if counts, _ := w.scan(); counts.Live != 0 {
		t.Fatalf("the refused bodies left %+v", counts)
	}
	for _, route := range routes {
		if r := route.send(small); r.status != route.ok {
			t.Errorf("%s of %d bytes: %d %q", route.name, len(small), r.status, r.body)
		}
	}
	if r := w.fetch("demo-raw", "room/a.bin"); r.status != http.StatusOK || !bytes.Equal(r.body, small) {
		t.Fatalf("%d, %d bytes", r.status, len(r.body))
	}
}

// A file system that reports 2^63 bytes or more free gives the room
// MaxInt64, and the bound of a read must not wrap there (finding 5 of the
// review of leg 4g): every body read through the room failed with a panic.
func TestTheRoomOfAHugeStore(t *testing.T) {
	room := &storeRoom{left: math.MaxInt64, refusal: errors.New("no room")}
	data, err := io.ReadAll(room.reader(strings.NewReader("a body")))
	if err != nil || string(data) != "a body" {
		t.Fatalf("%q %v", data, err)
	}
}
