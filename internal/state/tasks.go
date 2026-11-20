package state

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The tasks of step 5: the schedule as JSON in the shape of tasks.Schedule,
// the properties as a JSON object of strings, and the state the old row
// held, NULL for a task of origin local. A property whose key holds a
// secret word is no part of a record (A11): the import keeps it in a secret
// file (PropertySecretPath). Every change goes through Update, so the
// generation rises. No task starts here.

func writeTasks(tx *sql.Tx, snap *model.Snapshot) error {
	if err := exec(tx, "DELETE FROM tasks"); err != nil {
		return err
	}
	for _, t := range snap.Tasks {
		if t.ID == "" {
			return errors.New("a task has no id")
		}
		if err := checkOrigin(fmt.Sprintf("task %q", t.ID), t.Origin); err != nil {
			return err
		}
		t.Schedule.StartAt = t.Schedule.StartAt.UTC()
		schedule, err := encode(t.Schedule)
		if err != nil {
			return fmt.Errorf("schedule of task %q: %w", t.ID, err)
		}
		properties, err := textMap(t.Properties)
		if err != nil {
			return fmt.Errorf("properties of task %q: %w", t.ID, err)
		}
		var imported any
		if t.ImportedState != nil {
			if imported, err = textMap(t.ImportedState); err != nil {
				return fmt.Errorf("imported state of task %q: %w", t.ID, err)
			}
		}
		if err := exec(tx, "INSERT INTO tasks VALUES (?, ?, ?, ?, ?, ?, ?, ?)", t.ID, t.Name, t.TypeID, flag(t.Enabled),
			schedule, properties, imported, t.Origin); err != nil {
			return fmt.Errorf("task %q: %w", t.ID, err)
		}
	}
	return nil
}

func loadTasks(tx *sql.Tx, snap *model.Snapshot) error {
	snap.Tasks = []model.Task{}
	return each(tx, "SELECT id, name, type_id, enabled, schedule, properties, imported_state, origin FROM tasks ORDER BY id",
		func(rows *sql.Rows) error {
			var t model.Task
			var enabled int
			var schedule, properties string
			var imported sql.NullString
			if err := rows.Scan(&t.ID, &t.Name, &t.TypeID, &enabled, &schedule, &properties, &imported, &t.Origin); err != nil {
				return err
			}
			t.Enabled = enabled != 0
			if err := decode(schedule, &t.Schedule); err != nil {
				return fmt.Errorf("schedule of task %q: %w", t.ID, err)
			}
			var err error
			if t.Properties, err = stringMap(properties); err != nil {
				return fmt.Errorf("properties of task %q: %w", t.ID, err)
			}
			if imported.Valid {
				if t.ImportedState, err = stringMap(imported.String); err != nil {
					return fmt.Errorf("imported state of task %q: %w", t.ID, err)
				}
			}
			snap.Tasks = append(snap.Tasks, t)
			return nil
		})
}

// Tasks are the tasks by id. The list is never nil.
func (st *Store) Tasks() ([]model.Task, error) {
	snap, err := st.readOne(loadTasks)
	if err != nil {
		return nil, err
	}
	return snap.Tasks, nil
}

// PutTask adds t, or replaces the task of its id, and raises the generation.
func (st *Store) PutTask(t model.Task) error {
	_, err := st.change(KindTasks, func(snap *model.Snapshot) (bool, error) {
		snap.Tasks = slices.DeleteFunc(snap.Tasks, func(x model.Task) bool { return x.ID == t.ID })
		snap.Tasks = append(snap.Tasks, t)
		slices.SortFunc(snap.Tasks, func(a, b model.Task) int { return strings.Compare(a.ID, b.ID) })
		return true, nil
	})
	return err
}

// RemoveTask removes the task of the id and raises the generation. false,
// and no write, when there is none. The runs of the task stay.
func (st *Store) RemoveTask(id string) (bool, error) {
	return st.change(KindTasks, func(snap *model.Snapshot) (bool, error) {
		n := len(snap.Tasks)
		snap.Tasks = slices.DeleteFunc(snap.Tasks, func(x model.Task) bool { return x.ID == id })
		return len(snap.Tasks) != n, nil
	})
}

// The kinds of records whose properties may have secret files.
const (
	SecretKindTask       = "task"
	SecretKindCapability = "capability"
)

// PropertySecretPath is the secret file of the property key of the task or
// capability id (kind SecretKindTask or SecretKindCapability), in the
// folder of the LDAP secrets (secrets.go): secrets/<kind>-<id>+<key>, each
// byte of id and key outside A-Z, a-z, 0-9, '.', '_' and '-' written as
// %XX, a leading '.' as well, so '+' parts the two and no name leaves the
// folder.
func PropertySecretPath(dataDir, kind, id, key string) string {
	return filepath.Join(dataDir, SecretsDir, kind+"-"+secretName(id)+"+"+secretName(key))
}

func secretName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.' && i == 0:
			fmt.Fprintf(&b, "%%%02X", c)
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// PropertySecretFiles are the secret files of the properties of the task or
// capability id (kind SecretKindTask or SecretKindCapability), sorted; none
// without a secrets folder.
func PropertySecretFiles(dataDir, kind, id string) ([]string, error) {
	dir := filepath.Join(dataDir, SecretsDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	prefix := kind + "-" + secretName(id) + "+"
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files, nil
}
