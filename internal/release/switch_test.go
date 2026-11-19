package release

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

// sqliteDB opens path as the openers of the data files open their writer.
func sqliteDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	params := url.Values{"mode": {"rwc"}, "_pragma": {"busy_timeout(10000)"}, "_txlock": {"immediate"}}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()+"?"+params.Encode())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// testSteps are n schema steps of invented tables: step k makes table t<k>
// with two rows, and changes the first row of t1.
func testSteps(n int) []migrate.Step {
	var steps []migrate.Step
	for k := 1; k <= n; k++ {
		steps = append(steps, migrate.Step{Version: k, Apply: func(tx *sql.Tx) error {
			for _, q := range []string{
				fmt.Sprintf("CREATE TABLE t%d (id INTEGER PRIMARY KEY, v TEXT NOT NULL)", k),
				fmt.Sprintf("INSERT INTO t%d (v) VALUES ('row a of step %d'), ('row b of step %d')", k, k, k),
				fmt.Sprintf("UPDATE t1 SET v = 'changed by step %d' WHERE id = 1", k),
			} {
				if _, err := tx.Exec(q); err != nil {
					return err
				}
			}
			return nil
		}})
	}
	return steps
}

// dataFile makes path at schema n with the test steps.
func dataFile(t *testing.T, path string, n int) {
	t.Helper()
	db := sqliteDB(t, path)
	defer db.Close()
	if _, err := migrate.Run(db, path, testSteps(n), migrate.Options{Kind: "test file", Create: true}); err != nil {
		t.Fatal(err)
	}
}

// dataFiles makes state.sqlite and catalog.sqlite of the rig at the
// versions given; 0 makes no file.
func (r *rig) dataFiles(state, catalog int) {
	r.t.Helper()
	if state > 0 {
		dataFile(r.t, filepath.Join(r.data, StateFile), state)
	}
	if catalog > 0 {
		dataFile(r.t, filepath.Join(r.data, CatalogFile), catalog)
	}
}

func answering(v, schema string) []byte {
	return schemaScript(`echo "pipesmith-registry `+v+`"; exit 0`, `echo '`+schema+`'; exit 0`)
}

// stage puts a release of bin into releases/<v>/ as the fetch stage does.
func (r *rig) stage(v string, bin []byte) string {
	r.t.Helper()
	target := r.l.Release(v)
	if err := os.Rename(r.release(bin), target); err != nil {
		r.t.Fatal(err)
	}
	return target
}

func TestSwitchPasses(t *testing.T) {
	for _, name := range []string{"0.2.0", "v0.2.0"} {
		r := newRig(t, "0.1.0")
		bin := versionScript("0.2.0")
		staged := r.stage("0.2.0", bin)
		if err := r.u.Switch(name, UpdateOptions{}); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, r.out.String())
		}
		if r.link(CurrentLink) != "releases/0.2.0" || r.link(PreviousLink) != "releases/0.1.0" || r.svc.restartCount() != 1 {
			t.Fatalf("current %q previous %q, %d restarts", r.link(CurrentLink), r.link(PreviousLink), r.svc.restartCount())
		}
		if got, _ := os.ReadFile(filepath.Join(staged, InstalledBinary)); string(got) != string(bin) {
			t.Fatalf("installed %q", got)
		}
		r.noLeftovers()
	}
}

