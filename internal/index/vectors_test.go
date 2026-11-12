package index_test

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/stores"
)

// The vectors come from tools/vectors/index.py, which runs the same steps on
// artifact_repository.index.Index of the reference.
type vectorFile struct {
	Failure string       `json:"failure"`
	Cases   []vectorCase `json:"cases"`
	File    struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
		Steps  []step `json:"steps"`
	} `json:"file"`
}

type vectorCase struct {
	Name  string `json:"name"`
	Steps []step `json:"steps"`
}

type step struct {
	Op        string          `json:"op"`
	Store     string          `json:"store"`
	Repo      string          `json:"repo"`
	Path      string          `json:"path"`
	Directory string          `json:"directory"`
	Limit     *int            `json:"limit"`
	Offset    *int            `json:"offset"`
	Entries   []entryJSON     `json:"entries"`
	Fail      bool            `json:"fail"`
	Second    bool            `json:"second"`
	Hints     [][2]string     `json:"hints"`
	Result    json.RawMessage `json:"result"`
}

type entryJSON struct {
	Repo        string  `json:"repo"`
	Path        string  `json:"path"`
	Size        int64   `json:"size"`
	SHA1        *string `json:"sha1"`
	ContentType *string `json:"content_type"`
	CreatedMS   int64   `json:"created_ms"`
	Location    string  `json:"location"`
}

func loadVectors(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/index.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) == 0 || len(v.File.Steps) == 0 || v.Failure == "" {
		t.Fatal("vector file holds no cases")
	}
	return &v
}

