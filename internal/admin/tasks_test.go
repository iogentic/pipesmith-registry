package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// scheduleBody is a schedule in the shape of the contract (S40).
type scheduleBody struct {
	Type      string   `json:"type"`
	StartAt   string   `json:"startAt"`
	WeekDays  []string `json:"weekDays"`
	MonthDays []int    `json:"monthDays"`
	Cron      string   `json:"cron"`
}

// schedule is the body as a tasks.Schedule; an empty startAt is none.
func (b scheduleBody) schedule(t *testing.T) tasks.Schedule {
	t.Helper()
	s := tasks.Schedule{Type: b.Type, WeekDays: b.WeekDays, MonthDays: b.MonthDays, Cron: b.Cron}
	if b.StartAt != "" {
		at, err := time.Parse(time.RFC3339, b.StartAt)
		if err != nil {
			t.Fatal(err)
		}
		s.StartAt = at
	}
	return s
}

// settingsBody is the body of a task create or save in the shape of the
// contract (S40).
type settingsBody struct {
	TypeID     string          `json:"typeId"`
	Name       string          `json:"name"`
	Enabled    json.RawMessage `json:"enabled"`
	Schedule   scheduleBody    `json:"schedule"`
	Properties map[string]any  `json:"properties"`
}

// settings is the body as TaskSettings, its name repeated repeat times when
// repeat is set.
func (b settingsBody) settings(t *testing.T, repeat int) TaskSettings {
	t.Helper()
	name := b.Name
	if repeat > 0 {
		name = strings.Repeat(name, repeat)
	}
	return TaskSettings{TypeID: b.TypeID, Name: name, Enabled: b.Enabled, Schedule: b.Schedule.schedule(t),
		Properties: b.Properties}
}

type taskCase struct {
	Row              string         `json:"row"`
	Op               string         `json:"op"` // error, schedule, settings, properties
	Error            string         `json:"error"`
	Wrap             string         `json:"wrap"`
	Task             string         `json:"task"`
	Passes           bool           `json:"passes"`
	Schedule         *scheduleBody  `json:"schedule"`
	Body             *settingsBody  `json:"body"`
	Stored           *settingsBody  `json:"stored"`
	NameRepeat       int            `json:"nameRepeat"`
	Type             string         `json:"type"`
	Properties       map[string]any `json:"properties"`
	StoredProperties map[string]any `json:"storedProperties"`
	Refusals         *[]wantRefusal `json:"refusals"`
	EnabledAfter     *bool          `json:"enabledAfter"`
}

// taskErrors are the errors a case of op error names.
var taskErrors = map[string]error{
	"none":         nil,
	"unknown-task": tasks.ErrUnknownTask,
	"duplicate":    tasks.ErrDuplicate,
	"running":      tasks.ErrRunning,
	"not-running":  tasks.ErrNotRunning,
	"disabled":     tasks.ErrDisabled,
	"not-started":  tasks.ErrNotStarted,
	"started":      tasks.ErrStarted,
	"closed":       tasks.ErrClosed,
	"other":        errors.New("disk full"),
}

// wrapIn wraps err as the key wrap of a case says; nil stays nil.
func wrapIn(t *testing.T, err error, wrap, id string) error {
	t.Helper()
	if err == nil {
		return nil
	}
	switch wrap {
	case "":
		return err
	case "fmt":
		return fmt.Errorf("task %q: %w", id, err)
	case "twice":
		return fmt.Errorf("run: %w", fmt.Errorf("task %q: %w", id, err))
	case "join":
		return errors.Join(errors.New("the store is slow"), err)
	}
	t.Fatalf("unknown wrap %q", wrap)
	return nil
}

