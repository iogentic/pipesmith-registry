package cleanup

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const vectorFile = "../../testdata/vectors/cleanup/select.json"

// assetCase is an asset of a vector. LastDownload is the value of
// LastDownload the case expects; Repeat expands the asset.
type assetCase struct {
	Repository             string `json:"repository"`
	Path                   string `json:"path"`
	Group                  string `json:"group"`
	Name                   string `json:"name"`
	Version                string `json:"version"`
	BlobCreated            string `json:"blobCreated"`
	LastDownloaded         string `json:"lastDownloaded"`
	ImportedLastDownloaded string `json:"importedLastDownloaded"`
	LastDownload           string `json:"lastDownload"`
	Repeat                 int    `json:"repeat"`
}

// previewCase is the preview a case expects: its items in full, or their
// count with the first and the last path.
type previewCase struct {
	Total     int     `json:"total"`
	Truncated bool    `json:"truncated"`
	Items     *[]Item `json:"items"`
	Count     int     `json:"count"`
	First     string  `json:"first"`
	Last      string  `json:"last"`
}

type selectCase struct {
	Row           string          `json:"row"`
	Label         string          `json:"label"`
	Criteria      json.RawMessage `json:"criteria"`
	Now           string          `json:"now"`
	CatalogBuilt  string          `json:"catalogBuilt"`
	Assets        []assetCase     `json:"assets"`
	Runnable      bool            `json:"runnable"`
	Unknown       []string        `json:"unknown"`
	Selected      *[]string       `json:"selected"`
	SelectedCount int             `json:"selectedCount"`
	Preview       *previewCase    `json:"preview"`
}

// readVectors decodes select.json. Its key family must name the file, and
// an unknown key fails, so that a misspelt key cannot leave a case unread.
func readVectors(t *testing.T) (file struct {
	Family       string       `json:"family"`
	Note         string       `json:"note"`
	Now          string       `json:"now"`
	CatalogBuilt string       `json:"catalogBuilt"`
	Cases        []selectCase `json:"cases"`
}) {
	t.Helper()
	data, err := os.ReadFile(vectorFile)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		t.Fatalf("select.json: %v", err)
	}
	if file.Family != "select" || len(file.Cases) == 0 {
		t.Fatalf("select.json names the family %q and holds %d cases", file.Family, len(file.Cases))
	}
	return file
}

// instant is a time of a vector in RFC 3339; the empty text is the zero
// time.
func instant(t *testing.T, text string) time.Time {
	t.Helper()
	if text == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func (a assetCase) asset(t *testing.T) Asset {
	return Asset{Repository: a.Repository, Path: a.Path, Group: a.Group, Name: a.Name, Version: a.Version,
		BlobCreated: instant(t, a.BlobCreated), LastDownloaded: instant(t, a.LastDownloaded),
		ImportedLastDownloaded: instant(t, a.ImportedLastDownloaded)}
}

// assets are the assets of a case. An asset with a key repeat gives that
// many, numbered from the last one down, with {n} in its path, name and
// version replaced by the number in three digits.
func (c selectCase) assets(t *testing.T) []Asset {
	var out []Asset
	for _, a := range c.Assets {
		if a.Repeat == 0 {
			out = append(out, a.asset(t))
			continue
		}
		for i := a.Repeat - 1; i >= 0; i-- {
			n := fmt.Sprintf("%03d", i)
			each := a.asset(t)
			each.Path = strings.ReplaceAll(each.Path, "{n}", n)
			each.Name = strings.ReplaceAll(each.Name, "{n}", n)
			each.Version = strings.ReplaceAll(each.Version, "{n}", n)
			out = append(out, each)
		}
	}
	return out
}

// criteriaOf decodes criteria with numbers as json.Number when numbers is
// set, else as float64. Absent and null give nil.
func criteriaOf(t *testing.T, raw json.RawMessage, numbers bool) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var criteria map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if numbers {
		dec.UseNumber()
	}
	if err := dec.Decode(&criteria); err != nil {
		t.Fatal(err)
	}
	return criteria
}

