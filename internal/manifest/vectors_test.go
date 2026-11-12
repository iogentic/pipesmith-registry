package manifest_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/manifest"
	_ "modernc.org/sqlite"
)

// The vectors come from tools/vectors/manifest.py, which runs the same cases
// on artifact_repository/manifest.py of the reference.
type vectorFile struct {
	Unescape []struct {
		Text   string `json:"text"`
		Expect string `json:"expect"`
	} `json:"unescape"`
	Parse []struct {
		Name  string     `json:"name"`
		Line  string     `json:"line"`
		Row   *rowJSON   `json:"row"`
		Bad   bool       `json:"bad"`
		Error *errorJSON `json:"error"`
	} `json:"parse"`
	Digest []struct {
		Text   string `json:"text"`
		Expect string `json:"expect"`
	} `json:"digest"`
	Read    []readCase   `json:"read"`
	Lookup  []lookupCase `json:"lookup"`
	Stores  []storeCase  `json:"stores"`
	Table   tableJSON    `json:"table"`
	GoBuilt goBuiltJSON  `json:"go_built"`
}

type rowJSON struct {
	Repo        string      `json:"repo"`
	Path        string      `json:"path"`
	Size        json.Number `json:"size"`
	SHA1        *string     `json:"sha1"`
	ContentType *string     `json:"content_type"`
	CreatedMS   json.Number `json:"created_ms"`
}

type fileJSON struct {
	Path    string `json:"path"`
	Data    string `json:"data"`
	MtimeNS *int64 `json:"mtime_ns"`
	Dir     bool   `json:"dir"`
	Link    string `json:"link"`
}

type errorJSON struct {
	Class   string `json:"class"`
	OSError bool   `json:"oserror"`
	Message string `json:"message"`
}

type readCase struct {
	Name      string     `json:"name"`
	Files     []fileJSON `json:"files"`
	Source    string     `json:"source"`
	Rows      []rowJSON  `json:"rows"`
	Warnings  []string   `json:"warnings"`
	Error     *errorJSON `json:"error"`
	NeedsZstd bool       `json:"needs_zstd"`
	PathDir   string     `json:"path_dir"`
}

type lookupCase struct {
	Name  string       `json:"name"`
	Steps []lookupStep `json:"steps"`
}

type lookupStep struct {
	Op      string     `json:"op"`
	Files   []fileJSON `json:"files"`
	ID      string     `json:"id"`
	Source  string     `json:"source"`
	Cache   string     `json:"cache"`
	Cwd     bool       `json:"cwd"`
	Repo    string     `json:"repo"`
	Path    string     `json:"path"`
	Dir     string     `json:"dir"`
	MtimeNS int64      `json:"mtime_ns"`
	Row     *rowJSON   `json:"row"`
	Names   *[]string  `json:"names"`
	Result  *struct {
		SourceID string     `json:"source_id"`
		State    string     `json:"state"`
		Error    *errorJSON `json:"error"`
	} `json:"result"`
}

type tableJSON struct {
	File          string     `json:"file"`
	SHA256        string     `json:"sha256"`
	ManifestFiles []fileJSON `json:"manifest_files"`
	Source        string     `json:"source"`
	State         string     `json:"state"`
	Gets          []struct {
		Repo string   `json:"repo"`
		Path string   `json:"path"`
		Row  *rowJSON `json:"row"`
	} `json:"gets"`
	Dump json.RawMessage `json:"dump"`
}

type goBuiltJSON struct {
	Ran         bool   `json:"ran"`
	Binary      string `json:"binary"`
	Gap         string `json:"gap"`
	Failed      string `json:"failed"`
	Stderr      string `json:"stderr"`
	Adopted     bool   `json:"adopted"`
	SameAnswers bool   `json:"same_answers"`
	SameDump    bool   `json:"same_dump"`
	Answers     []struct {
		Repo string   `json:"repo"`
		Path string   `json:"path"`
		Row  *rowJSON `json:"row"`
	} `json:"answers"`
	CorruptZstd *struct {
		Code      int  `json:"code"`
		NamesPart bool `json:"names_part"`
		Tables    int  `json:"tables"`
	} `json:"corrupt_zstd"`
}

const vectorsDir = "../../testdata/vectors"

