package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// The cleanup policies, capabilities and tasks of the content set (leg
// 6-state). Tables and columns follow the schema list of the old database
// (tools/h2export/README.md, S6); the keys of a task in the job data of the
// scheduler follow S10 of the same file. Every row arrives with the origin
// import. The rulings S4 to S7 of the leg apply: tasks arrive paused (I12),
// a policy keeps every criterion (A9), a cron expression stays as stored,
// and a secret property goes into a secret file, never into a record (A11).

const (
	policyTable     = "CLEANUP_POLICY"
	capabilityTable = "CAPABILITY_STORAGE_ITEM"
	jobTable        = "QRTZ_JOB_DETAILS"
	triggerTable    = "QRTZ_TRIGGERS"

	policyFile     = "cleanup_policy.jsonl"
	capabilityFile = "capability_storage_item.jsonl"
	jobFile        = "qrtz_job_details.jsonl"
	triggerFile    = "qrtz_triggers.jsonl"
)

// The keys of a task in the job data of its job and of its trigger (S10).
const (
	keyID       = ".id"
	keyName     = ".name"
	keyTypeID   = ".typeId"
	keyEnabled  = ".enabled"
	keyLastRun  = "lastRunState."
	keyType     = "schedule.type"
	keyStartAt  = "schedule.startAt"
	keyCron     = "schedule.cronExpression"
	keyDaysToRn = "schedule.daysToRun"
)

// CronNote is the note of every imported cron task (open point 10, option
// B: nothing is rewritten).
const CronNote = "the old server read a cron expression in the time zone of its process; this server reads it in UTC"

// PolicyNote is a cleanup policy that may not run (A9): the names of its
// criteria this server does not know, and the reason in words: those names,
// no criterion when no criterion holds a value, or a value the server does
// not take (ruling S14).
type PolicyNote struct {
	Name    string   `json:"name"`
	Unknown []string `json:"unknown"`
	Reason  string   `json:"reason"`
}

// TaskNote is a task whose schedule this server does not take, with the
// reason. The task is stored with the schedule as read.
type TaskNote struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// SecretProperty is a property of a task or capability whose key holds a
// secret word. Key is its path in the properties, as auth.password or
// hosts[0].token for a key inside a value. Kept is true when its value went
// into the secret file state.PropertySecretPath names, false when the
// export program or the import took it out and it must be entered again.
// It never holds the value.
type SecretProperty struct {
	Kind string `json:"kind"` // state.SecretKindTask or state.SecretKindCapability
	ID   string `json:"id"`
	Key  string `json:"key"`
	Kept bool   `json:"kept"`
}

// pendingSecret is a secret property to write after the records. The value
// is behind a function, so no print of an Export shows it.
type pendingSecret struct {
	SecretProperty
	value func() []byte
}

// administration holds what an export of the administration tables gives
// beyond the records.
type administration struct {
	PoliciesNotRun  []PolicyNote `json:"policiesNotRun"`
	TaskNotes       []TaskNote   `json:"taskNotes"`
	CronTasks       []string     `json:"cronTasks"`
	UnreadableTasks []string     `json:"unreadableTasks"`
	// TriggersWithoutJob are the triggers whose job the export does not
	// hold, as "<group>.<name>"; none of them makes a task.
	TriggersWithoutJob []string         `json:"triggersWithoutJob"`
	SecretProperties   []SecretProperty `json:"secretProperties"`
	secrets            []pendingSecret
}

func emptyAdministration() administration {
	return administration{PoliciesNotRun: []PolicyNote{}, TaskNotes: []TaskNote{}, CronTasks: []string{},
		UnreadableTasks: []string{}, TriggersWithoutJob: []string{}, SecretProperties: []SecretProperty{}}
}

