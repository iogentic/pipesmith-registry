package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// vectorDir holds one vector file per rule family and answers.json.
const vectorDir = "../../testdata/vectors/admin/"

// readJSON decodes the vector file of a family into v. Its key family must
// name the file, and an unknown key fails, so that a misspelt key cannot
// leave a case unread.
func readJSON(t *testing.T, family string, v any) {
	t.Helper()
	data, err := os.ReadFile(vectorDir + family + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var head struct {
		Family string `json:"family"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		t.Fatalf("%s.json: %v", family, err)
	}
	if head.Family != family {
		t.Fatalf("%s.json names the family %q", family, head.Family)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s.json: %v", family, err)
	}
}

// wantRefusal is a refusal a vector expects: its code, the status of its
// answer, its field, the area of a 501, and the names its message holds, in
// this order.
type wantRefusal struct {
	Code   Code     `json:"code"`
	Status int      `json:"status"`
	Field  string   `json:"field"`
	Area   string   `json:"area"`
	Names  []string `json:"names"`
}

// checkRefusals compares the refusals of err with the expected ones, in
// their order, and the status that Answer gives for err.
func checkRefusals(t *testing.T, label string, err error, want *[]wantRefusal) {
	t.Helper()
	if want == nil {
		t.Fatalf("%s: the case has no key refusals", label)
	}
	var got []*Refusal
	if err != nil && !collect(err, &got) {
		t.Errorf("%s: %v is no refusal", label, err)
		return
	}
	if len(got) != len(*want) {
		t.Errorf("%s: %d refusals (%v), want %d", label, len(got), err, len(*want))
		return
	}
	status := 0
	for i, w := range *want {
		r := got[i]
		switch {
		case r.Code != w.Code || r.Field != w.Field || r.Area != w.Area:
			t.Errorf("%s: refusal %d is %q at %q in %q, want %q at %q in %q", label, i, r.Code, r.Field, r.Area,
				w.Code, w.Field, w.Area)
		case answers[r.Code].status != w.Status:
			t.Errorf("%s: %q answers %d, want %d", label, r.Code, answers[r.Code].status, w.Status)
		case r.Message == "" || strings.ContainsAny(r.Message, "\n\r"):
			t.Errorf("%s: the message %q is not one line", label, r.Message)
		case !holdsInOrder(r.Message, w.Names):
			t.Errorf("%s: the message %q does not hold %q in this order", label, r.Message, w.Names)
		}
		if status == 0 && w.Status != http.StatusBadRequest {
			status = w.Status
		}
	}
	if len(got) == 0 {
		return
	}
	if status == 0 {
		status = http.StatusBadRequest
	}
	if answered, _ := Answer(err); answered != status {
		t.Errorf("%s: Answer gives %d, want %d", label, answered, status)
	}
}

// returns gives the result of a rule and fails the test when the rule does
// not return within 10 s, as a walk that enters a record of a stored cycle
// again does not.
func returns(t *testing.T, label string, rule func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- rule() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: the rule did not return within 10 s", label)
		return nil
	}
}

// holdsInOrder reports whether every name is in the message, each after the
// one before it.
func holdsInOrder(message string, names []string) bool {
	rest := message
	for _, name := range names {
		i := strings.Index(rest, name)
		if i < 0 {
			return false
		}
		rest = rest[i+len(name):]
	}
	return true
}

// tally counts the accepted and the refused cases of each ruling of a file.
type tally map[string]*[2]int

func (tl tally) add(ruling string, refused bool) {
	if tl[ruling] == nil {
		tl[ruling] = &[2]int{}
	}
	if refused {
		tl[ruling][1]++
	} else {
		tl[ruling][0]++
	}
}

// check fails for a ruling of the file without an accepted and a refused
// case, and for a case of a ruling the file is not for.
func (tl tally) check(t *testing.T, family string, rulings ...string) {
	t.Helper()
	for _, ruling := range rulings {
		if n := tl[ruling]; n == nil || n[0] == 0 || n[1] == 0 {
			t.Errorf("%s.json: %s needs an accepted and a refused case, has %v", family, ruling, n)
		}
	}
	for ruling := range tl {
		if !slices.Contains(rulings, ruling) {
			t.Errorf("%s.json: cases of %q, which the file is not for", family, ruling)
		}
	}
}