func load(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(vectorsDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func show(s *string) string {
	if s == nil {
		return "None"
	}
	return strconv.Quote(*s)
}

// sameNumber compares an int64 field with a Python int; a Python int beyond
// int64 must come with the Overflow flag.
func sameNumber(got int64, overflow bool, want json.Number) bool {
	n, err := strconv.ParseInt(string(want), 10, 64)
	if err != nil {
		return overflow
	}
	return got == n
}

func checkRow(t *testing.T, label string, got *manifest.Row, want *rowJSON) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("%s: got row %+v, want None", label, *got)
		}
		return
	}
	if got == nil {
		t.Errorf("%s: got no row, want %+v", label, *want)
		return
	}
	_, sizeErr := strconv.ParseInt(string(want.Size), 10, 64)
	_, createdErr := strconv.ParseInt(string(want.CreatedMS), 10, 64)
	if got.Overflow != (sizeErr != nil || createdErr != nil) {
		t.Errorf("%s: overflow %v, Python size %s created %s", label, got.Overflow, want.Size, want.CreatedMS)
	}
	if got.Repo != want.Repo || got.Path != want.Path || show(got.SHA1) != show(want.SHA1) ||
		show(got.ContentType) != show(want.ContentType) ||
		(sizeErr == nil && !sameNumber(got.Size, got.Overflow, want.Size)) ||
		(createdErr == nil && !sameNumber(got.CreatedMS, got.Overflow, want.CreatedMS)) {
		t.Errorf("%s:\n got %q %q %d %s %s %d\nwant %q %q %s %s %s %s", label,
			got.Repo, got.Path, got.Size, show(got.SHA1), show(got.ContentType), got.CreatedMS,
			want.Repo, want.Path, want.Size, show(want.SHA1), show(want.ContentType), want.CreatedMS)
	}
}

// kind is how Python code tells the error apart: FileNotFoundError, another
// OSError, or the class name.
func kind(e *errorJSON) string {
	switch {
	case e.Class == "FileNotFoundError":
		return "FileNotFoundError"
	case e.OSError:
		return "OSError"
	}
	return e.Class
}

func goKind(err error) string {
	var me *manifest.Error
	switch {
	case errors.As(err, &me):
		return me.Class
	case errors.Is(err, fs.ErrNotExist):
		return "FileNotFoundError"
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return "OSError"
	}
	return "Exception"
}

func checkError(t *testing.T, label, base string, err error, want *errorJSON) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Errorf("%s: error %v, want none", label, err)
		}
		return
	}
	if err == nil {
		t.Errorf("%s: no error, want %s: %s", label, want.Class, want.Message)
		return
	}
	got := strings.ReplaceAll(err.Error(), base, "<base>")
	if goKind(err) != kind(want) || got != want.Message {
		t.Errorf("%s: error %s %q, want %s %q", label, goKind(err), got, kind(want), want.Message)
	}
}

func materialize(t *testing.T, base string, files []fileJSON) {
	t.Helper()
	for _, f := range files {
		path := filepath.Join(base, f.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			t.Fatal(err)
		}
		switch {
		case f.Dir:
			if err := os.MkdirAll(path, 0o777); err != nil {
				t.Fatal(err)
			}
		case f.Link != "":
			if err := os.Symlink(f.Link, path); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.WriteFile(path, unb64(t, f.Data), 0o666); err != nil {
				t.Fatal(err)
			}
			if f.MtimeNS != nil {
				stamp := time.Unix(0, *f.MtimeNS)
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil || info.ModTime().UnixNano() != *f.MtimeNS {
					t.Fatalf("%s: the file system did not keep the modification time in nanoseconds", f.Path)
				}
			}
		}
	}
}

func haveZstd() bool {
	_, err := exec.LookPath("zstd")
	return err == nil
}

// logs collects the messages of the manifest logger.
type logs struct {
	mu       sync.Mutex
	messages []string
}

func (l *logs) Enabled(context.Context, slog.Level) bool { return true }

func (l *logs) Handle(_ context.Context, r slog.Record) error {
	logger := ""
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "logger" {
			logger = a.Value.String()
		}
		return true
	})
	if logger == "pipesmith_registry.manifest" && r.Level >= slog.LevelWarn {
		l.mu.Lock()
		l.messages = append(l.messages, r.Message)
		l.mu.Unlock()
	}
	return nil
}