// textValues are the values of an object as strings: a string as it is,
// any other value as its JSON text after clean took out the secret keys at
// every depth, each path of them given to removed (ruling S9). Each key with
// a secret word goes to secret, and the rest to the map.
func textValues(object map[string]any, secret func(key, value string), removed func(path string)) (map[string]string, error) {
	out := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(object)) {
		value, ok := object[key].(string)
		if ok && secretKey(key) {
			secret(key, value)
			continue
		}
		if !ok {
			raw := object[key]
			var paths []string
			if !secretKey(key) {
				raw = clean(raw, key, &paths)
			}
			text, err := json.Marshal(raw)
			if err != nil {
				return nil, err
			}
			value = string(text)
			if secretKey(key) {
				secret(key, value)
				continue
			}
			slices.Sort(paths)
			for _, path := range paths {
				removed(path)
			}
		}
		out[key] = value
	}
	return out, nil
}

// removedProperties lists the keys the export program removed from the
// column, by their paths in _removed below the column: a key at the top
// as its name, a key inside a value with its path.
func (a *administration) removedProperties(kind, id, column string, removed []string) {
	for _, path := range removed {
		if key, ok := strings.CutPrefix(path, column+"."); ok && key != "" {
			a.enterAgain(kind, id, key)
		}
	}
}

// enterAgain records a secret property the import took out of a value and
// does not keep: it is entered again.
func (a *administration) enterAgain(kind, id, key string) {
	a.SecretProperties = append(a.SecretProperties, SecretProperty{Kind: kind, ID: id, Key: key})
}

// keep records a secret property found in a row, to be written into its
// secret file.
func (a *administration) keep(kind, id, key, value string) {
	p := SecretProperty{Kind: kind, ID: id, Key: key, Kept: true}
	data := []byte(value)
	a.SecretProperties = append(a.SecretProperties, p)
	a.secrets = append(a.secrets, pendingSecret{SecretProperty: p, value: func() []byte { return data }})
}

