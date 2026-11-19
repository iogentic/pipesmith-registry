package repos

import (
	"encoding/json"
	"math"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// DefaultWriteStore is the store this server writes new blobs into (W1 of
// the phase 4 plan): stores/local-write in the data directory.
const DefaultWriteStore = "local-write"

// The limits of a write (W14 of the phase 4 plan).
const (
	DefaultMaxBodyBytes   int64 = 10 << 30 // the largest body a write takes
	DefaultFreeSpaceFloor int64 = 2 << 30  // the free space a write leaves on its store
)

// WriteSettings are the attributes of a hosted repository that decide a
// write: the policies, the content check, the target store and the limits.
// Policies keep the spelling of the old product; internal/policy reads them
// without regard to case. An attribute of the state wins over the field of
// repositories.json.
type WriteSettings struct {
	// WritePolicy is storage.writePolicy, else write_policy: ALLOW,
	// ALLOW_ONCE or DENY. Empty when neither is known, which refuses every
	// write.
	WritePolicy string
	// VersionPolicy is maven.versionPolicy, else version_policy: RELEASE,
	// SNAPSHOT or MIXED; empty checks nothing.
	VersionPolicy string
	// LayoutPolicy is maven.layoutPolicy, else layout_policy: STRICT or
	// PERMISSIVE; empty checks nothing.
	LayoutPolicy string
	// StrictContentType is storage.strictContentTypeValidation.
	StrictContentType bool
	// LatestRedeploy is storage.latestPolicy of a docker repository: the tag
	// latest may be pushed again under ALLOW_ONCE.
	LatestRedeploy bool
	// BlobStoreName is storage.blobStoreName: the store of the old product,
	// kept for the record. New blobs never go there.
	BlobStoreName string
	// WriteStore is write_store, an attribute of this server: the writable
	// store new blobs go into. DefaultWriteStore when it is not set.
	WriteStore string
	// MaxBodyBytes is max_body_bytes, an attribute of this server.
	MaxBodyBytes int64
	// FreeSpaceFloor is free_space_floor_bytes, an attribute of this server.
	FreeSpaceFloor int64
}

func sectionOf(attrs map[string]any, key string) map[string]any {
	m, _ := attrs[key].(map[string]any)
	return m
}

func stringOf(m map[string]any, key string) (string, bool) {
	s, ok := m[key].(string)
	return s, ok && s != ""
}

// sizeOf is a count of bytes that is not negative, in the forms a JSON
// decoder gives a number.
func sizeOf(m map[string]any, key string) (int64, bool) {
	var n int64
	switch v := m[key].(type) {
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return 0, false
		}
		n = parsed
	case float64:
		if v != math.Trunc(v) || v < 0 || v > math.MaxInt64/2 {
			return 0, false
		}
		n = int64(v)
	case int64:
		n = v
	case int:
		n = int64(v)
	default:
		return 0, false
	}
	return n, n >= 0
}

// TakesWrites reports a repository that a write may target: hosted by its
// definition, and hosted and online by its stored record when it has one
// (finding N3 of review 4). A name the index discovers without a definition
// counts as hosted, so a proxy or a group that repositories.json lacks is
// told by its record.
func (r Repo) TakesWrites() bool {
	return r.Type == "hosted" && (r.Record == nil || r.Record.Type == "hosted" && r.Record.Online)
}

// WriteSettings reads the write settings of the repository.
func (r Repo) WriteSettings() WriteSettings {
	w := WriteSettings{WriteStore: DefaultWriteStore, MaxBodyBytes: DefaultMaxBodyBytes, FreeSpaceFloor: DefaultFreeSpaceFloor}
	for _, field := range []struct {
		value  *string
		target *string
	}{{r.WritePolicy, &w.WritePolicy}, {r.VersionPolicy, &w.VersionPolicy}, {r.LayoutPolicy, &w.LayoutPolicy}} {
		if field.value != nil {
			*field.target = *field.value
		}
	}
	storage, maven := sectionOf(r.Attributes, "storage"), sectionOf(r.Attributes, "maven")
	for _, attribute := range []struct {
		section map[string]any
		key     string
		target  *string
	}{
		{storage, "writePolicy", &w.WritePolicy}, {maven, "versionPolicy", &w.VersionPolicy},
		{maven, "layoutPolicy", &w.LayoutPolicy}, {storage, "blobStoreName", &w.BlobStoreName},
		{r.Attributes, "write_store", &w.WriteStore},
	} {
		if s, ok := stringOf(attribute.section, attribute.key); ok {
			*attribute.target = s
		}
	}
	w.StrictContentType, _ = storage["strictContentTypeValidation"].(bool)
	w.LatestRedeploy, _ = storage["latestPolicy"].(bool)
	if n, ok := sizeOf(r.Attributes, "max_body_bytes"); ok {
		w.MaxBodyBytes = n
	}
	if n, ok := sizeOf(r.Attributes, "free_space_floor_bytes"); ok {
		w.FreeSpaceFloor = n
	}
	return w
}

// WithAttributes returns the repositories, each with the attributes, the
// type and the state of the stored record of its name (seam Repository
// settings of the phase 4 execution plan). A record without a repository of
// its name adds none; the repositories it is called on stay as they are.
func (r *Repositories) WithAttributes(records []model.Repository) *Repositories {
	byName := map[string]model.Repository{}
	for _, rec := range records {
		if _, seen := byName[rec.Name]; !seen {
			byName[rec.Name] = rec
		}
	}
	out := &Repositories{byName: make(map[string]Repo, len(r.byName)), sorted: make([]Repo, 0, len(r.sorted))}
	for _, repo := range r.sorted {
		if rec, ok := byName[repo.Name]; ok {
			repo.Attributes = rec.Attributes
			repo.Record = &Record{Type: rec.Type, Online: rec.Online}
		}
		out.byName[repo.Name] = repo
		out.sorted = append(out.sorted, repo)
	}
	return out
}
