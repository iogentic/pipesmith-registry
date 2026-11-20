package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// schedulerCodes are the codes of the refusals of the scheduler (ruling
// Q1), in the order TaskRefusal looks for them.
var schedulerCodes = []struct {
	refusal tasks.Refusal
	code    Code
}{
	{tasks.ErrUnknownTask, CodeNotFound},
	{tasks.ErrDisabled, CodeTaskDisabled},
	{tasks.ErrRunning, CodeTaskRunning},
	{tasks.ErrNotRunning, CodeTaskNotRunning},
	{tasks.ErrNotStarted, CodeUnavailable},
	{tasks.ErrClosed, CodeUnavailable},
}

// TaskRefusal turns an error of internal/tasks about the task with the id
// into a refusal. It finds a refusal of the scheduler with errors.Is, also
// when err wraps it (ruling Q1): an unknown task is not-found (404), a
// disabled task task-disabled (405), with the reason of a
// *tasks.DisabledError when err holds one, a running task task-running (409), a
// task that is not running task-not-running (409), and a scheduler that has
// not started or is closed unavailable (503). A *tasks.FieldError of a
// schedule is a 400 at schedule.<Field> with its message (ruling Q2). nil
// gives nil, and every other error passes unchanged, so that Answer gives
// 500 for it.
func TaskRefusal(id string, err error) error {
	if err == nil {
		return nil
	}
	// the registry gives the reason a task cannot run (ruling SB1)
	var disabled *tasks.DisabledError
	if errors.As(err, &disabled) {
		return &Refusal{Code: CodeTaskDisabled, Message: disabled.Reason}
	}
	if errors.Is(err, tasks.ErrPaused) {
		return &Refusal{Code: CodeReadOnlyState, Message: tasks.ErrPaused.Error()}
	}
	for _, s := range schedulerCodes {
		if errors.Is(err, s.refusal) {
			return &Refusal{Code: s.code, Message: taskMessage(id, s.refusal)}
		}
	}
	var field *tasks.FieldError
	if errors.As(err, &field) {
		return scheduleRefusal(field)
	}
	return err
}

// taskMessage is the message of a refusal of the scheduler. The messages of
// the scheduler itself name no task.
func taskMessage(id string, r tasks.Refusal) string {
	switch r {
	case tasks.ErrUnknownTask:
		return fmt.Sprintf("no task has the id %s", id)
	case tasks.ErrDisabled:
		return fmt.Sprintf("the task %s is disabled", id)
	case tasks.ErrRunning:
		return fmt.Sprintf("the task %s is running already", id)
	case tasks.ErrNotRunning:
		return fmt.Sprintf("the task %s is not running", id)
	}
	return string(r)
}

// scheduleRefusal is the refusal of a schedule that tasks.Schedule.Trigger
// refuses: a 400 at schedule.<Field> with the message of the error.
func scheduleRefusal(e *tasks.FieldError) *Refusal {
	return &Refusal{Code: CodeScheduleInvalid, Field: "schedule." + e.Field, Message: e.Message}
}

// TaskSettings is what the checks of a task read from the body of its
// create or save (contract S40, TaskSettings): the type, the name, enabled,
// the schedule and the properties. Enabled is the JSON value of the key
// enabled as the body holds it, nil when the body lacks the key. The id is
// not checked.
type TaskSettings struct {
	TypeID     string
	Name       string
	Enabled    json.RawMessage
	Schedule   tasks.Schedule
	Properties map[string]any
}

// maxTaskName is the most characters, Unicode code points, of the name of a
// task.
const maxTaskName = 256

// numberPattern is a whole number in decimal digits, with a minus sign
// before a negative one.
var numberPattern = regexp.MustCompile(`^-?[0-9]+$`)