func (l *logs) WithAttrs([]slog.Attr) slog.Handler { return l }

func (l *logs) WithGroup(string) slog.Handler { return l }

func captureLogs(t *testing.T) *logs {
	t.Helper()
	l := &logs{}
	saved := slog.Default()
	slog.SetDefault(slog.New(l))
	t.Cleanup(func() { slog.SetDefault(saved) })
	return l
}

func TestUnescapeVectors(t *testing.T) {
	for _, c := range load(t).Unescape {
		if got := manifest.Unescape(c.Text); got != c.Expect {
			t.Errorf("unescape(%q) = %q, want %q", c.Text, got, c.Expect)
		}
	}
}

func TestParseVectors(t *testing.T) {
	v := load(t)
	if len(v.Parse) == 0 {
		t.Fatal("no parse cases")
	}
	raised := 0
	for _, c := range v.Parse {
		row, bad, err := manifest.ParseLine(unb64(t, c.Line))
		if bad != c.Bad {
			t.Errorf("%s: bad %v, want %v", c.Name, bad, c.Bad)
		}
		checkError(t, c.Name, "\x00", err, c.Error) // no path in these messages
		if c.Error != nil {
			raised++
		}
		checkRow(t, c.Name, row, c.Row)
	}
	if raised < 4 {
		t.Fatalf("only %d lines raise", raised)
	}
}

func TestDigestVectors(t *testing.T) {
	for _, c := range load(t).Digest {
		text := string(unb64(t, c.Text))
		if got := manifest.Digest(text); got != c.Expect {
			t.Errorf("digest(%q) = %s, want %s", text, got, c.Expect)
		}
	}
}

func TestReadRowsVectors(t *testing.T) {
	v := load(t)
	if len(v.Read) == 0 {
		t.Fatal("no read cases")
	}
	for _, c := range v.Read {
		t.Run(c.Name, func(t *testing.T) {
			if c.NeedsZstd && !haveZstd() {
				t.Skip("the zstd program is not in PATH; this case needs it to read a .tsv.zst part")
			}
			base := t.TempDir()
			materialize(t, base, c.Files)
			if c.PathDir != "" {
				if err := os.Mkdir(filepath.Join(base, c.PathDir), 0o777); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", filepath.Join(base, c.PathDir))
			}
			captured := captureLogs(t)
			var rows []manifest.Row
			err := manifest.ReadRows(base+"/"+c.Source, func(r manifest.Row) error {
				rows = append(rows, r)
				return nil
			})
			checkError(t, c.Name, base, err, c.Error)
			if len(rows) != len(c.Rows) {
				t.Errorf("%d rows, want %d", len(rows), len(c.Rows))
			}
			for i := range min(len(rows), len(c.Rows)) {
				checkRow(t, c.Name+" row "+strconv.Itoa(i), &rows[i], &c.Rows[i])
			}
			var got []string
			for _, m := range captured.messages {
				got = append(got, strings.ReplaceAll(m, base, "<base>"))
			}
			if !slices.Equal(got, c.Warnings) {
				t.Errorf("warnings %q, want %q", got, c.Warnings)
			}
		})
	}
}

// sourceID is the first half of a table name: the digest of the resolved
// source. The case directory of a test has no links, so base plus the
// label is the resolved path.
func sourceID(base, label string) string {
	sum := sha256.Sum256([]byte(base + "/" + label))
	return hex.EncodeToString(sum[:])[:16]
}

type lookupRun struct {
	t       *testing.T
	base    string
	lookups map[string]*manifest.Lookup
	labels  map[string]bool
}

// shown writes a table name as the script does: "<id:L>-<state>.sqlite".
func (r *lookupRun) shown(name string) string {
	labels := make([]string, 0, len(r.labels))
	for label := range r.labels {
		labels = append(labels, label)
	}
	slices.Sort(labels)
	for _, label := range labels {
		if id := sourceID(r.base, label); strings.HasPrefix(name, id+"-") {
			return "<id:" + label + ">" + name[len(id):]
		}
	}
	return name
}

// real turns a shown name back into the path of this run.
func (r *lookupRun) real(shown string) string {
	for label := range r.labels {
		shown = strings.ReplaceAll(shown, "<id:"+label+">", sourceID(r.base, label))
	}
	return filepath.Join(r.base, shown)
}

