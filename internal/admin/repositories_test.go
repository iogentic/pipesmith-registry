package admin

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/authz"
)

func TestRepositoryVectors(t *testing.T) {
	var file struct {
		Family string              `json:"family"`
		Note   string              `json:"note"`
		Lookup map[string][]string `json:"lookup"`
		Cases  []struct {
			Row     string              `json:"row"`
			Op      string              `json:"op"`
			Segment string              `json:"segment"`
			Type    string              `json:"type"`
			Format  string              `json:"format"`
			Remote  *string             `json:"remote"`
			Held    []string            `json:"held"`
			Body    map[string]any      `json:"body"`
			Stored  map[string]any      `json:"stored"`
			Name    string              `json:"name"`
			Groups  map[string][]string `json:"groups"`
			Members map[string]string   `json:"members"`
			NewFmt  bool                `json:"newFormat"`
			Create  bool                `json:"create"`
			Secret  *struct {
				Action SecretAction `json:"action"`
				Value  string       `json:"value"`
			} `json:"secret"`
			Refusals *[]wantRefusal `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "repositories", &file)
	lookup := func(host string) ([]netip.Addr, error) {
		list, ok := file.Lookup[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var out []netip.Addr
		for _, a := range list {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
	stores := []string{WriteStore, "demo-store", "store-a", "store-b"}
	counts := tally{}
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s %s %s %v)", i, c.Row, c.Op, c.Segment, c.Type, c.Body)
		holds := func(p string) bool {
			return slices.ContainsFunc(c.Held, func(g string) bool { return authz.Implies(g, p) })
		}
		var err error
		switch c.Op {
		case "kind":
			format := FormatOfSegment(c.Segment)
			if format != c.Format {
				t.Errorf("%s: the segment names %q, want %q", label, format, c.Format)
			}
			_, err = CheckSegment(c.Segment, c.Type)
		case "remote":
			err = CheckRemote(c.Type, c.Remote)
		case "host":
			err = CheckRemoteHost(*c.Remote, lookup, holds)
		case "storage":
			err = CheckStorage(c.Type, c.Body, c.Stored, stores)
		case "server":
			err = CheckServerAttributes(c.Body, c.Stored, c.Create)
		case "docker-port":
			err = CheckDockerPort(c.Body, c.Stored)
		case "secret":
			var got UpstreamSecret
			got, err = UpstreamSecretOf(c.Body)
			if err == nil && (c.Secret == nil || got.Action != c.Secret.Action || got.Value != c.Secret.Value) {
				t.Errorf("%s: %+v, want %+v", label, got, c.Secret)
			}
		case "delete":
			err = CheckRepositoryDelete(c.Name, c.Groups)
		case "grant":
			err = CheckRepositoryGrant(c.Format, c.Name, c.NewFmt, holds)
		case "member-grant":
			err = CheckMemberGrant(c.Name, c.Members, holds)
		default:
			t.Fatalf("%s: unknown op", label)
		}
		checkRefusals(t, label, err, c.Refusals)
		if c.Refusals != nil {
			counts.add(c.Row, len(*c.Refusals) > 0)
		}
	}
	counts.check(t, "repositories", "A5", "ST8", "ST2", "ST7", "ST9", "ST5", "ST10", "ST12", "ST13")
}

// Every format of A5 is served by its segment, and the segment of a format
// is the one the REST paths use.
func TestRepositorySegments(t *testing.T) {
	for _, format := range RepositoryFormats {
		if got := FormatOfSegment(SegmentOf(format)); got != format {
			t.Errorf("%s: the segment %s names %s", format, SegmentOf(format), got)
		}
	}
	if SegmentOf("maven2") != "maven" {
		t.Errorf("maven2 has the segment %s", SegmentOf("maven2"))
	}
}

// The closed networks of ruling ST11 are the set of internal/proxy, which
// the dial guard and the rule of a create both read.
func TestClosedAddressVectors(t *testing.T) {
	var file struct {
		Family string `json:"family"`
		Note   string `json:"note"`
		Cases  []struct {
			Address string `json:"address"`
			Closed  bool   `json:"closed"`
		} `json:"cases"`
	}
	readJSON(t, "closed", &file)
	counts := map[bool]int{}
	for _, c := range file.Cases {
		a := netip.MustParseAddr(c.Address)
		if got := ClosedAddress(a); got != c.Closed {
			t.Errorf("%s: closed %v, want %v", c.Address, got, c.Closed)
		}
		counts[c.Closed]++
	}
	if counts[true] == 0 || counts[false] == 0 {
		t.Fatalf("closed.json needs closed and public rows: %v", counts)
	}
}

// The limits a create holds as its values are numbers, as internal/repos
// reads them (WriteSettings), not text.
func TestServerAttributeLimitsAreNumbers(t *testing.T) {
	for key, want := range map[string]int64{"max_body_bytes": 10 << 30, "free_space_floor_bytes": 2 << 30} {
		if got, ok := ServerAttributes[key].(int64); !ok || got != want {
			t.Errorf("%s: %#v, want the number %d", key, ServerAttributes[key], want)
		}
		if err := CheckServerAttributes(map[string]any{key: float64(want)}, nil, true); err != nil {
			t.Errorf("%s: the default sent as a number of a body is refused: %v", key, err)
		}
	}
}
