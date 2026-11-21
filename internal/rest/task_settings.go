package rest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// The task settings of leg 6-api (contract S39 and S40, ruling AP5): the
// task types, and the read, create, save and delete of a task in the shape
// TaskSettings. A change is checked by admin.CheckTask against
// tasks.TypeList and the stored settings (ruling KN16) in the transaction
// of admin.Change, and the registry takes it once it is committed.

// taskKinds are the kinds a change of a task writes.
var taskKinds = []state.Kind{state.KindTasks}

// scheduleXO is the schedule of TaskSettings; startAt is RFC 3339 in UTC.
type scheduleXO struct {
	Type      string   `json:"type"`
	StartAt   string   `json:"startAt,omitempty"`
	WeekDays  []string `json:"weekDays,omitempty"`
	MonthDays []int    `json:"monthDays,omitempty"`
	Cron      string   `json:"cron,omitempty"`
}

// taskSettingsXO is TaskSettings.
type taskSettingsXO struct {
	ID         string            `json:"id"`
	TypeID     string            `json:"typeId"`
	Name       string            `json:"name"`
	Enabled    bool              `json:"enabled"`
	Schedule   scheduleXO        `json:"schedule"`
	Properties map[string]string `json:"properties"`
}

// scheduleBody and taskBody are the body of a create or save. A key the
// body lacks is empty; id is read and not used.
type scheduleBody struct {
	Type      string   `json:"type"`
	StartAt   *string  `json:"startAt"`
	WeekDays  []string `json:"weekDays"`
	MonthDays []int    `json:"monthDays"`
	Cron      string   `json:"cron"`
}

type taskBody struct {
	ID         string          `json:"id"`
	TypeID     string          `json:"typeId"`
	Name       string          `json:"name"`
	Enabled    json.RawMessage `json:"enabled"`
	Schedule   scheduleBody    `json:"schedule"`
	Properties map[string]any  `json:"properties"`
}

// TaskTypes is GET /api/task-types: the built types of tasks.TypeList.
func TaskTypes() (Answer, error) {
	list := tasks.TypeList()
	if list == nil {
		list = []tasks.TaskType{}
	}
	return ok(list)
}

// TaskSettings is GET /api/tasks/{id}: the stored settings, or 404. A
// property whose key holds a secret word is left out, though no record
// holds one (A11).
func TaskSettings(env *Env, id string) (Answer, error) {
	var task *model.Task
	if env.Snapshot != nil {
		if at := slices.IndexFunc(env.Snapshot.Tasks, func(t model.Task) bool { return t.ID == id }); at >= 0 {
			task = &env.Snapshot.Tasks[at]
		}
	}
	if task == nil {
		return refused(admin.TaskRefusal(id, tasks.ErrUnknownTask))
	}
	x := taskSettingsXO{ID: task.ID, TypeID: task.TypeID, Name: task.Name, Enabled: task.Enabled, Properties: map[string]string{},
		Schedule: scheduleXO{Type: task.Schedule.Type, WeekDays: task.Schedule.WeekDays, MonthDays: task.Schedule.MonthDays, Cron: task.Schedule.Cron}}
	if !task.Schedule.StartAt.IsZero() {
		x.Schedule.StartAt = task.Schedule.StartAt.UTC().Format(time.RFC3339)
	}
	for key, value := range task.Properties {
		if !admin.SecretKey(key) {
			x.Properties[key] = value
		}
	}
	return ok(x)
}

// CreateTask is POST /api/tasks: 201 with {id}, a new id of 16 lowercase
// hex characters. The task is of origin local.
func CreateTask(w IdentityWrite, r *tasks.Registry, body []byte) (Answer, error) {
	b, schedule, err := decodeTask(body)
	if err != nil {
		return refused(err)
	}
	var id string
	err = r.Create(func() (model.Task, error) {
		var created model.Task
		_, err := admin.Change(w.Store, taskKinds, func(s *model.Snapshot) error {
			settings := b.settings(schedule)
			if err := admin.CheckTask(s, tasks.TypeList(), settings, nil); err != nil {
				return err
			}
			if id, err = newTaskID(s); err != nil {
				return err
			}
			created = b.task(id, schedule, admin.TaskEnabled(settings, nil))
			s.Tasks = append(slices.Clone(s.Tasks), created)
			return nil
		})
		return created, err
	})
	if err != nil {
		return refused(admin.TaskRefusal(id, err))
	}
	return Answer{Status: http.StatusCreated, Body: map[string]string{"id": id}}, nil
}

// SaveTask is PUT /api/tasks/{id}: 204 after the save. The type cannot
// change. The saved task is of origin local: an imported task saved enabled
// leaves the paused state (I12). A running task keeps its run; the saved
// settings apply from its next run.
func SaveTask(w IdentityWrite, r *tasks.Registry, id string, body []byte) (Answer, error) {
	b, schedule, err := decodeTask(body)
	if err != nil {
		return refused(err)
	}
	err = r.Save(id, func() (model.Task, error) {
		var saved model.Task
		_, err := admin.Change(w.Store, taskKinds, func(s *model.Snapshot) error {
			at := slices.IndexFunc(s.Tasks, func(t model.Task) bool { return t.ID == id })
			if at < 0 {
				return fmt.Errorf("task %q: %w", id, tasks.ErrUnknownTask)
			}
			stored := s.Tasks[at]
			held := storedSettings(stored)
			settings := b.settings(schedule)
			if err := admin.CheckTask(s, tasks.TypeList(), settings, &held); err != nil {
				return err
			}
			if !slices.ContainsFunc(tasks.TypeList(), func(t tasks.TaskType) bool { return t.ID == stored.TypeID }) {
				// no type checks the properties of this task
				if err := plainProperties(b.Properties); err != nil {
					return err
				}
			}
			saved = b.task(id, schedule, admin.TaskEnabled(settings, &held))
			s.Tasks = slices.Clone(s.Tasks)
			s.Tasks[at] = saved
			return nil
		})
		return saved, err
	})
	if err != nil {
		return refused(admin.TaskRefusal(id, err))
	}
	return noContent()
}