func mapPolicies(export *Export, data map[string][]row) error {
	snap := export.Snapshot
	names := map[string]bool{}
	err := eachRow(policyFile, data[policyTable], func(rd *reader) error {
		p := model.CleanupPolicy{Name: rd.text("name"), Notes: rd.text("notes"), Format: rd.text("format"),
			Criteria: rd.object("criteria"), Origin: model.OriginImport}
		switch {
		case rd.err != nil:
			return nil // eachRow returns the field error
		case p.Name == "":
			return fmt.Errorf("%s: row %d has no name", rd.file, rd.row)
		case names[p.Name]:
			return fmt.Errorf("%s: cleanup policy %q is given twice", rd.file, p.Name)
		}
		names[p.Name] = true
		if ok, unknown := admin.Runnable(p.Criteria); !ok {
			export.PoliciesNotRun = append(export.PoliciesNotRun, PolicyNote{Name: p.Name, Unknown: unknown,
				Reason: policyReason(p.Criteria, unknown)})
		}
		snap.CleanupPolicies = append(snap.CleanupPolicies, p)
		return nil
	})
	slices.SortFunc(snap.CleanupPolicies, func(a, b model.CleanupPolicy) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(export.PoliciesNotRun, func(a, b PolicyNote) int { return strings.Compare(a.Name, b.Name) })
	return err
}

// policyReason is the reason a policy that admin.Runnable refuses may not
// run.
func policyReason(criteria map[string]any, unknown []string) string {
	switch {
	case len(unknown) > 0:
		return "criteria this server does not know: " + strings.Join(unknown, ", ")
	case !slices.ContainsFunc(slices.Collect(maps.Values(criteria)), func(v any) bool { return v != nil }):
		return "no criterion"
	}
	return "a criterion holds a value this server does not take"
}

// importedTasks counts the stored tasks of origin import.
func importedTasks(stored *model.Snapshot) int {
	n := 0
	for _, t := range stored.Tasks {
		if t.Origin == model.OriginImport {
			n++
		}
	}
	return n
}

func mapCapabilities(export *Export, data map[string][]row) error {
	snap := export.Snapshot
	ids := map[string]bool{}
	err := eachRow(capabilityFile, data[capabilityTable], func(rd *reader) error {
		c := model.Capability{ID: rd.text("id"), TypeID: rd.text("type"), Enabled: rd.flag("enabled"), Notes: rd.text("notes"),
			Origin: model.OriginImport}
		properties := rd.object("properties")
		switch {
		case rd.err != nil:
			return nil // eachRow returns the field error
		case c.ID == "":
			return fmt.Errorf("%s: row %d has no id", rd.file, rd.row)
		case ids[c.ID]:
			return fmt.Errorf("%s: capability %q is given twice", rd.file, c.ID)
		}
		ids[c.ID] = true
		var err error
		c.Properties, err = textValues(properties, func(key, value string) {
			export.keep(state.SecretKindCapability, c.ID, key, value)
		}, func(path string) { export.enterAgain(state.SecretKindCapability, c.ID, path) })
		if err != nil {
			return fmt.Errorf("%s: capability %q: properties cannot be read", rd.file, c.ID)
		}
		export.removedProperties(state.SecretKindCapability, c.ID, "properties", rd.removed())
		snap.Capabilities = append(snap.Capabilities, c)
		return nil
	})
	slices.SortFunc(snap.Capabilities, func(a, b model.Capability) int { return strings.Compare(a.ID, b.ID) })
	return err
}

// triggerSecrets lists the secret keys of the job data of the triggers of
// the task id like those of its job (ruling S14): a value of the first
// trigger, the one read, is kept unless the job data holds the same key; a
// value of a further trigger and a key the export or the import took out
// are entered again.
func (a *administration) triggerSecrets(id string, found []trigger) {
	for i, tr := range found {
		for _, secret := range tr.secrets {
			taken := slices.ContainsFunc(a.secrets, func(p pendingSecret) bool {
				return p.Kind == state.SecretKindTask && p.ID == id && p.Key == secret[0]
			})
			if i == 0 && !taken {
				a.keep(state.SecretKindTask, id, secret[0], secret[1])
			} else {
				a.enterAgain(state.SecretKindTask, id, secret[0])
			}
		}
		for _, path := range tr.removed {
			a.enterAgain(state.SecretKindTask, id, path)
		}
	}
}

// trigger is the part of a trigger row a task needs.
type trigger struct {
	name    string // <group>.<name>
	state   string
	data    map[string]string
	secrets [][2]string // key and value of each secret key of the job data
	removed []string    // the paths of the secret keys taken out, below the job data
}

// mapTasks joins each job with its trigger by group and name. A job whose
// job data the export program could not read is listed and left out.
func mapTasks(export *Export, data map[string][]row) error {
	jobs, withJobs := data[jobTable]
	triggerRows, withTriggers := data[triggerTable]
	if withJobs != withTriggers {
		return fmt.Errorf("the export holds one of %s and %s; the tasks need both", jobFile, triggerFile)
	}
	if !withJobs {
		return nil
	}
	triggers := map[string][]trigger{}
	err := eachRow(triggerFile, triggerRows, func(rd *reader) error {
		key := rd.text("job_group") + "\x00" + rd.text("job_name")
		tr := trigger{name: rd.text("trigger_group") + "." + rd.text("trigger_name"), state: rd.text("trigger_state")}
		values, err := textValues(rd.object("job_data"), func(key, value string) {
			tr.secrets = append(tr.secrets, [2]string{key, value})
		}, func(path string) { tr.removed = append(tr.removed, path) })
		if err != nil {
			return fmt.Errorf("%s: row %d: job_data cannot be read", rd.file, rd.row)
		}
		for _, path := range rd.removed() {
			if key, ok := strings.CutPrefix(path, "job_data."); ok && key != "" {
				tr.removed = append(tr.removed, key)
			}
		}
		tr.data = values
		triggers[key] = append(triggers[key], tr)
		return nil
	})
	if err != nil {
		return err
	}
	snap := export.Snapshot
	ids := map[string]bool{}
	jobKeys := map[string]bool{}
	err = eachRow(jobFile, jobs, func(rd *reader) error {
		name := rd.text("job_name")
		jobKeys[rd.text("job_group")+"\x00"+name] = true
		if rd.r["job_data"] == nil {
			export.UnreadableTasks = append(export.UnreadableTasks, name)
			return nil
		}
		object := rd.object("job_data")
		if rd.err != nil {
			return nil // eachRow returns the field error
		}
		id, _ := object[keyID].(string)
		if id == "" {
			export.UnreadableTasks = append(export.UnreadableTasks, name)
			return nil
		}
		if ids[id] {
			return fmt.Errorf("%s: task %q is given twice", rd.file, id)
		}
		ids[id] = true
		values, err := textValues(object, func(key, value string) { export.keep(state.SecretKindTask, id, key, value) },
			func(path string) { export.enterAgain(state.SecretKindTask, id, path) })
		if err != nil {
			return fmt.Errorf("%s: task %q: job_data cannot be read", rd.file, id)
		}
		export.removedProperties(state.SecretKindTask, id, "job_data", rd.removed())
		t := model.Task{ID: id, Name: values[keyName], TypeID: values[keyTypeID], Origin: model.OriginImport,
			Properties: map[string]string{}, ImportedState: map[string]string{"enabled": values[keyEnabled]}}
		for key, value := range values {
			switch {
			case strings.HasPrefix(key, keyLastRun):
				t.ImportedState[key] = value
			case !strings.HasPrefix(key, "."):
				t.Properties[key] = value
			}
		}
		found := triggers[rd.text("job_group")+"\x00"+name]
		export.triggerSecrets(id, found)
		if len(found) == 0 {
			t.Schedule = model.TaskSchedule{Type: tasks.TypeManual}
			export.TaskNotes = append(export.TaskNotes, TaskNote{ID: id, Reason: "the job has no trigger; read as manual"})
		} else {
			t.ImportedState["triggerState"] = found[0].state
			var notes []string
			t.Schedule, notes = taskSchedule(found[0].data)
			if len(found) > 1 {
				notes = append(notes, fmt.Sprintf("the job has %d triggers; the first is read", len(found)))
			}
			for _, note := range notes {
				export.TaskNotes = append(export.TaskNotes, TaskNote{ID: id, Reason: note})
			}
		}
		if t.Schedule.Type == tasks.TypeCron {
			export.CronTasks = append(export.CronTasks, id)
		}
		snap.Tasks = append(snap.Tasks, t)
		return nil
	})
	for key, list := range triggers {
		if !jobKeys[key] {
			for _, tr := range list {
				export.TriggersWithoutJob = append(export.TriggersWithoutJob, tr.name)
			}
		}
	}
	slices.Sort(export.TriggersWithoutJob)
	slices.SortFunc(snap.Tasks, func(a, b model.Task) int { return strings.Compare(a.ID, b.ID) })
	slices.SortStableFunc(export.TaskNotes, func(a, b TaskNote) int { return strings.Compare(a.ID, b.ID) })
	slices.Sort(export.CronTasks)
	slices.Sort(export.UnreadableTasks)
	return err
}

// taskSchedule reads the schedule of a trigger's job data as stored, and
// notes each part this server does not take: a start that is no time, a day
// that is no number, and every refusal of tasks.Schedule.Trigger, the cron
// expression among them (ruling S6).
func taskSchedule(data map[string]string) (model.TaskSchedule, []string) {
	var notes []string
	s := model.TaskSchedule{Type: data[keyType]}
	if text := data[keyStartAt]; text != "" {
		at, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			notes = append(notes, keyStartAt+" is no time of the form 2006-01-02T15:04:05Z07:00")
		} else {
			s.StartAt = at.UTC()
		}
	}
	days := data[keyDaysToRn]
	switch s.Type {
	case tasks.TypeCron:
		s.Cron = data[keyCron]
	case tasks.TypeWeekly:
		if days != "" {
			s.WeekDays = strings.Split(days, ",")
		}
	case tasks.TypeMonthly:
		for _, day := range strings.Split(days, ",") {
			n, err := strconv.Atoi(day)
			if err != nil {
				notes = append(notes, keyDaysToRn+" holds a day that is no number")
				continue
			}
			s.MonthDays = append(s.MonthDays, n)
		}
	}
	_, err := tasks.Schedule{Type: s.Type, StartAt: s.StartAt, WeekDays: s.WeekDays, MonthDays: s.MonthDays,
		Cron: s.Cron}.Trigger()
	if err != nil {
		notes = append(notes, err.Error())
	}
	return s, notes
}