func (r *lookupRun) step(label string, s lookupStep) {
	t := r.t
	switch s.Op {
	case "write":
		materialize(t, r.base, s.Files)
	case "open":
		var l *manifest.Lookup
		var err error
		if s.Cwd {
			t.Chdir(r.base)
			l, err = manifest.Open(s.Source, s.Cache)
		} else {
			l, err = manifest.Open(filepath.Join(r.base, s.Source), filepath.Join(r.base, s.Cache))
		}
		want := s.Result
		if want.Error != nil {
			checkError(t, label, r.base, err, want.Error)
			if l != nil {
				l.Close()
			}
			if _, statErr := os.Stat(filepath.Join(r.base, s.Source)); statErr == nil {
				real, _ := filepath.EvalSymlinks(filepath.Join(r.base, s.Source))
				rel, _ := filepath.Rel(r.base, real)
				r.labels[rel] = true
			}
			return
		}
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		r.labels[want.SourceID] = true
		r.lookups[s.ID] = l
		wantPath := filepath.Join(r.base, s.Cache, sourceID(r.base, want.SourceID)+"-"+want.State+".sqlite")
		if l.Path() != wantPath {
			t.Errorf("%s: table %s, want %s", label, l.Path(), wantPath)
		}
		if l.Source() != r.base+"/"+want.SourceID {
			t.Errorf("%s: source %s, want %s", label, l.Source(), r.base+"/"+want.SourceID)
		}
	case "get":
		row, err := r.lookups[s.ID].Get(s.Repo, s.Path)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		checkRow(t, label, row, s.Row)
	case "close":
		if err := r.lookups[s.ID].Close(); err != nil {
			t.Errorf("%s: %v", label, err)
		}
		delete(r.lookups, s.ID)
	case "listing":
		entries, err := os.ReadDir(filepath.Join(r.base, s.Dir))
		if s.Names == nil {
			if !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s: %s exists, want no directory", label, s.Dir)
			}
			return
		}
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		names := []string{}
		for _, e := range entries {
			names = append(names, r.shown(e.Name()))
		}
		slices.Sort(names)
		if !slices.Equal(names, *s.Names) {
			t.Errorf("%s: listing %q, want %q", label, names, *s.Names)
		}
	case "touch":
		stamp := time.Unix(0, s.MtimeNS)
		if err := os.Chtimes(r.real(s.Path), stamp, stamp); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	case "stat":
		info, err := os.Stat(r.real(s.Path))
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if info.ModTime().UnixNano() != s.MtimeNS {
			t.Errorf("%s: %s has mtime %d, want %d: the table was built again", label, s.Path,
				info.ModTime().UnixNano(), s.MtimeNS)
		}
	default:
		t.Fatalf("%s: unknown op %q", label, s.Op)
	}
}

func TestLookupVectors(t *testing.T) {
	v := load(t)
	if len(v.Lookup) == 0 {
		t.Fatal("no lookup cases")
	}
	for _, c := range v.Lookup {
		t.Run(c.Name, func(t *testing.T) {
			r := &lookupRun{t: t, base: t.TempDir(), lookups: map[string]*manifest.Lookup{}, labels: map[string]bool{}}
			t.Cleanup(func() {
				for _, l := range r.lookups {
					l.Close()
				}
			})
			captureLogs(t)
			for i, s := range c.Steps {
				r.step(c.Name+" step "+strconv.Itoa(i)+" "+s.Op, s)
			}
		})
	}
}