func paths(list []Asset) []string {
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.Path
	}
	return out
}

func itemsOf(list []Asset) []Item {
	out := make([]Item, len(list))
	for i, a := range list {
		out[i] = Item{Repository: a.Repository, Group: a.Group, Name: a.Name, Version: a.Version, Path: a.Path}
	}
	return out
}

func TestSelectVectors(t *testing.T) {
	file := readVectors(t)
	counts := map[string]*[2]int{} // per row: cases that select an asset, cases that keep one
	for i, c := range file.Cases {
		in := Input{Assets: c.assets(t), CatalogBuilt: instant(t, cmp.Or(c.CatalogBuilt, file.CatalogBuilt)),
			Now: instant(t, cmp.Or(c.Now, file.Now))}
		before := slices.Clone(in.Assets)
		var first Selection
		for _, numbers := range []bool{false, true} {
			label := fmt.Sprintf("case %d (%s, %s, json.Number %v)", i, c.Row, c.Label, numbers)
			criteria := criteriaOf(t, c.Criteria, numbers)
			sel := Select(criteria, in)
			if !numbers {
				first = sel
			}
			got := paths(sel.Assets)
			switch {
			case sel.Runnable != c.Runnable || !slices.Equal(sel.Unknown, c.Unknown):
				t.Errorf("%s: runnable %v, unknown %q; want %v, %q", label, sel.Runnable, sel.Unknown, c.Runnable, c.Unknown)
			case c.Selected != nil && !slices.Equal(got, *c.Selected):
				t.Errorf("%s: selected %q, want %q", label, got, *c.Selected)
			case c.Selected == nil && (c.SelectedCount == 0 || len(got) != c.SelectedCount):
				t.Errorf("%s: selected %d, want %d", label, len(got), c.SelectedCount)
			}
			p := PreviewOf(criteria, in)
			n := min(len(sel.Assets), PreviewLimit)
			if p.Total != len(sel.Assets) || p.Truncated != (len(sel.Assets) > PreviewLimit) ||
				!slices.Equal(p.Items, itemsOf(sel.Assets[:n])) {
				t.Errorf("%s: the preview (total %d, truncated %v, %d items) does not list the selection of %d",
					label, p.Total, p.Truncated, len(p.Items), len(sel.Assets))
			}
			if w := c.Preview; w != nil {
				previewed := make([]string, len(p.Items))
				for j, item := range p.Items {
					previewed[j] = item.Path
				}
				switch {
				case p.Total != w.Total || p.Truncated != w.Truncated:
					t.Errorf("%s: total %d, truncated %v; want %d, %v", label, p.Total, p.Truncated, w.Total, w.Truncated)
				case p.Items == nil:
					t.Errorf("%s: the items are nil, not an empty list", label)
				case w.Items != nil && !slices.Equal(p.Items, *w.Items):
					t.Errorf("%s: items %+v, want %+v", label, p.Items, *w.Items)
				case w.Items == nil && (len(previewed) != w.Count || w.Count == 0 || previewed[0] != w.First ||
					previewed[w.Count-1] != w.Last):
					t.Errorf("%s: %d items from %v, want %d from %s to %s", label, len(previewed), previewed[:min(2, len(previewed))],
						w.Count, w.First, w.Last)
				}
			}
			if !reflect.DeepEqual(in.Assets, before) {
				t.Errorf("%s: Select changed the assets it was given", label)
			}
		}
		for _, a := range c.Assets {
			if a.LastDownload == "" {
				continue
			}
			if got := LastDownload(a.asset(t), in.CatalogBuilt); !got.Equal(instant(t, a.LastDownload)) {
				t.Errorf("case %d (%s): the last download of %s is %v, want %s", i, c.Label, a.Path, got, a.LastDownload)
			}
		}
		if counts[c.Row] == nil {
			counts[c.Row] = &[2]int{}
		}
		if len(first.Assets) > 0 {
			counts[c.Row][0]++
		}
		if len(first.Assets) < len(in.Assets) {
			counts[c.Row][1]++
		}
	}
	rows := []string{"Q9", "Q10", "Q11", "Q12", "Q13"}
	for _, row := range rows {
		if n := counts[row]; n == nil || n[0] == 0 || n[1] == 0 {
			t.Errorf("select.json: %s needs a case that selects an asset and one that keeps an asset, has %v", row, n)
		}
	}
	for row := range counts {
		if !slices.Contains(rows, row) {
			t.Errorf("select.json: cases of %q, which the file is not for", row)
		}
	}
}