// DeleteTask is DELETE /api/tasks/{id}: 204 after the delete, 409 while the
// task runs. The runs and the secret files of the task stay.
func DeleteTask(w IdentityWrite, r *tasks.Registry, id string) (Answer, error) {
	err := r.Remove(id, func() error {
		_, err := admin.Change(w.Store, taskKinds, func(s *model.Snapshot) error {
			n := len(s.Tasks)
			s.Tasks = slices.DeleteFunc(slices.Clone(s.Tasks), func(t model.Task) bool { return t.ID == id })
			if len(s.Tasks) == n {
				return fmt.Errorf("task %q: %w", id, tasks.ErrUnknownTask)
			}
			return nil
		})
		return err
	})
	if errors.Is(err, tasks.ErrRunning) {
		return refused(&admin.Refusal{Code: admin.CodeTaskRunning, Message: fmt.Sprintf(
			"the task %s is running; stop it or wait for the end of its run before the delete", id)})
	}
	if err != nil {
		return refused(admin.TaskRefusal(id, err))
	}
	return noContent()
}

// decodeTask reads the body of a create or save and its schedule. A
// startAt that is not a time of RFC 3339 is a 400 at schedule.startAt.
func decodeTask(body []byte) (taskBody, tasks.Schedule, error) {
	var b taskBody
	if err := decode(body, '{', &b); err != nil {
		return taskBody{}, tasks.Schedule{}, err
	}
	s := tasks.Schedule{Type: b.Schedule.Type, WeekDays: b.Schedule.WeekDays, MonthDays: b.Schedule.MonthDays, Cron: b.Schedule.Cron}
	if b.Schedule.StartAt != nil && *b.Schedule.StartAt != "" {
		at, err := time.Parse(time.RFC3339, *b.Schedule.StartAt)
		if err != nil {
			return taskBody{}, tasks.Schedule{}, admin.Refusals{{Code: admin.CodeScheduleInvalid, Field: "schedule." + tasks.FieldStartAt,
				Message: "startAt needs a time in the form 2026-11-12T03:00:00Z"}}
		}
		s.StartAt = at.UTC().Truncate(time.Second)
	}
	return b, s, nil
}

func (b taskBody) settings(schedule tasks.Schedule) admin.TaskSettings {
	return admin.TaskSettings{TypeID: b.TypeID, Name: b.Name, Enabled: b.Enabled, Schedule: schedule, Properties: b.Properties}
}

// task is the stored form of a body that passed its checks.
func (b taskBody) task(id string, s tasks.Schedule, enabled bool) model.Task {
	t := model.Task{ID: id, Name: strings.TrimSpace(b.Name), TypeID: b.TypeID, Enabled: enabled, Origin: model.OriginLocal,
		Schedule:   model.TaskSchedule{Type: s.Type, StartAt: s.StartAt, WeekDays: slices.Clone(s.WeekDays), MonthDays: slices.Clone(s.MonthDays), Cron: s.Cron},
		Properties: map[string]string{}}
	for key, value := range b.Properties {
		if text, ok := value.(string); ok {
			t.Properties[key] = text
		}
	}
	return t
}

// storedSettings are the settings of a stored task as CheckTask compares
// them.
func storedSettings(t model.Task) admin.TaskSettings {
	enabled, _ := json.Marshal(t.Enabled)
	properties := map[string]any{}
	for key, value := range t.Properties {
		properties[key] = value
	}
	return admin.TaskSettings{TypeID: t.TypeID, Name: t.Name, Enabled: enabled, Schedule: tasks.ScheduleOf(t.Schedule), Properties: properties}
}

// plainProperties checks the properties of a task of a type this server
// does not check: each value is a string, and no key names a secret (A11).
func plainProperties(properties map[string]any) error {
	var list admin.Refusals
	for _, key := range slices.Sorted(maps.Keys(properties)) {
		if _, text := properties[key].(string); !text {
			list = append(list, &admin.Refusal{Code: admin.CodePropertyValue, Field: "properties." + key, Message: fmt.Sprintf(
				"the property %s needs its value as a string", key)})
			continue
		}
		if admin.SecretKey(key) {
			list = append(list, &admin.Refusal{Code: admin.CodePropertySecret, Field: "properties." + key, Message: fmt.Sprintf(
				"the key %s names a secret; a task of this server holds no secret in its properties", key)})
		}
	}
	if len(list) == 0 {
		return nil
	}
	return list
}

// newTaskID is an id of 16 lowercase hex characters that no task of s has.
func newTaskID(s *model.Snapshot) (string, error) {
	for range 8 {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		id := hex.EncodeToString(raw[:])
		if !slices.ContainsFunc(s.Tasks, func(t model.Task) bool { return t.ID == id }) {
			return id, nil
		}
	}
	return "", errors.New("no free task id")
}