// localKeys are the keys of the rows of origin local of each kind of step
// 4, as "cleanup policy <name>", "task <id>" and "capability <id>".
func localKeys(snap *model.Snapshot) map[string]bool {
	keys := map[string]bool{}
	for _, p := range snap.CleanupPolicies {
		if p.Origin == model.OriginLocal {
			keys["cleanup policy "+p.Name] = true
		}
	}
	for _, t := range snap.Tasks {
		if t.Origin == model.OriginLocal {
			keys["task "+t.ID] = true
		}
	}
	for _, c := range snap.Capabilities {
		if c.Origin == model.OriginLocal {
			keys["capability "+c.ID] = true
		}
	}
	return keys
}

// merge replaces the rows of origin import of the kinds of step 5 the
// export holds with its rows, keeps every row of origin local, and leaves
// out an imported row whose key a local row holds (ruling S4). It gives the
// keys of the rows left out, sorted.
func merge(snap, from *model.Snapshot, kinds []state.Kind) []string {
	local := localKeys(snap)
	var kept []string
	skip := func(key string) bool {
		if local[key] {
			kept = append(kept, key)
			return true
		}
		return false
	}
	if slices.Contains(kinds, state.KindCleanupPolicies) {
		out := slices.DeleteFunc(slices.Clone(snap.CleanupPolicies), func(p model.CleanupPolicy) bool {
			return p.Origin != model.OriginLocal
		})
		for _, p := range from.CleanupPolicies {
			if !skip("cleanup policy " + p.Name) {
				out = append(out, p)
			}
		}
		slices.SortFunc(out, func(a, b model.CleanupPolicy) int { return strings.Compare(a.Name, b.Name) })
		snap.CleanupPolicies = out
	}
	if slices.Contains(kinds, state.KindTasks) {
		out := slices.DeleteFunc(slices.Clone(snap.Tasks), func(t model.Task) bool { return t.Origin != model.OriginLocal })
		for _, t := range from.Tasks {
			if !skip("task " + t.ID) {
				out = append(out, t)
			}
		}
		slices.SortFunc(out, func(a, b model.Task) int { return strings.Compare(a.ID, b.ID) })
		snap.Tasks = out
	}
	if slices.Contains(kinds, state.KindCapabilities) {
		out := slices.DeleteFunc(slices.Clone(snap.Capabilities), func(c model.Capability) bool {
			return c.Origin != model.OriginLocal
		})
		for _, c := range from.Capabilities {
			if !skip("capability " + c.ID) {
				out = append(out, c)
			}
		}
		slices.SortFunc(out, func(a, b model.Capability) int { return strings.Compare(a.ID, b.ID) })
		snap.Capabilities = out
	}
	slices.Sort(kept)
	return kept
}