// A preview and a run read the same selection (ruling Q14): on one input
// below the cap, whose assets both criteria select and keep, the paths of
// the preview are the paths of the selection, in its order.
func TestPreviewListsTheSelection(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	in := Input{CatalogBuilt: now.AddDate(0, -10, 0), Now: now}
	for i := range 40 {
		n := 39 - i
		a := Asset{Repository: "demo-maven", Path: fmt.Sprintf("com/example/q14/%02d/q14-%02d.jar", n, n),
			Group: "com.example", Name: "q14", Version: fmt.Sprintf("%02d", n), BlobCreated: now.AddDate(0, -3, 0)}
		switch i % 4 {
		case 1:
			a.BlobCreated = now.AddDate(0, 0, -1)
		case 2:
			a.LastDownloaded = now.AddDate(0, 0, -2)
		case 3:
			a.ImportedLastDownloaded = now.AddDate(0, 0, -60)
		}
		in.Assets = append(in.Assets, a)
	}
	criteria := map[string]any{"lastBlobUpdatedDays": 30.0, "lastDownloadedDays": 7.0}
	selected := paths(Select(criteria, in).Assets)
	p := PreviewOf(criteria, in)
	previewed := make([]string, len(p.Items))
	for i, item := range p.Items {
		previewed[i] = item.Path
	}
	if len(selected) != 20 {
		t.Fatalf("the input selects %d of %d assets, want 20, below the cap", len(selected), len(in.Assets))
	}
	if !slices.Equal(previewed, selected) || p.Total != len(selected) || p.Truncated {
		t.Errorf("preview %q (total %d, truncated %v), selection %q", previewed, p.Total, p.Truncated, selected)
	}
}

// A preview has the JSON names of the contract; an empty one has an empty
// list of items.
func TestPreviewJSON(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	criteria := map[string]any{"lastBlobUpdatedDays": 30.0}
	for _, c := range []struct {
		assets []Asset
		want   string
	}{
		{nil, `{"items":[],"total":0,"truncated":false}`},
		{[]Asset{{Repository: "demo-raw", Path: "com/example/files/readme.txt", BlobCreated: now.AddDate(-1, 0, 0)}},
			`{"items":[{"repository":"demo-raw","group":"","name":"","version":"","path":"com/example/files/readme.txt"}],` +
				`"total":1,"truncated":false}`},
	} {
		data, err := json.Marshal(PreviewOf(criteria, Input{Assets: c.assets, CatalogBuilt: now, Now: now}))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != c.want {
			t.Errorf("%s, want %s", data, c.want)
		}
	}
}

// Assets of one path in several repositories come in the order of the
// repositories, after the assets of a path before theirs.
func TestSelectionOrderOfOnePath(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, -3, 0)
	in := Input{Now: now, CatalogBuilt: now, Assets: []Asset{
		{Repository: "demo-maven-b", Path: "com/example/x.jar", BlobCreated: old},
		{Repository: "demo-maven-a", Path: "com/example/x.jar", BlobCreated: old},
		{Repository: "demo-maven-c", Path: "com/example/a.jar", BlobCreated: old},
	}}
	var got []string
	for _, a := range Select(map[string]any{"lastBlobUpdatedDays": 30.0}, in).Assets {
		got = append(got, a.Repository+" "+a.Path)
	}
	want := []string{"demo-maven-c com/example/a.jar", "demo-maven-a com/example/x.jar", "demo-maven-b com/example/x.jar"}
	if !slices.Equal(got, want) {
		t.Errorf("%q, want %q", got, want)
	}
}

