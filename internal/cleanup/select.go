// Package cleanup selects the assets that a cleanup policy removes
// (decisions A9 and A19 of the phase 6 plan). It reads the assets it is
// given and removes nothing. A preview and a run call the same selection, so
// a preview lists exactly what a run removes.
package cleanup

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/admin"
)

// PreviewLimit is the most items of a preview.
const PreviewLimit = 100

// secondsPerDay are the seconds of a day of the criteria, 24 hours.
const secondsPerDay = 24 * 60 * 60

// Asset is one asset as the selection reads it from the catalog. Its times
// are UTC, and a zero time is none.
type Asset struct {
	Repository string // assets.repo
	Path       string // assets.path
	Group      string // components.namespace, empty for none
	Name       string // components.name
	Version    string // components.version, empty for none
	// BlobCreated is the time its blob was last updated: the created_ms of
	// the row of blobs that holds the path now, as a blob is written whole.
	BlobCreated time.Time
	// LastDownloaded is the last download time this server recorded,
	// assets.last_downloaded_ms.
	LastDownloaded time.Time
	// ImportedLastDownloaded is the last download time an import brought,
	// imported_assets.last_downloaded_ms.
	ImportedLastDownloaded time.Time
}

// Input is what a selection reads besides the criteria: the assets, the
// build time of the catalog file and the current time.
type Input struct {
	Assets       []Asset
	CatalogBuilt time.Time
	Now          time.Time
}

// Selection is what Select gives. Runnable is false for criteria that
// admin.Runnable refuses; Unknown then names their criteria of another
// name, sorted, and Assets is empty.
type Selection struct {
	Runnable bool
	Unknown  []string
	Assets   []Asset
}

// Item is one asset of a preview, in the JSON names of the contract.
type Item struct {
	Repository string `json:"repository"`
	Group      string `json:"group"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Path       string `json:"path"`
}

// Preview is the answer of a cleanup preview (contract S15): the first
// PreviewLimit selected assets, the count of all of them, and whether there
// are more than the items.
type Preview struct {
	Items     []Item `json:"items"`
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
}

// LastDownload is the time of the last download that the usage criterion
// reads (decision A19): the latest of the time this server recorded, the
// time an import brought and the build time of the catalog file, in UTC.
func LastDownload(a Asset, catalogBuilt time.Time) time.Time {
	latest := catalogBuilt
	for _, t := range []time.Time{a.LastDownloaded, a.ImportedLastDownloaded} {
		if t.After(latest) {
			latest = t
		}
	}
	return latest.UTC()
}

// Select gives the assets of in that the criteria of a cleanup policy
// select, sorted by path in byte order and then by repository. An asset is
// selected when every criterion that is set holds; an absent or null
// criterion is not applied. lastBlobUpdatedDays N holds when the blob was
// last updated more than N days before the current time, lastDownloadedDays
// N when LastDownload lies more than N days before it. A day is 24 hours,
// and a time exactly N days back does not hold. For criteria that
// admin.Runnable refuses, Select selects nothing and names the unknown
// criteria; it never falls back to the known ones. The selected assets are
// copies whose times are UTC; the assets of in are not changed.
func Select(criteria map[string]any, in Input) Selection {
	runs, unknown := admin.Runnable(criteria)
	if !runs {
		return Selection{Unknown: unknown}
	}
	age, byAge := admin.CriterionDays(criteria[admin.CriterionLastBlobUpdatedDays])
	usage, byUsage := admin.CriterionDays(criteria[admin.CriterionLastDownloadedDays])
	var selected []Asset
	for _, a := range in.Assets {
		if byAge && !olderThan(a.BlobCreated, in.Now, age) {
			continue
		}
		if byUsage && !olderThan(LastDownload(a, in.CatalogBuilt), in.Now, usage) {
			continue
		}
		a.BlobCreated = a.BlobCreated.UTC()
		a.LastDownloaded = a.LastDownloaded.UTC()
		a.ImportedLastDownloaded = a.ImportedLastDownloaded.UTC()
		selected = append(selected, a)
	}
	slices.SortStableFunc(selected, func(a, b Asset) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Repository, b.Repository))
	})
	return Selection{Runnable: true, Unknown: unknown, Assets: selected}
}

// PreviewOf is the preview of the criteria over in: the assets Select
// gives, in its order, the first PreviewLimit of them as items.
func PreviewOf(criteria map[string]any, in Input) Preview {
	selected := Select(criteria, in).Assets
	shown := selected[:min(len(selected), PreviewLimit)]
	items := make([]Item, len(shown))
	for i, a := range shown {
		items[i] = Item{Repository: a.Repository, Group: a.Group, Name: a.Name, Version: a.Version, Path: a.Path}
	}
	return Preview{Items: items, Total: len(selected), Truncated: len(selected) > PreviewLimit}
}

// olderThan reports whether t lies more than days days of 24 hours before
// now, to the nanosecond. A number of days whose seconds an int64 does not
// hold never holds.
func olderThan(t, now time.Time, days int64) bool {
	if days > math.MaxInt64/secondsPerDay {
		return false
	}
	seconds := now.Unix() - t.Unix()
	nanos := now.Nanosecond() - t.Nanosecond()
	if nanos < 0 {
		seconds--
		nanos += int(time.Second)
	}
	limit := days * secondsPerDay
	return seconds > limit || seconds == limit && nanos > 0
}
