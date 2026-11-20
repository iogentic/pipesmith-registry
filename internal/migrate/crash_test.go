package migrate_test

import (
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

// Review 3-15: a crash between the copy and the first step leaves the copy
// <file>.v<old> beside a file that is still of the old schema. The next run
// finds that the copy holds the file as it is, keeps it and takes the steps.
// A copy that holds anything else is still refused and left as it is.
func TestRunGoesOnAfterACrashBetweenCopyAndStep(t *testing.T) {
	path, db := at(t, 1)
	if _, err := db.Exec("VACUUM INTO ?", path+".v1"); err != nil {
		t.Fatal(err)
	}
	var l ledger
	got, err := migrate.Run(db, path, l.steps(2), migrate.Options{Kind: kind, Backup: true})
	if err != nil {
		t.Fatalf("the run after the crash: %v", err)
	}
	if want := (migrate.Result{From: 1, To: 2, Copy: path + ".v1"}); got != want {
		t.Fatalf("result %+v, want %+v", got, want)
	}
	l.ran(t, 0, 1)
	old := open(t, path+".v1")
	if v, names := schemaRow(t, old), tables(t, old); v != "1" || names != "meta t1" {
		t.Fatalf("copy: schema row %s, tables %q", v, names)
	}
}

func TestRunRefusesACopyOfOtherContent(t *testing.T) {
	path, db := at(t, 1)
	if _, err := db.Exec("VACUUM INTO ?", path+".v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE t1 SET v = 'written after the copy'"); err != nil {
		t.Fatal(err)
	}
	var l ledger
	_, err := migrate.Run(db, path, l.steps(2), migrate.Options{Kind: kind, Backup: true})
	if err == nil || !strings.Contains(err.Error(), path+".v1") {
		t.Fatalf("error %v, want a refusal that names the copy", err)
	}
	l.ran(t, 0, 0)
	if v := schemaRow(t, db); v != "1" {
		t.Fatalf("schema row %s", v)
	}
	old := open(t, path+".v1")
	if row := query(t, old, "SELECT v FROM t1"); row != "one" {
		t.Fatalf("the earlier copy now holds %q", row)
	}
}
