package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

// Refusals of the schema plan and of the space for the copies.
var (
	ErrSchema   = errors.New("schema")
	ErrNoSchema = errors.New("the release does not know the command schema")
	ErrSpace    = errors.New("data directory")
)

// The files of the data directory that the schema plan reads, as
// internal/state and internal/catalog name them.
const (
	StateFile   = "state.sqlite"
	CatalogFile = "catalog.sqlite"
)

// Schema is the answer of `<binary> schema`: the schema versions the binary
// writes.
type Schema struct {
	Catalog int
	State   int
}

var schemaNumber = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)

// unknownSchema is the usage error of the argument parser for the command
// schema, as a release before the command writes it.
var unknownSchema = regexp.MustCompile(`invalid choice: ['"]schema['"]`)

// RunSchema runs `<bin> schema` as RunVersion runs `version`. A binary that
// answers with the usage error of the argument parser (exit code 2 and a
// standard error that names schema as an invalid choice) gives ErrNoSchema;
// any other exit code 2 is a failure.
func RunSchema(bin string, timeout time.Duration) (Schema, error) {
	u := &Updater{VersionTimeout: timeout}
	return u.runSchema(bin)
}

func (u *Updater) runSchema(bin string) (Schema, error) {
	for attempt := 0; ; attempt++ {
		answer, errOut, err := u.runOnce(bin, "schema")
		if err != nil && textBusy(err) && attempt < 5 {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 2 && unknownSchema.MatchString(errOut) {
			return Schema{}, ErrNoSchema
		}
		if err != nil {
			return Schema{}, fmt.Errorf("%w: %s schema failed: %v", ErrSchema, InstalledBinary, err)
		}
		return parseSchema(answer)
	}
}

// parseSchema reads the answer of `schema`: one JSON object on one line in
// which catalog and state are whole numbers from 1 up. Further keys are
// ignored, so a later release may name more files; a key given twice is
// refused.
func parseSchema(answer string) (Schema, error) {
	refuse := func(why string) (Schema, error) {
		return Schema{}, fmt.Errorf("%w: %s schema answered %q (%s), want one line with a JSON object {\"catalog\":N,\"state\":N}",
			ErrSchema, InstalledBinary, answer, why)
	}
	line, ok := strings.CutSuffix(answer, "\n")
	if !ok || strings.ContainsAny(line, "\r\n") || !strings.HasPrefix(line, "{") {
		return refuse("not one line with an object")
	}
	dec := json.NewDecoder(strings.NewReader(line))
	if _, err := dec.Token(); err != nil {
		return refuse(err.Error())
	}
	seen := map[string]bool{}
	numbers := map[string]int{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return refuse(err.Error())
		}
		key, _ := tok.(string)
		if seen[key] {
			return refuse("the key " + key + " is given twice")
		}
		seen[key] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return refuse(err.Error())
		}
		if key == "catalog" || key == "state" {
			if !schemaNumber.Match(value) {
				return refuse(key + " is no whole number from 1 up")
			}
			numbers[key], _ = strconv.Atoi(string(value))
		}
	}
	if _, err := dec.Token(); err != nil {
		return refuse(err.Error())
	}
	if dec.InputOffset() != int64(len(line)) {
		return refuse("text after the object")
	}
	if !seen["catalog"] || !seen["state"] {
		return refuse("a key is missing")
	}
	return Schema{Catalog: numbers["catalog"], State: numbers["state"]}, nil
}

// planFile is a file of the data directory that the release migrates.
type planFile struct {
	name string // StateFile or CatalogFile
	path string
	from int   // the version of the file before the switch
	to   int   // the version the release writes
	size int64 // bytes of the file before the switch
	copy bool  // the runner writes path.v<from> before its first step
}

func (f planFile) copyPath() string { return f.path + ".v" + strconv.Itoa(f.from) }

// schemaPlan lists the files that the release migrates.
type schemaPlan struct {
	dataDir string
	files   []planFile
}

// planSchema runs `<bin> schema`, reads the versions of the files of the
// data directory and refuses a file newer than the release writes.
func (u *Updater) planSchema(s *session, bin string, v Version, opts UpdateOptions) (*schemaPlan, error) {
	dataDir := s.dataDir
	schema, err := u.runSchema(bin)
	if errors.Is(err, ErrNoSchema) {
		if !opts.AllowDowngrade {
			return nil, fmt.Errorf("%w: %s %v; it may write an older schema than the data directory holds, and --allow-downgrade switches anyway", ErrSchema, v, ErrNoSchema)
		}
		u.step("schema: %s does not know the command schema; no schema plan exists", v)
		return &schemaPlan{dataDir: dataDir}, nil
	}
	if err != nil {
		return nil, err
	}
	p := &schemaPlan{dataDir: dataDir}
	var words []string
	for _, f := range []struct {
		name   string
		writes int
	}{{StateFile, schema.State}, {CatalogFile, schema.Catalog}} {
		path := filepath.Join(dataDir, f.name)
		found, err := migrate.FileVersion(path)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSchema, err)
		}
		switch {
		case found == 0:
			words = append(words, fmt.Sprintf("%s none, the release writes %d", f.name, f.writes))
		case found > f.writes:
			return nil, fmt.Errorf("%w: %s has schema %d and %s writes schema %d; a release that old cannot open it",
				ErrSchema, path, found, v, f.writes)
		case found == f.writes:
			words = append(words, fmt.Sprintf("%s %d stays", f.name, found))
		default:
			info, err := os.Stat(path)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrSchema, err)
			}
			p.files = append(p.files, planFile{name: f.name, path: path, from: found, to: f.writes, size: info.Size(), copy: true})
			words = append(words, fmt.Sprintf("%s %d -> %d migrates", f.name, found, f.writes))
		}
	}
	u.step("schema: %s", strings.Join(words, ", "))
	return p, nil
}