// dump reads a table as the script's dump() does, with the source row
// replaced by "<source>", as JSON text.
func dump(t *testing.T, path, source string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	query := func(q string, columns int) [][]any {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out [][]any
		for rows.Next() {
			values := make([]any, columns)
			pointers := make([]any, columns)
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			out = append(out, values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	meta := query("SELECT key, value FROM meta ORDER BY key", 2)
	for _, kv := range meta {
		if kv[0] == "source" && kv[1] == source {
			kv[1] = "<source>"
		}
	}
	pragmas := map[string]any{}
	for _, name := range []string{"page_size", "encoding", "auto_vacuum", "user_version", "application_id"} {
		var value any
		if err := db.QueryRow("PRAGMA " + name).Scan(&value); err != nil {
			t.Fatal(err)
		}
		pragmas[name] = value
	}
	header, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pragmas["file_format"] = []int{int(header[18]), int(header[19])}
	out, err := json.Marshal(map[string]any{
		"schema":  query("SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY type, name", 4),
		"rows":    query("SELECT * FROM rows ORDER BY 1, 2, 3, 4, 5, 6", 6),
		"meta":    meta,
		"pragmas": pragmas,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// canonical re-encodes JSON text with sorted keys and numbers kept.
func canonical(t *testing.T, data []byte) string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The table Python built is answered from as it is.
func TestPythonTableAnswers(t *testing.T) {
	v := load(t).Table
	data, err := os.ReadFile(filepath.Join(vectorsDir, v.File))
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != v.SHA256 {
		t.Fatalf("%s does not match the sha256 in manifest.json", v.File)
	}
	path := filepath.Join(t.TempDir(), v.File)
	copyFile(t, filepath.Join(vectorsDir, v.File), path)
	l, err := manifest.OpenTable(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, g := range v.Gets {
		row, err := l.Get(g.Repo, g.Path)
		if err != nil {
			t.Fatal(err)
		}
		checkRow(t, g.Repo+" "+g.Path, row, g.Row)
	}
}

// A data directory written by the Python server works here: Open takes the
// table Python built for the same manifest state and does not build again.
func TestOpenAdoptsThePythonTable(t *testing.T) {
	v := load(t).Table
	base := t.TempDir()
	materialize(t, base, v.ManifestFiles)
	name := sourceID(base, v.Source) + "-" + v.State + ".sqlite"
	table := filepath.Join(base, "cache", name)
	copyFile(t, filepath.Join(vectorsDir, v.File), table)
	old := time.Unix(1_000_000_000, 0)
	if err := os.Chtimes(table, old, old); err != nil {
		t.Fatal(err)
	}
	l, err := manifest.Open(filepath.Join(base, v.Source), filepath.Join(base, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if l.Path() != table {
		t.Errorf("Open chose %s, want the Python table %s", l.Path(), table)
	}
	for _, g := range v.Gets {
		row, err := l.Get(g.Repo, g.Path)
		if err != nil {
			t.Fatal(err)
		}
		checkRow(t, g.Repo+" "+g.Path, row, g.Row)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(table)
	if err != nil || !info.ModTime().Equal(old) {
		t.Errorf("the Python table was written again: %v %v", info, err)
	}
	entries, _ := os.ReadDir(filepath.Join(base, "cache"))
	if len(entries) != 1 {
		t.Errorf("cache holds %d files, want the Python table only", len(entries))
	}
}

// The table built here has the schema, rows, meta keys and file format of
// the table Python built from the same manifest.
func TestBuiltTableMatchesPython(t *testing.T) {
	v := load(t).Table
	base := t.TempDir()
	materialize(t, base, v.ManifestFiles)
	captureLogs(t)
	l, err := manifest.Open(filepath.Join(base, v.Source), filepath.Join(base, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	path, source := l.Path(), l.Source()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if want := sourceID(base, v.Source) + "-" + v.State + ".sqlite"; filepath.Base(path) != want {
		t.Errorf("table %s, want %s", filepath.Base(path), want)
	}
	if got, want := dump(t, path, source), canonical(t, v.Dump); got != want {
		t.Errorf("dump of the Go table\n got %s\nwant %s", got, want)
	}
}

// The Python module read a table the Go binary built (see go_built in
// tools/vectors/manifest.py).
func TestPythonReadsTheGoTable(t *testing.T) {
	g := load(t).GoBuilt
	switch {
	case !g.Ran:
		t.Skip("not checked when the vectors were written: " + g.Gap)
	case g.Failed != "":
		t.Fatalf("%s %s failed when the vectors were written: %s", g.Binary, g.Failed, g.Stderr)
	}
	if !g.Adopted || !g.SameAnswers || !g.SameDump {
		t.Errorf("Python on the Go table: adopted %v, same answers %v, same dump %v", g.Adopted, g.SameAnswers, g.SameDump)
	}
	if len(g.Answers) == 0 {
		t.Error("no answers recorded")
	}
	if c := g.CorruptZstd; c == nil || c.Code != 1 || !c.NamesPart || c.Tables != 1 {
		t.Errorf("scan with a corrupt zstd part: %+v, want code 1, the part named, the first table kept", c)
	}
}
