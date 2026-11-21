// Package scan replaces the rows of one store in the index with what the
// store holds now, and then the rows of that store in catalog.sqlite beside
// the index.
package scan

import (
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// ProgressEvery is the number of live blobs between two progress calls.
const ProgressEvery = 100

// HintOrder lists the formats a repository can be hinted as; it gets the
// first of them that any of its paths gives.
var HintOrder = [...]string{"docker", "npm", "maven2"}

// Result is the outcome of one scan.
type Result struct {
	Store                                      string
	Live, Deleted, Copies, NoBytes, Unparsable int
	DeletedBytes                               int64 // the bytes of the Deleted blobs of a blob store (SR14)
	Seconds                                    float64
	// Hints maps a repository to its format. Python keeps them in a dict in
	// sorted order, the order pyjson writes a map in.
	Hints map[string]string
}

// GuessFormat is the format a repository path points at: "docker", "npm",
// "maven2" or "" for none. v2/ at the top gives docker,
// <name>/-/<file>.tgz or @<scope>/<name>/-/<file>.tgz gives npm, a .pom or
// a maven-metadata.xml gives maven2.
func GuessFormat(path string) string {
	if strings.HasPrefix(path, "v2/") {
		return "docker"
	}
	segments := strings.Split(path, "/")
	name := segments[len(segments)-1]
	if strings.HasSuffix(name, ".tgz") && len(segments) >= 3 && segments[len(segments)-2] == "-" {
		pkg := segments[:len(segments)-2]
		if len(pkg) == 1 && pkg[0] != "" && !strings.HasPrefix(pkg[0], "@") {
			return "npm"
		}
		// len counts bytes here and code points in Python; after the one
		// byte "@" both are above 1 for the same names.
		if len(pkg) == 2 && len(pkg[0]) > 1 && strings.HasPrefix(pkg[0], "@") && pkg[1] != "" {
			return "npm"
		}
	}
	if strings.HasSuffix(name, ".pom") || name == "maven-metadata.xml" {
		return "maven2"
	}
	return ""
}

var errStopped = errors.New("scan: walk stopped")

// Run replaces the rows of the store with what the store holds now.
//
// progress, when not nil, receives the number of live blobs read so far,
// every ProgressEvery blobs and once at the end. When the walk fails, the
// index keeps the rows of the previous scan. A store that looks unmounted
// and has rows fails with an error that matches fs.ErrNotExist and keeps
// them too.
func Run(s stores.Store, ix *index.Index, progress func(live int)) (Result, error) {
	return RunWith(s, ix, progress, nil)
}

// RunWith is Run with an arbiter of the rows of the store (index.Arbiter):
// the scan of a store whose write order the caller knows. A nil arbiter is
// Run.
func RunWith(s stores.Store, ix *index.Index, progress func(live int), arbiter index.Arbiter) (Result, error) {
	return RunIn(nil, s, ix, progress, arbiter)
}

// RunIn is RunWith with catalog.sqlite as the caller holds it open: a
// serving process opens the file once and its writes keep it (leg F1b).
// The scan takes the writer of the catalog before the index commits, so a
// write that follows the index waits for the catalog of the scan. A nil
// file is RunWith, which opens the file beside the index for the scan.
func RunIn(file *catalog.Catalog, s stores.Store, ix *index.Index, progress func(live int), arbiter index.Arbiter) (Result, error) {
	var counts stores.ScanCounts
	started := time.Now()
	reported := -1
	ranks := map[string]int{}

	// The walk runs as a pull iterator, as the generator of Python does, and
	// counts holds what the walk had counted when it handed over the current
	// entry. The first entry is read inside the index transaction: a write
	// that lands after the first read of the walk then waits for the scan
	// instead of losing its row to it.
	var walkErr error
	var markedBytes int64
	walker, counted := s.(stores.MarkedWalker)
	next, stop := iter.Pull(func(yield func(stores.Entry) bool) {
		give := func(e stores.Entry) error {
			if !yield(e) {
				return errStopped
			}
			return nil
		}
		if counted {
			// a blob store gives its marked blobs as it walks: their count and
			// bytes go into store_marks of catalog.sqlite (ruling SR14)
			walkErr = walker.Walk(&counts, give, func(m stores.Marked) error {
				markedBytes += m.Size
				return nil
			})
			return
		}
		walkErr = s.Entries(&counts, give)
	})
	defer stop()

	// catalog.sqlite beside the index takes the same entries once the index
	// holds them; it never fails the scan.
	var fill *catalog.Scan
	if arbiter != nil {
		arbiter = &chooser{Arbiter: arbiter, fill: &fill}
	}
	_, err := ix.ReplaceStoreWith(s.Name(), func(yield func(stores.Entry) error) error {
		entry, ok := next()
		if !ok {
			if walkErr != nil {
				return walkErr
			}
			if err := keepUnmounted(s, ix); err != nil {
				return err
			}
		}
		fill = catalog.StartScanIn(file, ix, s.Name())
		for ok {
			rank, seen := ranks[entry.Repo]
			if !seen || rank != 0 {
				if guess := GuessFormat(entry.Path); guess != "" {
					found := slices.Index(HintOrder[:], guess)
					if !seen || found < rank {
						ranks[entry.Repo] = found
					}
				}
			}
			if err := yield(entry); err != nil {
				return err
			}
			fill.Add(entry)
			if progress != nil && counts.Live%ProgressEvery == 0 {
				reported = counts.Live
				progress(reported)
			}
			entry, ok = next()
		}
		if walkErr == nil {
			if counted {
				fill.SetMarks(catalog.Marks{Blobs: int64(counts.Deleted), Bytes: markedBytes, Counted: time.Now()})
			}
			fill.Hold()
		}
		return walkErr
	}, arbiter)
	if err != nil {
		if fill != nil {
			fill.Finish(ix, false)
		}
		return Result{}, err
	}
	hints := map[string]string{}
	for repo, rank := range ranks {
		hints[repo] = HintOrder[rank]
	}
	err = ix.SetHints(s.Name(), hints)
	fill.Finish(ix, err == nil)
	if err != nil {
		return Result{}, err
	}
	if progress != nil && reported != counts.Live {
		progress(counts.Live)
	}
	return Result{
		Store:        s.Name(),
		Live:         counts.Live,
		Deleted:      counts.Deleted,
		DeletedBytes: markedBytes,
		Copies:       counts.Copies,
		NoBytes:      counts.NoBytes,
		Unparsable:   counts.Unparsable,
		Seconds:      time.Since(started).Seconds(),
		Hints:        hints,
	}, nil
}

// chooser hands every decision of the arbiter of a store to the catalog
// side of the scan, so that catalog.sqlite takes the blob the index takes
// for each path: the one the arbiter keeps of two blobs of a path, and none
// for a row it drops. fill points at the catalog side, which the scan starts
// inside the index transaction once it has read the first entry.
type chooser struct {
	index.Arbiter
	fill **catalog.Scan
}

func (c *chooser) choose(repo, path, location string) {
	if *c.fill != nil {
		(*c.fill).Choose(repo, path, location)
	}
}

func (c *chooser) Newer(held, next stores.Entry) bool {
	newer := c.Arbiter.Newer(held, next)
	kept := held
	if newer {
		kept = next
	}
	c.choose(kept.Repo, kept.Path, kept.Location)
	return newer
}

func (c *chooser) Settle(rows index.Rows) error {
	return c.Arbiter.Settle(chosenRows{Rows: rows, chooser: c})
}

// chosenRows are the rows of a store that tell the catalog of every row the
// arbiter drops.
type chosenRows struct {
	index.Rows
	chooser *chooser
}

func (r chosenRows) Drop(repo, path string) error {
	if err := r.Rows.Drop(repo, path); err != nil {
		return err
	}
	r.chooser.choose(repo, path, "")
	return nil
}

// keepUnmounted fails a walk without entries of a store that looks unmounted
// while the index has rows of it, so that the rows stay. The rows are read
// on a reader connection, which sees them as the last commit left them.
func keepUnmounted(s stores.Store, ix *index.Index) error {
	if !stores.LooksUnmounted(s) {
		return nil
	}
	stats, err := ix.StoreStats()
	if err != nil {
		return err
	}
	var held int64
	for _, stat := range stats {
		if stat.Store == s.Name() {
			held = stat.Files
			break
		}
	}
	if held == 0 {
		return nil
	}
	return &stores.NotFoundError{Message: fmt.Sprintf(
		"store %s at %s holds no files, but the index has %d rows of it; the rows are kept."+
			" Check that the disk is mounted, or detach the store with: store remove %s",
		s.Name(), s.Root(), held, s.Name())}
}