func TestTaskVectors(t *testing.T) {
	var file struct {
		Family       string             `json:"family"`
		Note         string             `json:"note"`
		Types        []tasks.TaskType   `json:"types"`
		Repositories []model.Repository `json:"repositories"`
		BlobStores   []model.BlobStore  `json:"blobStores"`
		Cases        []taskCase         `json:"cases"`
	}
	readJSON(t, "tasks", &file)
	s := &model.Snapshot{Repositories: file.Repositories, BlobStores: file.BlobStores}
	counts := tally{}
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s %q %q %q)", i, c.Row, c.Op, c.Error, c.Wrap, c.Type)
		switch c.Op {
		case "error":
			err, known := taskErrors[c.Error]
			if !known {
				t.Fatalf("%s: unknown error", label)
			}
			err = wrapIn(t, err, c.Wrap, c.Task)
			got := TaskRefusal(c.Task, err)
			if c.Passes {
				if c.Refusals != nil || err == nil {
					t.Fatalf("%s: a case that passes names an error and no refusals", label)
				}
				if got != err {
					t.Errorf("%s: %v did not pass unchanged, got %v", label, err, got)
				}
				if status, body := Answer(got); status != 500 {
					t.Errorf("%s: Answer gives %d %v, want 500", label, status, body)
				}
				counts.add(c.Row, false)
				continue
			}
			checkRefusals(t, label, got, c.Refusals)
		case "schedule":
			if c.Schedule == nil {
				t.Fatalf("%s: no schedule", label)
			}
			_, err := c.Schedule.schedule(t).Trigger()
			checkRefusals(t, label, TaskRefusal(c.Task, wrapIn(t, err, c.Wrap, c.Task)), c.Refusals)
		case "settings":
			if c.Body == nil {
				t.Fatalf("%s: no body", label)
			}
			var stored *TaskSettings
			if c.Stored != nil {
				held := c.Stored.settings(t, 0)
				stored = &held
			}
			body := c.Body.settings(t, c.NameRepeat)
			checkRefusals(t, label, CheckTask(s, file.Types, body, stored), c.Refusals)
			if c.EnabledAfter != nil {
				if c.Refusals == nil || len(*c.Refusals) > 0 {
					t.Fatalf("%s: a case with enabledAfter is accepted", label)
				}
				if got := TaskEnabled(body, stored); got != *c.EnabledAfter {
					t.Errorf("%s: TaskEnabled gives %v, want %v", label, got, *c.EnabledAfter)
				}
			}
		case "properties":
			i := slices.IndexFunc(file.Types, func(tt tasks.TaskType) bool { return tt.ID == c.Type })
			if i < 0 {
				t.Fatalf("%s: the file has no type %q", label, c.Type)
			}
			checkRefusals(t, label, CheckTaskProperties(s, file.Types[i], c.Properties, c.StoredProperties), c.Refusals)
		default:
			t.Fatalf("%s: unknown op", label)
		}
		counts.add(c.Row, len(*c.Refusals) > 0)
	}
	counts.check(t, "tasks", "Q1", "Q2", "Q3", "Q4", "Q4a", "Q4b", "Q4c", "Q4d", "Q4e", "Q16", "Q17")
}

// runLog is a store of runs for the scheduler of a test.
type runLog struct {
	mu   sync.Mutex
	runs int64
}

func (l *runLog) BeginRun(string, time.Time) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs++
	return l.runs, nil
}

func (l *runLog) EndRun(int64, time.Time, string, string) error { return nil }

func (l *runLog) EndOpenRuns(time.Time, string, string) (int64, error) { return 0, nil }