// checkSpace refuses an existing copy or .failed file of a file that
// migrates, and a free space below the sum of what the copies need: the size
// of state.sqlite and twice the size of catalog.sqlite. With noCatalogBackup
// and too little space for that sum, catalog.sqlite migrates without a copy
// when the copy of state.sqlite fits alone.
func (u *Updater) checkSpace(p *schemaPlan, noCatalogBackup bool) error {
	if len(p.files) == 0 {
		return nil
	}
	freeSpace := migrate.FreeSpace
	if u.freeSpace != nil {
		freeSpace = u.freeSpace
	}
	free, err := freeSpace(p.dataDir)
	if err != nil {
		return fmt.Errorf("%w: free space of %s: %v", ErrSpace, p.dataDir, err)
	}
	var state, catalog *planFile
	var stateNeed, catalogNeed uint64
	for i := range p.files {
		f := &p.files[i]
		if _, err := os.Lstat(f.copyPath()); err == nil || !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: the copy %s exists, from an earlier migration; move it away, then start again", ErrSpace, f.copyPath())
		}
		if _, err := os.Lstat(f.path + ".failed"); err == nil || !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s.failed exists, from an earlier failed update; move it away, then start again", ErrSpace, f.path)
		}
		size := uint64(f.size) // #nosec G115 -- the size of a regular file is not negative.
		if f.name == CatalogFile {
			catalog, catalogNeed = f, 2*size
		} else {
			state, stateNeed = f, size
		}
	}
	switch total := stateNeed + catalogNeed; {
	case free >= total:
	case catalog != nil && noCatalogBackup && free >= stateNeed:
		catalog.copy = false
		u.step("space: %s migrates without a copy (--no-catalog-backup): %d bytes free, a copy needs %d", catalog.name, free, catalogNeed)
	case catalog == nil || noCatalogBackup:
		return fmt.Errorf("%w: %s migrates and its copy needs %d bytes free in %s; %d are free", ErrSpace, state.name, stateNeed, p.dataDir, free)
	case state == nil:
		return fmt.Errorf("%w: %s migrates and its copy needs %d bytes free in %s, twice its size; %d are free; --no-catalog-backup migrates without a copy",
			ErrSpace, catalog.name, catalogNeed, p.dataDir, free)
	default:
		return fmt.Errorf("%w: %s and %s migrate and their copies need %d bytes free in %s, %d for %s and %d, twice its size, for %s; %d are free; --no-catalog-backup migrates %s without a copy",
			ErrSpace, state.name, catalog.name, total, p.dataDir, stateNeed, state.name, catalogNeed, catalog.name, free, catalog.name)
	}
	var copies []string
	for _, f := range p.files {
		if f.copy {
			copies = append(copies, filepath.Base(f.copyPath()))
		}
	}
	if len(copies) > 0 {
		u.step("space: %d bytes free in %s for %s", free, p.dataDir, strings.Join(copies, ", "))
	}
	return nil
}

// rollbackSchema runs `<previous binary> schema` and refuses when a data
// file is newer than that release writes. A release that does not know the
// command is not compared.
func (u *Updater) rollbackSchema(s *session, prev string) error {
	schema, err := u.runSchema(filepath.Join(s.l.Release(prev), InstalledBinary))
	if errors.Is(err, ErrNoSchema) {
		u.step("schema: %s/%s does not know the command schema; the data files are not compared", ReleasesDir, prev)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w; rollback changed nothing", err)
	}
	var words []string
	for _, f := range []struct {
		name   string
		writes int
	}{{StateFile, schema.State}, {CatalogFile, schema.Catalog}} {
		path := filepath.Join(s.dataDir, f.name)
		found, err := migrate.FileVersion(path)
		if err != nil {
			return fmt.Errorf("%w: %v; rollback changed nothing", ErrSchema, err)
		}
		switch {
		case found == 0:
			words = append(words, f.name+" none")
			continue
		case found <= f.writes:
			words = append(words, fmt.Sprintf("%s %d, the release writes %d", f.name, found, f.writes))
			continue
		}
		copyText := fmt.Sprintf("no copy %s.v%d exists", path, f.writes)
		if _, err := os.Lstat(fmt.Sprintf("%s.v%d", path, f.writes)); err == nil {
			copyText = fmt.Sprintf("the copy %s.v%d holds the file before the update", path, f.writes)
		}
		return fmt.Errorf("%w: %s has schema %d, written by %s/%s, and %s/%s writes schema %d; rollback changed nothing and restarted nothing; %s, and putting it back by hand loses every change made since the update",
			ErrSchema, path, found, ReleasesDir, s.current, ReleasesDir, prev, f.writes, copyText)
	}
	u.step("schema: %s", strings.Join(words, ", "))
	return nil
}