// CheckTask checks the create or save of a task against the task types of
// this server (ruling Q3). typeId names one of types, else 400 at typeId;
// on a save, a typeId other than the stored one is refused with 400 at
// typeId, because the type cannot change. The name is not empty after
// trimming white space and has, trimmed, at most 256 characters, else 400
// at name. enabled, when the body holds it, is a JSON boolean, else 400 at
// enabled (ruling Q17). tasks.Schedule.Trigger checks the schedule, and its
// refusal is a 400 at schedule.<Field> (ruling Q2). CheckTaskProperties checks the properties
// against the type of typeId when types holds it and typeId is not refused
// (ruling Q4). stored is the stored task on a save and nil on a create. A
// save never refuses the typeId, the name or the schedule the stored task
// holds (ruling R6). The refusals travel together, in the order of the
// fields of the contract: typeId, name, enabled, schedule, properties.
func CheckTask(s *model.Snapshot, types []tasks.TaskType, body TaskSettings, stored *TaskSettings) error {
	var list Refusals
	i := slices.IndexFunc(types, func(t tasks.TaskType) bool { return t.ID == body.TypeID })
	switch {
	case stored != nil && body.TypeID != stored.TypeID:
		list = append(list, &Refusal{Code: CodeTaskTypeChanged, Field: "typeId", Message: fmt.Sprintf(
			"the type of a task cannot change; this task has the type %s", stored.TypeID)})
		i = -1
	case i < 0 && stored == nil:
		list = append(list, &Refusal{Code: CodeTaskType, Field: "typeId", Message: fmt.Sprintf(
			"this server has no task type %q", body.TypeID)})
	}
	if stored == nil || body.Name != stored.Name {
		if r := checkTaskName(body.Name); r != nil {
			list = append(list, r)
		}
	}
	if _, ok := enabledValue(body.Enabled); body.Enabled != nil && !ok {
		list = append(list, &Refusal{Code: CodeTaskEnabled, Field: "enabled", Message: fmt.Sprintf(
			"enabled needs true or false, not %s", body.Enabled)})
	}
	if stored == nil || !sameSchedule(body.Schedule, stored.Schedule) {
		if _, err := body.Schedule.Trigger(); err != nil {
			var field *tasks.FieldError
			if !errors.As(err, &field) {
				return err
			}
			list = append(list, scheduleRefusal(field))
		}
	}
	if i >= 0 {
		var held map[string]any
		if stored != nil {
			held = stored.Properties
		}
		list = append(list, taskProperties(s, types[i], body.Properties, held)...)
	}
	return list.err()
}

// TaskEnabled is the value of enabled that a create or save of a task that
// CheckTask accepts stores (ruling Q17): the value of the body when it holds
// one; else the stored value on a save, and true on a create, where stored
// is nil.
func TaskEnabled(body TaskSettings, stored *TaskSettings) bool {
	if on, ok := enabledValue(body.Enabled); ok {
		return on
	}
	if stored != nil {
		if on, ok := enabledValue(stored.Enabled); ok {
			return on
		}
	}
	return true
}

// enabledValue reads a JSON value that is true or false. ok is false for
// any other value, a string, null and nothing among them.
func enabledValue(raw json.RawMessage) (on, ok bool) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false, false
	}
	on, ok = v.(bool)
	return on, ok
}

// checkTaskName refuses a task name that is empty after trimming white
// space or, after trimming, longer than 256 characters, with 400 at name
// (ruling Q3).
func checkTaskName(name string) *Refusal {
	trimmed := strings.TrimSpace(name)
	n := utf8.RuneCountInString(trimmed)
	switch {
	case trimmed == "":
		return &Refusal{Code: CodeTaskName, Field: "name", Message: "a task needs a name that is not empty"}
	case n > maxTaskName:
		return &Refusal{Code: CodeTaskName, Field: "name", Message: fmt.Sprintf(
			"the name of a task has %d characters; use at most %d", n, maxTaskName)}
	}
	return nil
}

// sameSchedule reports whether two schedules hold the same type, instant
// and fields.
func sameSchedule(a, b tasks.Schedule) bool {
	return a.Type == b.Type && a.StartAt.Equal(b.StartAt) && slices.Equal(a.WeekDays, b.WeekDays) &&
		slices.Equal(a.MonthDays, b.MonthDays) && a.Cron == b.Cron
}

