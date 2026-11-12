// Package scan replaces the rows of one store in the index with what the
// store holds now.
package scan

import (
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"

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
	var counts stores.ScanCounts
	started := time.Now()
	reported := -1
	ranks := map[string]int{}

	// The walk runs as a pull iterator, as the generator of Python does: the
	// first entry is read before the index is touched, and counts holds what
	// the walk had counted when it handed over the current entry.
	var walkErr error
	next, stop := iter.Pull(func(yield func(stores.Entry) bool) {
		walkErr = s.Entries(&counts, func(e stores.Entry) error {
			if !yield(e) {
				return errStopped
			}
			return nil
		})
	})
	defer stop()

	entry, ok := next()
	if !ok {
		if walkErr != nil {
			return Result{}, walkErr
		}
		if stores.LooksUnmounted(s) {
			stats, err := ix.StoreStats()
			if err != nil {
				return Result{}, err
			}
			var held int64
			for _, stat := range stats {
				if stat.Store == s.Name() {
					held = stat.Files
					break
				}
			}
			if held != 0 {
				return Result{}, &stores.NotFoundError{Message: fmt.Sprintf(
					"store %s at %s holds no files, but the index has %d rows of it; the rows are kept."+
						" Check that the disk is mounted, or detach the store with: store remove %s",
					s.Name(), s.Root(), held, s.Name())}
			}
		}
	}

	_, err := ix.ReplaceStore(s.Name(), func(yield func(stores.Entry) error) error {
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
			if progress != nil && counts.Live%ProgressEvery == 0 {
				reported = counts.Live
				progress(reported)
			}
			entry, ok = next()
		}
		return walkErr
	})
	if err != nil {
		return Result{}, err
	}
	hints := map[string]string{}
	for repo, rank := range ranks {
		hints[repo] = HintOrder[rank]
	}
	if err := ix.SetHints(s.Name(), hints); err != nil {
		return Result{}, err
	}
	if progress != nil && reported != counts.Live {
		progress(counts.Live)
	}
	return Result{
		Store:      s.Name(),
		Live:       counts.Live,
		Deleted:    counts.Deleted,
		Copies:     counts.Copies,
		NoBytes:    counts.NoBytes,
		Unparsable: counts.Unparsable,
		Seconds:    time.Since(started).Seconds(),
		Hints:      hints,
	}, nil
}