// generic decodes JSON into maps, slices, strings, json.Number, bools and nil.
func generic(t *testing.T, data []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func normalize(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return generic(t, data)
}

func assetJSON(a *index.Asset) any {
	if a == nil {
		return nil
	}
	return map[string]any{
		"repo": a.Repo, "path": a.Path, "store": a.Store, "location": a.Location, "size": a.Size,
		"sha1": a.SHA1, "content_type": a.ContentType, "created_ms": a.CreatedMS,
	}
}

func childrenJSON(rows []index.Child) any {
	out := []any{}
	for _, c := range rows {
		out = append(out, map[string]any{"name": c.Name, "is_dir": c.IsDir, "size": c.Size, "created_ms": c.CreatedMS})
	}
	return out
}

func repoStatsJSON(rows []index.RepoStat) any {
	out := []any{}
	for _, s := range rows {
		out = append(out, map[string]any{"repo": s.Repo, "files": s.Files, "bytes": s.Bytes, "newest_ms": s.NewestMS})
	}
	return out
}

func storeStatsJSON(rows []index.StoreStat) any {
	out := []any{}
	for _, s := range rows {
		out = append(out, map[string]any{"store": s.Store, "files": s.Files, "bytes": s.Bytes})
	}
	return out
}

// dumpFile reads schema, journal mode and every row of every table, as the
// dump of tools/vectors/index.py does with sqlite3.
func dumpFile(t *testing.T, path string) any {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	query := func(text string) [][]any {
		rows, err := db.Query(text)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		out := [][]any{}
		for rows.Next() {
			cells := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range cells {
				targets[i] = &cells[i]
			}
			if err := rows.Scan(targets...); err != nil {
				t.Fatal(err)
			}
			out = append(out, cells)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	schema := query("SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY type, name")
	tables := []any{}
	for _, row := range query("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name") {
		name := row[0].(string)
		rows, err := db.Query(fmt.Sprintf(`SELECT * FROM "%s" LIMIT 0`, name))
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		order := make([]string, len(columns))
		for i := range order {
			order[i] = fmt.Sprint(i + 1)
		}
		tables = append(tables, []any{name, query(fmt.Sprintf(`SELECT * FROM "%s" ORDER BY %s`, name, strings.Join(order, ", ")))})
	}
	journal := query("PRAGMA journal_mode")[0][0]
	return map[string]any{"schema": schema, "tables": tables, "journal_mode": journal}
}

// runner replays steps on a Go index.
type runner struct {
	t       *testing.T
	path    string
	ix      *index.Index
	failure error
}

func openRunner(t *testing.T, path, failure string) *runner {
	t.Helper()
	ix, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &runner{t: t, path: path, ix: ix, failure: errors.New(failure)}
	t.Cleanup(func() { r.ix.Close() })
	return r
}

func entriesOf(rows []entryJSON) []stores.Entry {
	out := make([]stores.Entry, 0, len(rows))
	for _, e := range rows {
		out = append(out, stores.Entry{
			Repo: e.Repo, Path: e.Path, Size: e.Size, SHA1: e.SHA1, ContentType: e.ContentType,
			CreatedMS: e.CreatedMS, Location: e.Location,
		})
	}
	return out
}

// errorResult is the result of a call that raised in Python.
func errorResult(t *testing.T, raw json.RawMessage) (string, bool) {
	t.Helper()
	var probe struct {
		Error *string `json:"error"`
	}
	if raw[0] != '{' || json.Unmarshal(raw, &probe) != nil || probe.Error == nil {
		return "", false
	}
	return *probe.Error, true
}

func (r *runner) must(err error) {
	r.t.Helper()
	if err != nil {
		r.t.Fatal(err)
	}
}

func (r *runner) run(s step) any {
	r.t.Helper()
	target := r.ix
	if s.Second {
		second, err := index.Open(r.path)
		r.must(err)
		defer func() { r.must(second.Close()) }()
		target = second
	}
	_, wantsError := errorResult(r.t, s.Result)
	answer := func(v any, err error) any {
		r.t.Helper()
		if wantsError {
			if err == nil {
				r.t.Fatalf("%s: no error, Python raised %s", s.Op, s.Result)
			}
			return nil
		}
		r.must(err)
		return v
	}
	switch s.Op {
	case "replace_store":
		rows := entriesOf(s.Entries)
		written, err := target.ReplaceStore(s.Store, func(yield func(stores.Entry) error) error {
			for _, e := range rows {
				if err := yield(e); err != nil {
					return err
				}
			}
			if s.Fail {
				return r.failure
			}
			return nil
		})
		if s.Fail {
			if !errors.Is(err, r.failure) {
				r.t.Fatalf("replace_store: error %v, want the error of the entries", err)
			}
			return map[string]any{"error": err.Error()}
		}
		r.must(err)
		return written
	case "remove_store":
		r.must(target.RemoveStore(s.Store))
		return nil
	case "set_hints":
		hints := map[string]string{}
		for _, pair := range s.Hints {
			hints[pair[0]] = pair[1]
		}
		r.must(target.SetHints(s.Store, hints))
		return nil
	case "drop":
		r.must(target.Drop(s.Repo, s.Path, s.Store))
		return nil
	case "lookup":
		a, err := target.Lookup(s.Repo, s.Path)
		return answer(assetJSON(a), err)
	case "children":
		limit, offset := index.DefaultLimit, 0
		if s.Limit != nil {
			limit = *s.Limit
		}
		if s.Offset != nil {
			offset = *s.Offset
		}
		rows, err := target.Children(s.Repo, s.Directory, limit, offset)
		return answer(childrenJSON(rows), err)
	case "is_dir":
		return answer(target.IsDir(s.Repo, s.Directory))
	case "repo_stats":
		rows, err := target.RepoStats()
		return answer(repoStatsJSON(rows), err)
	case "store_stats":
		rows, err := target.StoreStats()
		return answer(storeStatsJSON(rows), err)
	case "repo_names":
		return answer(target.RepoNames())
	case "hints":
		hints, err := target.Hints()
		r.must(err)
		var pairs [][2]string
		if err := json.Unmarshal(s.Result, &pairs); err != nil {
			r.t.Fatal(err)
		}
		want := map[string]string{}
		for _, pair := range pairs {
			want[pair[0]] = pair[1]
		}
		if !reflect.DeepEqual(hints, want) {
			r.t.Fatalf("hints %v, want %v", hints, want)
		}
		return nil
	case "generation":
		return answer(target.Generation())
	case "reopen":
		r.must(r.ix.Close())
		ix, err := index.Open(r.path)
		r.must(err)
		r.ix = ix
		return nil
	case "dump":
		return dumpFile(r.t, r.path)
	}
	r.t.Fatalf("unknown op %q", s.Op)
	return nil
}

// difference names the first place where got and want differ.
func difference(at string, got, want any) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			break
		}
		for _, key := range slices.Sorted(maps.Keys(w)) {
			if !reflect.DeepEqual(g[key], w[key]) {
				return difference(at+"."+key, g[key], w[key])
			}
		}
		for key := range g {
			if _, ok := w[key]; !ok {
				return fmt.Sprintf("%s.%s: extra key", at, key)
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			break
		}
		for i := range min(len(g), len(w)) {
			if !reflect.DeepEqual(g[i], w[i]) {
				return difference(fmt.Sprintf("%s[%d]", at, i), g[i], w[i])
			}
		}
		if len(g) != len(w) {
			gotText, _ := json.Marshal(g[min(len(g), len(w)):])
			wantText, _ := json.Marshal(w[min(len(g), len(w)):])
			return fmt.Sprintf("%s: %d items, want %d; rest got %s want %s", at, len(g), len(w), gotText, wantText)
		}
	}
	gotText, _ := json.Marshal(got)
	wantText, _ := json.Marshal(want)
	return fmt.Sprintf("%s: got %s, want %s", at, gotText, wantText)
}

func (r *runner) replay(steps []step) {
	r.t.Helper()
	for number, s := range steps {
		got := r.run(s)
		if _, raised := errorResult(r.t, s.Result); s.Op == "hints" || (raised && s.Op != "replace_store") {
			continue // run compared it
		}
		if want, have := generic(r.t, s.Result), normalize(r.t, got); !reflect.DeepEqual(have, want) {
			r.t.Fatalf("step %d %s(%s %s %s %s): %s", number, s.Op, s.Store, s.Repo, s.Path, s.Directory, difference("", have, want))
		}
	}
}

func TestVectorCases(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			r := openRunner(t, filepath.Join(t.TempDir(), "index.sqlite"), v.Failure)
			r.replay(c.Steps)
		})
	}
}

// TestPythonIndexFile opens an index the Python reference wrote by scanning
// two stores, reads the answers the Python index gave on the same file, then
// writes to it and compares the rows with those Python wrote.
func TestPythonIndexFile(t *testing.T) {
	v := loadVectors(t)
	data, err := os.ReadFile(filepath.Join("../../testdata/vectors", v.File.Name))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != v.File.SHA256 {
		t.Fatalf("%s has sha256 %s, the vectors were made from %s", v.File.Name, got, v.File.SHA256)
	}
	path := filepath.Join(t.TempDir(), "data", "index.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o666); err != nil {
		t.Fatal(err)
	}
	r := openRunner(t, path, v.Failure)
	r.replay(v.File.Steps)
}