// CheckTaskProperties checks the properties of a task body, a decoded
// object, against its type (ruling Q4). Every value is a string, as the
// contract sends them; a required field is present and not empty; a value
// that is not empty fits the type of its field: a number is a whole number
// in decimal digits that an int64 holds, a boolean true or false, a select
// one of the values of its options, or any value when it has none (ruling
// Q16), a repository * or a repository of the
// snapshot, a blobstore a blob store of the snapshot. An empty value of a
// field that is not required passes. A property the type does not define,
// and a value of a field of another type, are refused. Each refusal is a
// 400 at properties.<id>, in the order of the fields of the type and then
// of the other ids, sorted. stored holds the properties of the stored task
// on a save and is nil on a create: a property whose value equals the
// stored one is never refused (ruling R6).
func CheckTaskProperties(s *model.Snapshot, t tasks.TaskType, properties, stored map[string]any) error {
	return taskProperties(s, t, properties, stored).err()
}

func taskProperties(s *model.Snapshot, t tasks.TaskType, properties, stored map[string]any) Refusals {
	var list Refusals
	refuse := func(code Code, id, message string) {
		list = append(list, &Refusal{Code: code, Field: "properties." + id, Message: message})
	}
	kept := func(id string) bool {
		value, sent := properties[id]
		held, ok := stored[id]
		return sent && ok && sameValue(value, held)
	}
	defined := map[string]bool{}
	for _, f := range t.Fields {
		defined[f.ID] = true
		value, sent := properties[f.ID]
		text, isText := value.(string)
		switch {
		case kept(f.ID):
		case f.Required && (!sent || isText && text == ""):
			refuse(CodePropertyMissing, f.ID, fmt.Sprintf("the property %s is required", f.ID))
		case !sent:
		case !isText:
			refuse(CodePropertyValue, f.ID, fmt.Sprintf("the property %s needs its value as a string", f.ID))
		case text == "":
		default:
			if reason := unfit(s, f, text); reason != "" {
				refuse(CodePropertyValue, f.ID, reason)
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(properties)) {
		if !defined[id] && !kept(id) {
			refuse(CodePropertyUnknown, id, fmt.Sprintf("the task type %s has no property %s", t.ID, id))
		}
	}
	if len(list) == 0 {
		// ruling KN16: what the kind of a built type refuses at load is
		// refused here with the same reason
		texts := map[string]string{}
		for id, value := range properties {
			if text, ok := value.(string); ok {
				texts[id] = text
			}
		}
		var refused *tasks.PropertyError
		if err := tasks.CheckProperties(t.ID, texts); errors.As(err, &refused) && !kept(refused.Field) {
			refuse(CodePropertyValue, refused.Field, refused.Reason)
		}
	}
	return list
}

// unfit says why a value that is not empty does not fit the type of its
// field, and is empty when it fits.
func unfit(s *model.Snapshot, f tasks.TaskField, value string) string {
	switch f.Type {
	case tasks.PropertyString:
		return ""
	case tasks.PropertyNumber:
		if _, err := strconv.ParseInt(value, 10, 64); err == nil && numberPattern.MatchString(value) {
			return ""
		}
		return fmt.Sprintf("the property %s needs a whole number in decimal digits, not %q", f.ID, value)
	case tasks.PropertyBoolean:
		if value == "true" || value == "false" {
			return ""
		}
		return fmt.Sprintf("the property %s needs true or false, not %q", f.ID, value)
	case tasks.PropertySelect:
		// A select without options takes any value (ruling Q16).
		if len(f.Options) == 0 {
			return ""
		}
		values := make([]string, len(f.Options))
		for i, o := range f.Options {
			values[i] = o.Value
		}
		if slices.Contains(values, value) {
			return ""
		}
		return fmt.Sprintf("the property %s needs one of %s, not %q", f.ID, strings.Join(values, ", "), value)
	case tasks.PropertyRepository:
		// * stands for every repository.
		if value == "*" || Exists(s, KindRepository, value) {
			return ""
		}
		return fmt.Sprintf("the repository %s of the property %s does not exist", value, f.ID)
	case tasks.PropertyBlobStore:
		if Exists(s, KindBlobStore, value) {
			return ""
		}
		return fmt.Sprintf("the blob store %s of the property %s does not exist", value, f.ID)
	}
	return fmt.Sprintf("the property %s has the field type %s, which this server cannot check", f.ID, f.Type)
}