// copyKinds sets the records of the kinds named, other than those of step 5,
// from from, as Write would replace them.
func copyKinds(snap, from *model.Snapshot, kinds []state.Kind) {
	for _, k := range kinds {
		switch k {
		case state.KindUsers:
			snap.Users, snap.RoleMappings = from.Users, from.RoleMappings
		case state.KindRoles:
			snap.Roles = from.Roles
		case state.KindPrivileges:
			snap.Privileges = from.Privileges
		case state.KindLDAP:
			snap.LDAP = from.LDAP
		case state.KindRealms:
			snap.Realms = from.Realms
		case state.KindAnonymous:
			snap.Anonymous = from.Anonymous
		case state.KindBlobStores:
			snap.BlobStores = from.BlobStores
		case state.KindRepositories:
			snap.Repositories = from.Repositories
		case state.KindDefaultRole:
			snap.DefaultRole = from.DefaultRole
		}
	}
}

// maxFileName bounds the name of a secret file: the longest name the file
// systems of Linux and macOS take is 255 bytes.
const maxFileName = 255

// secretOwner names the record of a secret property, as "task <id>".
func secretOwner(p SecretProperty) string {
	return map[string]string{state.SecretKindTask: "task ", state.SecretKindCapability: "capability "}[p.Kind] + p.ID
}