// The errors of a scheduler, wrapped as its methods wrap them, give the
// codes of ruling Q1 and Q2; its refusals of a second Add and a second
// Start pass unchanged.
func TestTaskRefusalOfTheScheduler(t *testing.T) {
	s := tasks.New(tasks.NewFakeClock(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)), &runLog{})
	wait := func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	manual := tasks.Schedule{Type: tasks.TypeManual}
	for _, task := range []tasks.Task{
		{ID: "demo-task", Schedule: manual, Enabled: true, Run: wait},
		{ID: "demo-off", Schedule: manual, Run: wait},
	} {
		if err := s.Add(task); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(label, id string, err error, code Code, field string, status int) {
		t.Helper()
		got := TaskRefusal(id, err)
		var r *Refusal
		if !errors.As(got, &r) || r.Code != code || r.Field != field || r.Message == "" {
			t.Errorf("%s: %v gives %v, want %q at %q", label, err, got, code, field)
			return
		}
		if answered, _ := Answer(got); answered != status {
			t.Errorf("%s: Answer gives %d, want %d", label, answered, status)
		}
	}
	passed := func(label string, err error) {
		t.Helper()
		if err == nil || TaskRefusal("demo-task", err) != err {
			t.Errorf("%s: %v did not pass unchanged", label, err)
		}
	}
	refused("run before start", "demo-task", s.Run("demo-task"), CodeUnavailable, "", 503)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	passed("second start", s.Start())
	passed("second add", s.Add(tasks.Task{ID: "demo-task", Schedule: manual, Enabled: true, Run: wait}))
	refused("run of an unknown task", "demo-gone", s.Run("demo-gone"), CodeNotFound, "", 404)
	refused("stop of an unknown task", "demo-gone", s.Stop("demo-gone"), CodeNotFound, "", 404)
	refused("run of a disabled task", "demo-off", s.Run("demo-off"), CodeTaskDisabled, "", 405)
	refused("stop of a task that is not running", "demo-task", s.Stop("demo-task"), CodeTaskNotRunning, "", 409)
	if err := s.Run("demo-task"); err != nil {
		t.Fatal(err)
	}
	refused("run of a running task", "demo-task", s.Run("demo-task"), CodeTaskRunning, "", 409)
	weekly := tasks.Schedule{Type: tasks.TypeWeekly, StartAt: time.Date(2026, 10, 4, 2, 0, 0, 0, time.UTC),
		WeekDays: []string{"FUNDAY"}}
	refused("add with a schedule the scheduler refuses", "demo-weekly",
		s.Add(tasks.Task{ID: "demo-weekly", Schedule: weekly, Enabled: true, Run: wait}),
		Code("schedule-invalid"), "schedule.weekDays", 400)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	refused("run after close", "demo-task", s.Run("demo-task"), CodeUnavailable, "", 503)
	refused("add after close", "demo-new", s.Add(tasks.Task{ID: "demo-new", Schedule: manual, Run: wait}),
		CodeUnavailable, "", 503)
}

// A refusal of a schedule carries the Message of its FieldError, without
// the field in front that Error adds; the field travels as the id.
func TestScheduleRefusalMessage(t *testing.T) {
	start := time.Date(2026, 10, 4, 2, 0, 0, 0, time.UTC)
	for _, s := range []tasks.Schedule{
		{Type: "yearly"},
		{Type: tasks.TypeDaily},
		{Type: tasks.TypeWeekly, StartAt: start},
		{Type: tasks.TypeMonthly, StartAt: start, MonthDays: []int{32}},
		{Type: tasks.TypeCron, Cron: "0 0 12 * * *"},
	} {
		_, err := s.Trigger()
		var field *tasks.FieldError
		if !errors.As(err, &field) {
			t.Fatalf("%+v: %v is no FieldError", s, err)
		}
		for _, wrapped := range []error{err, fmt.Errorf("task %q: %w", "demo-task", err)} {
			var r *Refusal
			if !errors.As(TaskRefusal("demo-task", wrapped), &r) {
				t.Fatalf("%+v: no refusal for %v", s, wrapped)
			}
			if r.Message != field.Message || r.Field != "schedule."+field.Field {
				t.Errorf("%+v: %q at %q, want %q at %q", s, r.Message, r.Field, field.Message, "schedule."+field.Field)
			}
			if strings.HasPrefix(r.Message, field.Field+":") {
				t.Errorf("%+v: the message %q starts with the field", s, r.Message)
			}
		}
	}
}

