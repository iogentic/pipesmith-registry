package rest

import (
	"runtime"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/version"
)

// SystemFacts are what the system information needs beyond Env: the start
// of the server and the time of the answer.
type SystemFacts struct {
	Started, Now time.Time
}

type storeInfo struct {
	Kind      string `json:"kind"`
	Available bool   `json:"available"`
	Files     int64  `json:"files"`
	Bytes     int64  `json:"bytes"`
}

// SystemInformation is GET /beta/system/information (ruling SY7, contract
// S34): sections of maps with the version, the schema numbers of the three
// files, the Go runtime, the start time and uptime, each store with kind,
// availability and the counts of the index as GET /v1/blobstores gives
// them (the stored stores and those of env.Stores), and an empty section
// update (task 8.13 fills it). No path of a store and nothing of the host,
// the user or the environment.
func SystemInformation(env *Env, f SystemFacts) (Answer, error) {
	list, err := env.blobStoreXOs()
	if err != nil {
		return Answer{}, err
	}
	stores := map[string]any{}
	for _, b := range list {
		stores[b.Name] = storeInfo{Kind: b.Type, Available: !b.Unavailable, Files: b.BlobCount, Bytes: b.TotalSizeInBytes}
	}
	return ok(map[string]map[string]any{
		"version": {"version": version.Version, "go": runtime.Version()},
		"schema":  {"state.sqlite": state.SchemaVersion, "index.sqlite": index.SchemaVersion, "catalog.sqlite": catalog.SchemaVersion},
		"runtime": {"started": timestamp(f.Started), "uptimeSeconds": int64(f.Now.Sub(f.Started) / time.Second)},
		"stores":  stores,
		"update":  {},
	})
}