// LastDownload and the assets of a selection give their times in UTC, at
// the same instant, also for times of another zone. The assets of the input
// keep their zone.
func TestTimesAreUTC(t *testing.T) {
	zone := time.FixedZone("demo+0230", 2*3600+30*60)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	in := Input{Now: now, CatalogBuilt: time.Date(2025, 6, 1, 0, 0, 0, 0, zone), Assets: []Asset{{
		Repository:             "demo-raw",
		Path:                   "com/example/a.txt",
		BlobCreated:            time.Date(2026, 1, 1, 10, 0, 0, 0, zone),
		LastDownloaded:         time.Date(2026, 2, 1, 10, 0, 0, 0, zone),
		ImportedLastDownloaded: time.Date(2026, 3, 1, 10, 0, 0, 0, zone),
	}, {
		Repository:  "demo-raw",
		Path:        "com/example/b.txt",
		BlobCreated: time.Date(2026, 1, 2, 10, 0, 0, 0, zone),
	}}}
	utc := func(label string, got, want time.Time) {
		t.Helper()
		if got.Location() != time.UTC || !got.Equal(want) {
			t.Errorf("%s: %v, want %v in UTC", label, got, want)
		}
	}
	utc("the import time", LastDownload(in.Assets[0], in.CatalogBuilt), in.Assets[0].ImportedLastDownloaded)
	utc("the build time", LastDownload(in.Assets[1], in.CatalogBuilt), in.CatalogBuilt)
	got := Select(map[string]any{"lastBlobUpdatedDays": 30.0, "lastDownloadedDays": 30.0}, in).Assets
	if len(got) != 2 {
		t.Fatalf("%d assets selected, want 2", len(got))
	}
	for i, a := range got {
		held := in.Assets[i]
		utc(a.Path+" BlobCreated", a.BlobCreated, held.BlobCreated)
		utc(a.Path+" LastDownloaded", a.LastDownloaded, held.LastDownloaded)
		utc(a.Path+" ImportedLastDownloaded", a.ImportedLastDownloaded, held.ImportedLastDownloaded)
		if a.Repository != held.Repository || a.Path != held.Path {
			t.Errorf("asset %d is %s %s, want %s %s", i, a.Repository, a.Path, held.Repository, held.Path)
		}
	}
	if !got[1].LastDownloaded.IsZero() || !got[1].ImportedLastDownloaded.IsZero() {
		t.Error("a zero time is no longer zero")
	}
	if in.Assets[0].BlobCreated.Location() != zone {
		t.Error("Select changed the zone of an asset of its input")
	}
}

// The age compares whole seconds and nanoseconds without an overflow. A
// number of days beyond what an int64 of seconds holds never holds; the
// zero time, the year 1, is older than any number of days below that.
func TestOlderThanAtTheEdges(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 500, time.UTC)
	day := 24 * time.Hour
	for _, c := range []struct {
		label string
		at    time.Time
		days  int64
		want  bool
	}{
		{"exactly one day", now.Add(-day), 1, false},
		{"one nanosecond beyond one day", now.Add(-day - time.Nanosecond), 1, true},
		{"one nanosecond short of one day, the nanoseconds borrowed", time.Date(2026, 11, 18, 12, 0, 0, 501, time.UTC), 1, false},
		{"one day and 999999999 nanoseconds, the nanoseconds borrowed", time.Date(2026, 11, 18, 11, 59, 59, 501, time.UTC), 1, true},
		{"a time after now", now.Add(time.Hour), 1, false},
		{"the zero time and 30 days", time.Time{}, 30, true},
		{"the zero time and the most days an int64 of seconds holds", time.Time{}, math.MaxInt64 / 86400, false},
		{"the zero time and 2^53 days", time.Time{}, 1 << 53, false},
	} {
		if got := olderThan(c.at, now, c.days); got != c.want {
			t.Errorf("%s: %v, want %v", c.label, got, c.want)
		}
	}
}