// The run of a task the registry keeps disabled answers 405 with the reason
// of the registry (ruling SB1), also when the error wraps it.
func TestTaskRefusalOfADisabledTaskGivesItsReason(t *testing.T) {
	reason := "the task type blobstore.compact is not built"
	err := fmt.Errorf("run: %w", &tasks.DisabledError{ID: "demo-cron", Reason: reason})
	status, body := Answer(TaskRefusal("demo-cron", err))
	if status != 405 || body != (Message{reason}) {
		t.Fatalf("%d %+v", status, body)
	}
	// the refusal of the scheduler itself keeps its message
	status, body = Answer(TaskRefusal("demo-cron", fmt.Errorf("task: %w", tasks.ErrDisabled)))
	if status != 405 || body != (Message{"the task demo-cron is disabled"}) {
		t.Fatalf("%d %+v", status, body)
	}
}

// Act runs an action that changes no record and ends in the hook of every
// change (ruling SB3); a refused action ends in no hook.
func TestActEndsInTheHookOfAChange(t *testing.T) {
	store := &fakeStore{}
	if err := Act(store, func() error { return nil }); err != nil || store.reset != 1 || store.stale != 1 {
		t.Fatalf("%v reset %d stale %d", err, store.reset, store.stale)
	}
	refusal := errors.New("refused")
	if err := Act(store, func() error { return refusal }); err != refusal || store.reset != 1 || store.stale != 1 {
		t.Fatalf("%v reset %d stale %d", err, store.reset, store.stale)
	}
	ran := false
	if err := Act(nil, func() error { ran = true; return nil }); err == nil || ran {
		t.Fatalf("an action without a store: %v, ran %v", err, ran)
	}
}

// Ruling KN16: a property that the kind of a built type refuses at load is
// refused at save with the same reason, and one it takes passes.
func TestSaveRefusesWhatTheLoadRefuses(t *testing.T) {
	types := map[string]tasks.TaskType{}
	for _, tt := range tasks.TypeList() {
		types[tt.ID] = tt
	}
	s := &model.Snapshot{Repositories: []model.Repository{{Name: "demo-releases", Format: "maven2", Type: "hosted"}},
		BlobStores: []model.BlobStore{{Name: "local-write"}}}
	kinds := tasks.Kinds(&tasks.HostRef{})
	for _, c := range []struct {
		typeID, field string
		properties    map[string]any
	}{
		{tasks.TypeUploadPurge, "olderThanHours", map[string]any{"repositoryName": "*", "olderThanHours": "0"}},
		{tasks.TypeUploadPurge, "olderThanHours", map[string]any{"repositoryName": "*", "olderThanHours": "87601"}},
		{tasks.TypeWorkFiles, "olderThanHours", map[string]any{"blobStoreName": "local-write", "olderThanHours": "0"}},
		{tasks.TypeMavenMetadata, "repositoryName", map[string]any{"repositoryName": "*"}},
		{tasks.TypeMavenMetadata, "groupId", map[string]any{"repositoryName": "demo-releases", "artifactId": "demo-lib"}},
		{tasks.TypeBackup, "location", map[string]any{"location": "relative/demo"}},
	} {
		err := CheckTaskProperties(s, types[c.typeID], c.properties, nil)
		texts := map[string]string{}
		for k, v := range c.properties {
			texts[k] = v.(string)
		}
		_, loadErr := kinds[c.typeID](model.Task{TypeID: c.typeID, Properties: texts})
		var refusal *Refusal
		if !errors.As(err, &refusal) || refusal.Field != "properties."+c.field || loadErr == nil || refusal.Message != loadErr.Error() {
			t.Errorf("%s %v: save %v, load %v", c.typeID, c.properties, err, loadErr)
		}
	}
	for typeID, properties := range map[string]map[string]any{
		tasks.TypeUploadPurge:   {"repositoryName": "*", "olderThanHours": "87600"},
		tasks.TypeMavenMetadata: {"repositoryName": "demo-releases", "groupId": "com.example", "artifactId": "demo-lib"},
		tasks.TypeBackup:        {"location": "/srv/demo-backup"},
	} {
		if err := CheckTaskProperties(s, types[typeID], properties, nil); err != nil {
			t.Errorf("%s %v: %v", typeID, properties, err)
		}
	}
}
