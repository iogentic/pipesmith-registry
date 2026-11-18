package rest

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// storedAttributes are attribute texts as the old product stores them and
// the import keeps them: many integers carry a decimal point or an exponent.
var storedAttributes = map[string]string{
	"demo-docker": `{"docker":{"httpPort":14511.0,"httpsPort":1.4512E4,"v1Enabled":false},` +
		`"storage":{"blobStoreName":"store-a","writePolicy":"ALLOW"}}`,
	"demo-proxy": `{"proxy":{"remoteUrl":"https://repo.example.com/maven2","contentMaxAge":1440.0,"metadataMaxAge":-1.0},` +
		`"negativeCache":{"enabled":true,"timeToLive":1.44E3},` +
		`"httpclient":{"connection":{"timeout":60.000,"retries":2}},` +
		`"demo":{"share":0.5,"weights":[3.0,2.25],"huge":1e400}}`,
}

// storedSnapshot writes the rows into state.sqlite and loads them again,
// the way the server reads them.
func storedSnapshot(t *testing.T) *model.Snapshot {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap := &model.Snapshot{Repositories: []model.Repository{
		{Name: "demo-docker", Format: "docker", Type: "hosted", Online: true},
		{Name: "demo-proxy", Format: "maven2", Type: "proxy", Online: true},
	}}
	for i := range snap.Repositories {
		r := &snap.Repositories[i]
		dec := json.NewDecoder(strings.NewReader(storedAttributes[r.Name]))
		dec.UseNumber()
		if err := dec.Decode(&r.Attributes); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Write([]state.Kind{state.KindRepositories}, snap); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", state.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var text string
	if err := db.QueryRow("SELECT attributes FROM repositories WHERE name = 'demo-docker'").Scan(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, `"httpPort":14511.0`) {
		t.Fatalf("the stored row is %s, the test needs the form of the old product", text)
	}
	if st, err = state.Open(dir); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func storedEnv(snap *model.Snapshot) *Env {
	var defined []repos.Repo
	for _, r := range snap.Repositories {
		defined = append(defined, repos.Repo{Name: r.Name, Format: r.Format, Type: r.Type, Defined: true})
	}
	return &Env{Context: context.Background(), Snapshot: snap, Repositories: repos.New(defined, nil, nil),
		Permits: func(string) bool { return true }, Base: "http://localhost:14650"}
}

// TestStoredWholeNumbersAreServedAsIntegers: a number of the stored
// attributes whose fraction is zero is served as an integer, as the REST
// surface of the old product shows it (14511.0 as 14511). A real fraction
// and a number outside int64 are served as stored.
func TestStoredWholeNumbersAreServedAsIntegers(t *testing.T) {
	snap := storedSnapshot(t)
	env := storedEnv(snap)
	docker := map[string]any{"httpPort": json.Number("14511"), "httpsPort": json.Number("14512"), "v1Enabled": false}
	proxy := map[string]any{
		"proxy":         map[string]any{"remoteUrl": "https://repo.example.com/maven2", "contentMaxAge": json.Number("1440"), "metadataMaxAge": json.Number("-1")},
		"negativeCache": map[string]any{"enabled": true, "timeToLive": json.Number("1440")},
		"httpClient":    map[string]any{"connection": map[string]any{"timeout": json.Number("60"), "retries": json.Number("2")}},
		"demo":          map[string]any{"share": json.Number("0.5"), "weights": []any{json.Number("3"), json.Number("2.25")}, "huge": json.Number("1e400")},
	}

	a, err := RepositoryDetail(env, "docker", "hosted", "demo-docker")
	m := body(t, a, err, 200).(map[string]any)
	if !reflect.DeepEqual(m["docker"], docker) {
		t.Errorf("detail of demo-docker: docker %#v, want %#v", m["docker"], docker)
	}
	data, _ := a.JSON()
	if !strings.Contains(string(data), `"httpPort":14511,`) {
		t.Errorf("detail of demo-docker: %s", data)
	}

	a, err = RepositorySettings(env)
	items := list(t, body(t, a, err, 200))
	if got := names(items, "name"); !reflect.DeepEqual(got, []string{"demo-docker", "demo-proxy"}) {
		t.Fatalf("settings of %v", got)
	}
	if !reflect.DeepEqual(items[0]["docker"], docker) {
		t.Errorf("settings of demo-docker: docker %#v, want %#v", items[0]["docker"], docker)
	}
	for key, want := range proxy {
		if !reflect.DeepEqual(items[1][key], want) {
			t.Errorf("settings of demo-proxy: %s %#v, want %#v", key, items[1][key], want)
		}
	}

	a, err = Repository(env, "demo-proxy")
	if got := body(t, a, err, 200).(map[string]any)["attributes"]; !reflect.DeepEqual(got,
		map[string]any{"proxy": map[string]any{"remoteUrl": "https://repo.example.com/maven2"}}) {
		t.Errorf("demo-proxy: attributes %#v", got)
	}

	// the answers leave the snapshot as stored
	if got := snap.Repositories[0].Attributes["docker"].(map[string]any)["httpPort"]; got != json.Number("14511.0") {
		t.Errorf("the snapshot holds httpPort %#v after the answers, want the stored 14511.0", got)
	}
}
