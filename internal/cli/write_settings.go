package cli

import (
	"encoding/json"
	"maps"
	"strconv"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/listeners"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pystr"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// writePolicies are the values of storage.writePolicy of the old product.
var writePolicies = []string{"ALLOW", "ALLOW_ONCE", "DENY"}

// byteCount is a count of bytes in decimal digits that fits an int64.
func byteCount(value string) (any, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if !asciiDigits(value) || err != nil {
		return nil, &typeError{pystr.Repr(value) + " is no count of bytes"}
	}
	return n, nil
}

// reposWriteSettings is repos write-settings <name>: it shows, or with an
// option sets, the write settings of a hosted repository in its record in
// state.sqlite. storage.writePolicy keeps the spelling of the old product;
// max_body_bytes, free_space_floor_bytes and write_store are attributes of
// this server that REST never takes from a body (ruling ST13), so this
// command is where the host sets them. write_store takes local-write alone
// (ST2). A change goes through admin.Change and raises the generation.
func (c *command) reposWriteSettings() (int, error) {
	name, _ := c.text("name")
	policy, setPolicy := c.text("write_policy")
	store, setStore := c.text("write_store")
	maxBody, setMax := c.ns.values["max_body_bytes"].(int64)
	floor, setFloor := c.ns.values["free_space_floor_bytes"].(int64)
	switch {
	case setMax && maxBody < 1:
		return 1, refuse("--max-body-bytes is at least 1; to refuse every write, use --write-policy DENY")
	case setStore && store != admin.WriteStore:
		return 1, refuse("a repository of this server writes into the store %s, not into %s; nothing changed", admin.WriteStore, store)
	}
	return c.withState(false, func(st *state.Store, cfg *config.Config) (int, error) {
		defined, err := listeners.LoadRepositories(cfg, openIndexReader)
		if err != nil {
			return 1, err
		}
		check := func(snap *model.Snapshot) (repos.Repo, int, error) {
			repo, ok := defined.WithAttributes(snap.Repositories).Get(name)
			i := recordIndex(snap, name)
			switch {
			case !ok:
				return repo, i, refuse("no repository named %s; see: repos", name)
			case repo.Type != "hosted":
				return repo, i, refuse("%s is a %s repository; only a hosted repository takes writes", name, repo.Type)
			case i < 0:
				return repo, i, refuse("%s has no record in state.sqlite, so it has no write settings to change; "+
					"write the records with import or import-state", name)
			}
			return repo, i, nil
		}
		if !setPolicy && !setStore && !setMax && !setFloor {
			snap, err := st.Load()
			if err != nil {
				return 1, err
			}
			repo, _, err := check(snap)
			if err != nil {
				return 1, err
			}
			c.printWriteSettings(repo)
			return 0, nil
		}
		after, err := change(st, cfg, []state.Kind{state.KindRepositories}, func(snap *model.Snapshot) error {
			_, i, err := check(snap)
			if err != nil {
				return err
			}
			rec := &snap.Repositories[i]
			attributes := maps.Clone(rec.Attributes)
			if attributes == nil {
				attributes = map[string]any{}
			}
			if setPolicy {
				storage, _ := attributes["storage"].(map[string]any)
				storage = maps.Clone(storage)
				if storage == nil {
					storage = map[string]any{}
				}
				storage["writePolicy"] = policy
				attributes["storage"] = storage
			}
			if setMax {
				attributes["max_body_bytes"] = json.Number(strconv.FormatInt(maxBody, 10))
			}
			if setFloor {
				attributes["free_space_floor_bytes"] = json.Number(strconv.FormatInt(floor, 10))
			}
			if setStore {
				attributes["write_store"] = store
			}
			rec.Attributes = attributes
			return nil
		})
		if err != nil {
			return 1, err
		}
		repo, _ := defined.WithAttributes(after.Repositories).Get(name)
		c.printWriteSettings(repo)
		c.print("a running server reads them at its next load of state.sqlite")
		return 0, nil
	})
}

// recordIndex is the place of the record of name in snap, -1 without one.
func recordIndex(snap *model.Snapshot, name string) int {
	for i := range snap.Repositories {
		if snap.Repositories[i].Name == name {
			return i
		}
	}
	return -1
}

// printWriteSettings prints the four settings as the server reads them,
// each marked when its attribute is absent and a default applies.
func (c *command) printWriteSettings(repo repos.Repo) {
	w := repo.WriteSettings()
	mark := func(m map[string]any, key string) string {
		if _, ok := m[key]; ok {
			return ""
		}
		return " (default)"
	}
	policy := w.WritePolicy
	if policy == "" {
		policy = "none, every write is refused"
	}
	c.print("write settings of %s:", repo.Name)
	c.print("  storage.writePolicy     %s", policy)
	c.print("  max_body_bytes          %d%s", w.MaxBodyBytes, mark(repo.Attributes, "max_body_bytes"))
	c.print("  free_space_floor_bytes  %d%s", w.FreeSpaceFloor, mark(repo.Attributes, "free_space_floor_bytes"))
	c.print("  write_store             %s%s", w.WriteStore, mark(repo.Attributes, "write_store"))
}