// checkSecrets refuses the import when a secret file it would write cannot
// be written: a value above state.MaxSecretBytes or a file name above
// maxFileName (ruling S10). It runs before the state transaction, and its
// message names the record and the key, never the value.
func checkSecrets(dataDir string, pending []pendingSecret) error {
	for _, p := range pending {
		switch {
		case len(p.value()) > state.MaxSecretBytes:
			return fmt.Errorf("%s: the secret property %s has more than %d bytes; nothing was imported", secretOwner(p.SecretProperty),
				p.Key, state.MaxSecretBytes)
		case len(filepath.Base(state.PropertySecretPath(dataDir, p.Kind, p.ID, p.Key))) > maxFileName:
			return fmt.Errorf("%s: the secret property %s gives a secret file name of more than %d bytes; nothing was imported",
				secretOwner(p.SecretProperty), p.Key, maxFileName)
		}
	}
	return nil
}

// keptWritten gives the list of secret properties with Kept false for each
// of a row left out, whose file is not written (ruling S11).
func keptWritten(list []SecretProperty, left []string) []SecretProperty {
	out := slices.Clone(list)
	for i := range out {
		if slices.Contains(left, secretOwner(out[i])) {
			out[i].Kept = false
		}
	}
	return out
}

// writeSecrets writes the secret properties of the rows that were written
// into their secret files. An error names the file, never the value.
func writeSecrets(dataDir string, pending []pendingSecret, left []string) error {
	for _, p := range pending {
		if slices.Contains(left, secretOwner(p.SecretProperty)) {
			continue
		}
		path := state.PropertySecretPath(dataDir, p.Kind, p.ID, p.Key)
		if err := state.WriteSecret(path, p.value()); err != nil {
			return fmt.Errorf("state.sqlite holds the import, but the secret file %s was not written: %w", path, err)
		}
	}
	return nil
}

// removeStaleSecrets removes each secret file of a task or capability that
// belongs to no stored row and key any more (ruling S12): of a kind the
// export holds, a row of origin import keeps the files the import wrote
// and a row of origin local keeps all of its files; of a kind the export
// does not hold, every stored row keeps all of its files. Other files of
// the folder, the LDAP secrets among them, are not touched.
func removeStaleSecrets(dataDir string, stored *model.Snapshot, kinds []state.Kind, pending []pendingSecret, left []string) error {
	dir := filepath.Join(dataDir, state.SecretsDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("state.sqlite holds the import, but the folder %s cannot be read: %w", dir, err)
	}
	keep := map[string]bool{}
	for _, p := range pending {
		if !slices.Contains(left, secretOwner(p.SecretProperty)) {
			keep[filepath.Base(state.PropertySecretPath(dataDir, p.Kind, p.ID, p.Key))] = true
		}
	}
	var owners []string // the prefixes of rows that keep all of their files
	add := func(kind string, k state.Kind, id, origin string) {
		if origin == model.OriginLocal || !slices.Contains(kinds, k) {
			owners = append(owners, filepath.Base(state.PropertySecretPath(dataDir, kind, id, "")))
		}
	}
	for _, t := range stored.Tasks {
		add(state.SecretKindTask, state.KindTasks, t.ID, t.Origin)
	}
	for _, c := range stored.Capabilities {
		add(state.SecretKindCapability, state.KindCapabilities, c.ID, c.Origin)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, state.SecretKindTask+"-") && !strings.HasPrefix(name, state.SecretKindCapability+"-") {
			continue
		}
		if keep[name] || slices.ContainsFunc(owners, func(prefix string) bool { return strings.HasPrefix(name, prefix) }) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("state.sqlite holds the import, but the stale secret file %s was not removed: %w",
				filepath.Join(dir, name), err)
		}
	}
	return nil
}