func TestSwitchVerifiesAgain(t *testing.T) {
	other := newSigner(t)
	for name, c := range map[string]struct {
		change func(r *rig, dir string) error
		want   error
	}{
		SumsFile: {func(_ *rig, dir string) error {
			f, err := os.OpenFile(filepath.Join(dir, SumsFile), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.WriteString(strings.Repeat("b", 64) + "  extra\n")
			return err
		}, ErrSignature},
		BundleFile: {func(_ *rig, dir string) error {
			sums, err := os.ReadFile(filepath.Join(dir, SumsFile))
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, BundleFile), other.bundle(t, sums), 0o644)
		}, ErrSignature},
		"binary": {func(_ *rig, dir string) error {
			return os.WriteFile(filepath.Join(dir, platformBinary()), versionScript("0.2.1"), 0o755)
		}, ErrDigest},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "0.1.0")
			dir := r.stage("0.2.0", versionScript("0.2.0"))
			if err := c.change(r, dir); err != nil {
				t.Fatal(err)
			}
			r.unchanged(c.want, func() error { return r.u.Switch("0.2.0", UpdateOptions{}) })
		})
	}
	r := newRig(t, "0.1.0")
	r.unchanged(ErrVersion, func() error { return r.u.Switch("../0.1.0", UpdateOptions{}) })
	before := tree(t, r.l.Root)
	if err := r.u.Switch("0.3.0", UpdateOptions{}); err == nil {
		t.Fatal("a version that is not staged passed")
	}
	if tree(t, r.l.Root) != before || r.svc.restartCount() != 0 {
		t.Fatal("a version that is not staged changed the tree")
	}
}

// refused fails unless f returns an error wrapping want and naming each
// of words, and neither tree changed.
func (r *rig) refused(want error, f func() error, words ...string) {
	r.t.Helper()
	data := tree(r.t, r.data)
	err := r.unchanged(want, f)
	for _, w := range words {
		if !strings.Contains(err.Error(), w) {
			r.t.Fatalf("message %q lacks %q", err, w)
		}
	}
	if tree(r.t, r.data) != data {
		r.t.Fatal("the data directory changed")
	}
}

func TestSchemaPlan(t *testing.T) {
	t.Run("older", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		r.dataFiles(1, 1)
		if err := r.u.Update(r.release(answering("0.2.0", `{"catalog":2,"state":1}`)), false); err != nil {
			t.Fatalf("%v\n%s", err, r.out.String())
		}
		if !strings.Contains(r.out.String(), "schema: state.sqlite 1 stays, catalog.sqlite 1 -> 2 migrates\n") ||
			!strings.Contains(r.out.String(), "bytes free in "+r.data+" for catalog.sqlite.v1\n") {
			t.Fatalf("output:\n%s", r.out.String())
		}
	})
	t.Run("equal", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		r.dataFiles(1, 2)
		if err := r.u.Update(r.release(answering("0.2.0", `{"catalog":2,"state":1}`)), false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.out.String(), "schema: state.sqlite 1 stays, catalog.sqlite 2 stays\n") || strings.Contains(r.out.String(), "space: ") {
			t.Fatalf("output:\n%s", r.out.String())
		}
	})
	t.Run("missing", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		r.dataFiles(1, 0)
		if err := r.u.Update(r.release(answering("0.2.0", `{"catalog":2,"state":1}`)), false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.out.String(), "schema: state.sqlite 1 stays, catalog.sqlite none, the release writes 2\n") {
			t.Fatalf("output:\n%s", r.out.String())
		}
	})
	for name, c := range map[string]struct {
		state, catalog int
		file           string
		found, writes  string
	}{
		"newer state":   {2, 1, StateFile, "has schema 2", "writes schema 1"},
		"newer catalog": {1, 3, CatalogFile, "has schema 3", "writes schema 2"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "0.1.0")
			r.dataFiles(c.state, c.catalog)
			dir := r.release(answering("0.2.0", `{"catalog":2,"state":1}`))
			r.refused(ErrSchema, func() error { return r.u.Update(dir, true) }, filepath.Join(r.data, c.file), c.found, c.writes)
		})
	}
	t.Run("no schema command", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		r.dataFiles(1, 1)
		bin := schemaScript(`echo "pipesmith-registry 0.2.0"; exit 0`, `echo "error: invalid choice: 'schema'" >&2; exit 2`)
		dir := r.release(bin)
		r.refused(ErrSchema, func() error { return r.u.Update(dir, false) }, "does not know the command schema", "--allow-downgrade")
		if err := r.u.Update(dir, true); err != nil {
			t.Fatalf("--allow-downgrade: %v", err)
		}
		if !strings.Contains(r.out.String(), "schema: 0.2.0 does not know the command schema; no schema plan exists\n") {
			t.Fatalf("output:\n%s", r.out.String())
		}
	})
	for name, answer := range map[string]string{
		"exit 2 without the usage error": `echo "panic: something else" >&2; exit 2`,
		"exit 2 naming another choice":   `echo "error: pipesmith-registry: argument command: invalid choice: 'other'" >&2; exit 2`,
		"fails":                          `echo '{"catalog":2,"state":1}'; exit 1`,
		"missing key":                    `echo '{"state":1,"overview":2}'; exit 0`,
		"zero":                           `echo '{"catalog":0,"state":1}'; exit 0`,
		"two lines":                      `echo '{"catalog":2,"state":1}'; echo; exit 0`,
		"nothing":                        `exit 0`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "0.1.0")
			dir := r.release(schemaScript(`echo "pipesmith-registry 0.2.0"; exit 0`, answer))
			r.refused(ErrSchema, func() error { return r.u.Update(dir, true) })
		})
	}
	t.Run("held open", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		r.dataFiles(1, 1)
		db := sqliteDB(t, filepath.Join(r.data, StateFile))
		defer db.Close()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.Exec("INSERT INTO t1 (v) VALUES ('open')"); err != nil {
			t.Fatal(err)
		}
		if err := r.u.Update(r.release(answering("0.2.0", `{"catalog":1,"state":2}`)), false); err != nil {
			t.Fatalf("%v\n%s", err, r.out.String())
		}
		if !strings.Contains(r.out.String(), "schema: state.sqlite 1 -> 2 migrates, catalog.sqlite 1 stays\n") {
			t.Fatalf("output:\n%s", r.out.String())
		}
	})
}

