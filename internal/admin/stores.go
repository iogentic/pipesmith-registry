package admin

import (
	"fmt"
	"path"
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
