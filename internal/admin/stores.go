package admin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// A blob store of this server is a file store (decision A6). Another kind
// answers 501 in the area of the blob stores. A soft quota has one of two
// types and a limit of at least 1 MiB.
const (
	storeKindFile = "file"
	storeArea     = "blob-stores"
	minQuotaLimit = 1048576
)

var quotaTypes = []string{"spaceRemainingQuota", "spaceUsedQuota"}

// CheckStoreKind refuses a kind of blob store other than file with 501 and
// the area blob-stores. The case of the kind does not matter: the REST path
// names it file, a stored record File.
func CheckStoreKind(kind string) error {
	if strings.EqualFold(kind, storeKindFile) {
		return nil
	}
	return &Refusal{Code: CodeStoreKind, Area: storeArea, Message: fmt.Sprintf(
		"the blob store kind %s is not available in this server, which has file stores only", kind)}
}

// CheckStoreCreate checks the create of a blob store: the kind is file, the
// path is not empty (400 at path), and a soft quota is absent or valid. A
// kind other than file is answered alone.
func CheckStoreCreate(kind, storePath string, quota *model.SoftQuota) error {
	if err := CheckStoreKind(kind); err != nil {
		return err
	}
	var list Refusals
	if strings.TrimSpace(storePath) == "" {
		list = append(list, &Refusal{Code: CodePathRequired, Field: "path", Message: "a blob store needs a path"})
	}
	return append(list, checkQuota(quota, nil)...).err()
}

// CheckStoreSave checks the save of a stored blob store: it is a file
// store, its path stays the stored one (400 at path), compared after
// path.Clean, and a soft quota is absent or valid. sent is nil when the
// body sends no path or null, and the stored path then stays; a path it
// sends, the empty one included, is compared. A value the stored store
// holds already is never refused.
func CheckStoreSave(stored model.BlobStore, sent *string, quota *model.SoftQuota) error {
	if err := CheckStoreKind(stored.Type); err != nil {
		return err
	}
	var list Refusals
	if sent != nil && path.Clean(*sent) != path.Clean(stored.Path) {
		list = append(list, &Refusal{Code: CodePathChanged, Field: "path", Message: fmt.Sprintf(
			"the path of the blob store %s is fixed at %s", stored.Name, stored.Path)})
	}
	return append(list, checkQuota(quota, stored.SoftQuota)...).err()
}

// checkQuota refuses a soft quota of another type (400 at softQuota.type)
// or with a limit below 1048576 bytes (400 at softQuota.limit), unless the
// stored quota has the same value.
func checkQuota(quota, stored *model.SoftQuota) Refusals {
	if quota == nil {
		return nil
	}
	var list Refusals
	if !slices.Contains(quotaTypes, quota.Type) && (stored == nil || quota.Type != stored.Type) {
		list = append(list, &Refusal{Code: CodeQuotaType, Field: "softQuota.type", Message: fmt.Sprintf(
			"the soft quota type %q is not known; use spaceRemainingQuota or spaceUsedQuota", quota.Type)})
	}
	if quota.Limit < minQuotaLimit && (stored == nil || quota.Limit != stored.Limit) {
		list = append(list, &Refusal{Code: CodeQuotaLimit, Field: "softQuota.limit", Message: fmt.Sprintf(
			"the soft quota limit of %d bytes is below 1048576 bytes", quota.Limit)})
	}
	return list
}

// StoresDir is the folder of the created stores in a data directory (ST1).
const StoresDir = "stores"

// resolveLoose resolves the symbolic links of the longest part of p that
// exists and appends the rest, so that a path that does not exist yet is
// read where the file system would put it.
func resolveLoose(p string) (string, error) {
	p = filepath.Clean(p)
	var rest []string
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(append([]string{real}, rest...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
}

// within reports whether p lies strictly below dir; both are resolved.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(rel)
}

// StorePath gives the path of a created store (ruling ST1), resolved with
// its symbolic links. sent is the path of the body, nil when it sends none,
// which means <data>/stores/<name>; a relative path is read below
// <data>/stores/. The resolved path lies strictly below the resolved
// stores folder, is no file and no folder that holds anything, and is not
// equal to, inside or around a path of taken, the paths of the other
// stores, compared without regard to letter case (ST14); else 400 at path. The stores folder is made when it is missing.
func StorePath(dataDir, name string, sent *string, taken []string) (string, error) {
	fail := func(problem string) error {
		return &Refusal{Code: CodePathInvalid, Field: "path", Message: "the path of the blob store " + name + " " + problem}
	}
	base := filepath.Join(dataDir, StoresDir)
	if err := os.MkdirAll(base, 0o750); err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	p := filepath.Join(base, name)
	if sent != nil {
		p = *sent
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
	}
	resolved, err := resolveLoose(p)
	if err != nil {
		return "", fail("cannot be resolved")
	}
	if !within(resolved, root) {
		return "", fail("must lie below the folder " + StoresDir + " of the data directory, its links resolved")
	}
	info, err := os.Lstat(resolved)
	switch {
	case err == nil && !info.IsDir():
		return "", fail("names a file")
	case err == nil:
		entries, err := os.ReadDir(resolved)
		if err != nil {
			return "", err
		}
		if len(entries) > 0 {
			return "", fail("holds files already; name an empty or a new folder")
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	for _, t := range taken {
		other, err := resolveLoose(t)
		if err != nil {
			continue
		}
		// without regard to case (ST14): two paths that differ in case only
		// may name one folder
		lowOther, lowResolved := strings.ToLower(other), strings.ToLower(resolved)
		if lowOther == lowResolved || within(lowResolved, lowOther) || within(lowOther, lowResolved) {
			return "", fail("is the path of another store, or lies inside or around it")
		}
	}
	return resolved, nil
}

// CheckStoreProtected refuses, with 409, the delete of local-write, the
// store this server writes into, and of a store that config.json of the
// host attaches (ST2): this server does not own those.
func CheckStoreProtected(name string, attached bool) error {
	switch {
	case name == WriteStore:
		return &Refusal{Code: CodeStoreProtected, Message: fmt.Sprintf(
			"the blob store %s is the store this server writes into; it is never deleted", name)}
	case attached:
		return &Refusal{Code: CodeStoreProtected, Message: fmt.Sprintf(
			"the blob store %s is attached by config.json of the host; remove it there", name)}
	}
	return nil
}
