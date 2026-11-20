package state_test

import (
	"encoding/json"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/state"
)

// A setting round trips as the JSON encode writes it, an absent key is not
// found, and the keys of Write are refused.
func TestSettingRoundTrip(t *testing.T) {
	dir, st := create(t)
	value, found, err := st.Setting("logs")
	if err != nil || found || value != nil {
		t.Fatalf("absent key: %q %v %v", value, found, err)
	}
	logs := map[string]any{"max_bytes": 1024, "classic": false}
	if err := st.PutSetting("logs", logs); err != nil {
		t.Fatal(err)
	}
	value, found, err = st.Setting("logs")
	want, _ := json.Marshal(logs)
	if err != nil || !found || string(value) != string(want) {
		t.Fatalf("logs: %q %v %v, want %s", value, found, err, want)
	}
	if err := st.PutSetting("logs", map[string]any{"classic": true}); err != nil {
		t.Fatal(err)
	}
	if value, _, _ := st.Setting("logs"); string(value) != `{"classic":true}` {
		t.Fatalf("replaced: %s", value)
	}
	var stored string
	if err := raw(t, dir).QueryRow("SELECT value FROM settings WHERE key = 'logs'").Scan(&stored); err != nil ||
		stored != `{"classic":true}` {
		t.Fatalf("row %q %v", stored, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if value, found, err := again.Setting("logs"); err != nil || !found || string(value) != `{"classic":true}` {
		t.Fatalf("after a new open: %q %v %v", value, found, err)
	}
}

// realms and anonymous belong to Store.Write: PutSetting refuses them and
// their rows stay.
func TestPutSettingRefusesTheKeysOfWrite(t *testing.T) {
	_, st := create(t)
	if err := st.Write(state.Kinds, sample()); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"realms", "anonymous"} {
		if err := st.PutSetting(key, []string{"demo-realm"}); err == nil {
			t.Errorf("PutSetting(%q) was taken", key)
		}
	}
	same(t, load(t, st), sorted())
}

// Load reads its records as before with a further key in the table.
func TestLoadWithAForeignSetting(t *testing.T) {
	_, st := create(t)
	if err := st.Write(state.Kinds, sample()); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSetting("logs", map[string]any{"max_age_days": 7}); err != nil {
		t.Fatal(err)
	}
	same(t, load(t, st), sorted())
}