func size(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(info.Size())
}

func TestSpaceRule(t *testing.T) {
	space := func(state, catalog int, free func(r *rig) uint64) *rig {
		r := newRig(t, "0.1.0")
		r.dataFiles(state, catalog)
		r.u.freeSpace = func(dir string) (uint64, error) {
			if dir != r.data {
				return 0, fmt.Errorf("free space of %s", dir)
			}
			return free(r), nil
		}
		return r
	}
	release := answering("0.2.0", `{"catalog":2,"state":2}`)
	stateSize := func(r *rig) uint64 { return size(t, filepath.Join(r.data, StateFile)) }
	catalogSize := func(r *rig) uint64 { return size(t, filepath.Join(r.data, CatalogFile)) }

	r := space(1, 0, func(r *rig) uint64 { return stateSize(r) - 1 })
	r.refused(ErrSpace, func() error { return r.u.Update(r.release(release), false) }, "state.sqlite migrates", "bytes free")
	r = space(1, 0, stateSize)
	if err := r.u.Update(r.release(release), false); err != nil {
		t.Fatalf("state with its size free: %v", err)
	}

	r = space(2, 1, func(r *rig) uint64 { return 2*catalogSize(r) - 1 })
	r.refused(ErrSpace, func() error { return r.u.Update(r.release(release), false) }, "twice its size", "--no-catalog-backup")
	if err := r.u.UpdateWith(r.release(release), UpdateOptions{NoCatalogBackup: true}); err != nil {
		t.Fatalf("--no-catalog-backup: %v", err)
	}
	if !strings.Contains(r.out.String(), "space: catalog.sqlite migrates without a copy (--no-catalog-backup)") {
		t.Fatalf("output:\n%s", r.out.String())
	}
	r = space(2, 1, func(r *rig) uint64 { return 2 * catalogSize(r) })
	if err := r.u.Update(r.release(release), false); err != nil {
		t.Fatalf("catalog with twice its size free: %v", err)
	}

	for _, c := range []struct {
		state, catalog int
		copy           string
		opts           UpdateOptions
	}{
		{1, 2, StateFile + ".v1", UpdateOptions{}},
		{2, 1, CatalogFile + ".v1", UpdateOptions{}},
		{2, 1, CatalogFile + ".v1", UpdateOptions{NoCatalogBackup: true}},
	} {
		r := space(c.state, c.catalog, func(*rig) uint64 { return 1 << 40 })
		if err := os.WriteFile(filepath.Join(r.data, c.copy), []byte("old copy"), 0o600); err != nil {
			t.Fatal(err)
		}
		r.refused(ErrSpace, func() error { return r.u.UpdateWith(r.release(release), c.opts) }, "the copy "+filepath.Join(r.data, c.copy)+" exists")
	}
	// Nothing migrates: the free space is not asked.
	r = space(2, 2, func(*rig) uint64 { t.Error("free space asked"); return 0 })
	if err := r.u.Update(r.release(release), false); err != nil {
		t.Fatal(err)
	}
}

