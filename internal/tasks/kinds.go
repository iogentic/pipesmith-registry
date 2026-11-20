package tasks

import (
	"fmt"
	"slices"
)

// The table of task kinds (task 11 of the phase 6 plan, section Task
// Kinds): testdata/tasks/kinds.json and this Go copy, which a test keeps
// equal.

// The treatments of a row of the table.
const (
	TreatmentBuild    = "build"
	TreatmentReplaced = "replaced"
	TreatmentLater    = "later"
	TreatmentDisabled = "disabled"
)

// The type ids of the built kinds.
const (
	TypeBackup        = "db.backup"
	TypeMavenMetadata = "maven.rebuild-metadata"
	TypeRescan        = "blobstore.rescan"
	TypeUploadPurge   = "docker.upload-purge"
	TypeWorkFiles     = "blobstore.work-files"
)

// KindRow is a row of the table: a type with its id, or a class of kinds
// without a known id and the number of kinds it holds.
type KindRow struct {
	ID         string    `json:"id,omitempty"`
	Own        bool      `json:"own,omitempty"`
	Class      string    `json:"class"`
	Count      int       `json:"count,omitempty"`
	Treatment  string    `json:"treatment"`
	ReplacedBy string    `json:"replacedBy,omitempty"`
	Leg        string    `json:"leg,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Type       *TaskType `json:"type,omitempty"`
}

// The legs and the reason the table names.
const (
	legCleanup     = "6-cleanup"
	reasonNotBuilt = "not built in this release"
)

// kindTypes is the Go copy of the types of kinds.json.
var kindTypes = []KindRow{
	{ID: "blobstore.compact", Class: "compaction of a store", Treatment: TreatmentLater, Leg: legCleanup},
	{ID: TypeBackup, Class: "backup of state.sqlite", Treatment: TreatmentBuild, Type: &TaskType{ID: TypeBackup,
		Name:        "Back up the state file",
		Description: "Copies state.sqlite while the server runs into a new file in a directory outside the data directory.",
		Fields:      []TaskField{{ID: "location", Label: "Directory", Type: PropertyString, Required: true}}}},
	{ID: "repository.cleanup", Class: "cleanup run over all policies", Treatment: TreatmentLater, Leg: legCleanup},
	{ID: "repository.maven.remove-snapshots", Class: "removal of maven2 snapshots", Treatment: TreatmentLater, Leg: legCleanup},
	{ID: TypeMavenMetadata, Own: true, Class: "rebuild of maven2 metadata", Treatment: TreatmentBuild, Type: &TaskType{ID: TypeMavenMetadata,
		Name:        "Rebuild Maven metadata",
		Description: "Writes the maven-metadata.xml files and their checksums of a hosted Maven repository again from the files it holds.",
		Fields: []TaskField{
			{ID: "repositoryName", Label: "Repository", Type: PropertyRepository, Required: true},
			{ID: "groupId", Label: "Group id", Type: PropertyString},
			{ID: "artifactId", Label: "Artifact id", Type: PropertyString},
			{ID: "baseVersion", Label: "Base version", Type: PropertyString},
		}}},
	{ID: TypeRescan, Own: true, Class: "rescan of a store, which replaces the browse rebuild", Treatment: TreatmentBuild, Type: &TaskType{ID: TypeRescan,
		Name:        "Rescan blob store",
		Description: "Reads every file of a blob store again and rebuilds its part of the index, the catalog and its counts.",
		Fields:      []TaskField{{ID: "blobStoreName", Label: "Blob store", Type: PropertyBlobStore, Required: true}}}},
	{ID: TypeUploadPurge, Own: true, Class: "removal of stale docker upload sessions", Treatment: TreatmentBuild, Type: &TaskType{ID: TypeUploadPurge,
		Name:        "Purge Docker uploads",
		Description: "Ends the Docker upload sessions that had no request for longer than the given hours and removes their work files.",
		Fields: []TaskField{
			{ID: "repositoryName", Label: "Repository", Type: PropertyRepository, Required: true, Default: "*"},
			{ID: "olderThanHours", Label: "Older than (hours)", Type: PropertyNumber, Required: true, Default: float64(24)},
		}}},
	{ID: TypeWorkFiles, Own: true, Class: "removal of temporary and orphaned files of a store", Treatment: TreatmentBuild, Type: &TaskType{ID: TypeWorkFiles,
		Name:        "Remove old work files",
		Description: "Removes the work files below content/tmp of a store of this server that are older than the given hours and that no running upload holds.",
		Fields: []TaskField{
			{ID: "blobStoreName", Label: "Blob store", Type: PropertyBlobStore, Required: true},
			{ID: "olderThanHours", Label: "Older than (hours)", Type: PropertyNumber, Required: true, Default: float64(24)},
		}}},
}

// kindClasses is the Go copy of the classes of kinds.json.
var kindClasses = []KindRow{
	{Class: "rebuild of the catalog, which replaces the search rebuild", Count: 1, Treatment: TreatmentReplaced, ReplacedBy: TypeRescan},
	{Class: "recount of a store", Count: 1, Treatment: TreatmentReplaced, ReplacedBy: TypeRescan},
	{Class: "removal of unused maven2 snapshots", Count: 1, Treatment: TreatmentLater, Leg: legCleanup},
	{Class: "removal of unused components", Count: 1, Treatment: TreatmentLater, Leg: legCleanup},
	{Class: "rebuild of npm metadata", Count: 1, Treatment: TreatmentDisabled, Reason: reasonNotBuilt},
	{Class: "removal of unused docker manifests and layers", Count: 1, Treatment: TreatmentDisabled, Reason: reasonNotBuilt},
	{Class: "repair of maven2 base versions", Count: 1, Treatment: TreatmentReplaced, ReplacedBy: TypeRescan},
	{Class: "reconcile of npm search data", Count: 1, Treatment: TreatmentReplaced, ReplacedBy: TypeRescan},
	{Class: "compaction scheduling by expiry", Count: 1, Treatment: TreatmentLater, Leg: legCleanup},
	{Class: "kinds of a format without assets", Count: 6, Treatment: TreatmentDisabled, Reason: "the format holds no asset in this server"},
	{Class: "removal of orphaned keys", Count: 1, Treatment: TreatmentDisabled, Reason: "a token row is removed with its user"},
	{Class: "malware scan", Count: 1, Treatment: TreatmentDisabled, Reason: "decision P12 of the plans"},
	{Class: "migration of cloud stores", Count: 1, Treatment: TreatmentDisabled, Reason: "this server keeps file stores only"},
	{Class: "publish and unpublish of the maven2 search index", Count: 2, Treatment: TreatmentDisabled, Reason: "this server keeps no maven2 search index"},
	{Class: "repair plan kinds", Count: 2, Treatment: TreatmentDisabled, Reason: "the area is not in use"},
	{Class: "script execution", Count: 1, Treatment: TreatmentDisabled, Reason: "this server runs no script"},
	{Class: "health check reports of proxies", Count: 1, Treatment: TreatmentDisabled, Reason: "decision P12 of the plans"},
}

// KindRows are copies of the types and the classes of the table.
func KindRows() ([]KindRow, []KindRow) {
	return cloneRows(kindTypes), cloneRows(kindClasses)
}

func cloneRows(rows []KindRow) []KindRow {
	out := slices.Clone(rows)
	for i := range out {
		if out[i].Type != nil {
			tt := cloneType(*out[i].Type)
			out[i].Type = &tt
		}
	}
	return out
}

func cloneType(t TaskType) TaskType {
	t.Fields = slices.Clone(t.Fields)
	for i := range t.Fields {
		t.Fields[i].Options = slices.Clone(t.Fields[i].Options)
	}
	return t
}

// KindRowOf is the row of a type id, false for a type the table does not
// know.
func KindRowOf(id string) (KindRow, bool) {
	at := slices.IndexFunc(kindTypes, func(r KindRow) bool { return r.ID == id })
	if at < 0 {
		return KindRow{}, false
	}
	return cloneRows(kindTypes[at : at+1])[0], true
}

// TypeList is the built types of the table, in its order.
func TypeList() []TaskType {
	var list []TaskType
	for _, r := range kindTypes {
		if r.Treatment == TreatmentBuild {
			list = append(list, cloneType(*r.Type))
		}
	}
	return list
}

// reasonOf is the reason of a stored task of a type without a kind: a
// later row names its leg (ruling KN1), a disabled row gives its reason, a
// type the table does not know is not known (ruling KN3), and a built type
// without a kind is not built.
func reasonOf(typeID string) string {
	r, ok := KindRowOf(typeID)
	switch {
	case !ok:
		return fmt.Sprintf("the task type %s is not known", typeID)
	case r.Treatment == TreatmentLater:
		return fmt.Sprintf("the task type %s is not built yet; leg %s builds it", typeID, r.Leg)
	case r.Treatment == TreatmentDisabled:
		return fmt.Sprintf("the task type %s is disabled: %s", typeID, r.Reason)
	}
	return fmt.Sprintf("the task type %s is not built", typeID)
}