// When both files migrate, the need is the size of state.sqlite plus twice
// the size of catalog.sqlite, because the server writes both copies.
func TestSpaceRuleSum(t *testing.T) {
	release := answering("0.2.0", `{"catalog":2,"state":2}`)
	space := func(free func(state, catalog uint64) uint64) (*rig, uint64, uint64) {
		r := newRig(t, "0.1.0")
		r.dataFiles(1, 1)
		state, catalog := size(t, filepath.Join(r.data, StateFile)), size(t, filepath.Join(r.data, CatalogFile))
		r.u.freeSpace = func(string) (uint64, error) { return free(state, catalog), nil }
		return r, state, catalog
	}
	r, state, catalog := space(func(s, c uint64) uint64 { return s + 2*c - 1 })
	if s, c := state, catalog; s+2*c-1 < s || s+2*c-1 < 2*c {
		t.Fatal("each file alone would not fit")
	}
	r.refused(ErrSpace, func() error { return r.u.Update(r.release(release), false) },
		fmt.Sprintf("%d bytes free", state+2*catalog), fmt.Sprintf("%d are free", state+2*catalog-1), "--no-catalog-backup")
	if err := r.u.UpdateWith(r.release(release), UpdateOptions{NoCatalogBackup: true}); err != nil {
		t.Fatalf("--no-catalog-backup with the space of the state copy: %v", err)
	}
	if !strings.Contains(r.out.String(), "space: catalog.sqlite migrates without a copy (--no-catalog-backup)") {
		t.Fatalf("output:\n%s", r.out.String())
	}

	r, state, _ = space(func(s, c uint64) uint64 { return s - 1 })
	r.refused(ErrSpace, func() error { return r.u.UpdateWith(r.release(release), UpdateOptions{NoCatalogBackup: true}) },
		"state.sqlite migrates", fmt.Sprintf("%d bytes free", state))

	r, _, _ = space(func(s, c uint64) uint64 { return s + 2*c })
	if err := r.u.Update(r.release(release), false); err != nil {
		t.Fatalf("with the sum free: %v", err)
	}
}

func TestSwitchWayBack(t *testing.T) {
	r := newRig(t, "0.1.0")
	r.u.HealthTimeout = 300 * time.Millisecond
	r.svc.unhealthy["0.2.0"] = true
	r.stage("0.2.0", versionScript("0.2.0"))
	before := tree(t, r.l.Root)
	err := r.u.Switch("0.2.0", UpdateOptions{})
	r.wayBack(err, ErrHealth, before, "")
	if !strings.Contains(err.Error(), "which answers again") || r.svc.restartCount() != 2 {
		t.Fatalf("message %q, %d restarts", err, r.svc.restartCount())
	}
	r.svc.unhealthy["0.2.0"] = false
	if err := r.u.Switch("0.2.0", UpdateOptions{}); err != nil {
		t.Fatalf("the second switch: %v", err)
	}
	if r.link(CurrentLink) != "releases/0.2.0" {
		t.Fatalf("current %q", r.link(CurrentLink))
	}
}
