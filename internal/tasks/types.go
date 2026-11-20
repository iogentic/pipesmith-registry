package tasks

// The types of the fields of a task type, the words of the contract.
const (
	PropertyString     = "string"
	PropertyNumber     = "number"
	PropertyBoolean    = "boolean"
	PropertySelect     = "select"
	PropertyRepository = "repository"
	PropertyBlobStore  = "blobstore"
)

// TaskType is a type of task as the list of task types of the contract
// shows it: its id, its name and description, and the fields of the
// properties of its tasks. This package holds no list of types.
type TaskType struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Fields      []TaskField `json:"fields"`
}

// TaskField is a field of a task type, one property of its tasks. Type is
// one of the six words above. Default is the value the form shows first,
// nil for none; Options are the choices of a select field, and a field
// without options has no key options.
type TaskField struct {
	ID       string        `json:"id"`
	Label    string        `json:"label"`
	Type     string        `json:"type"`
	Required bool          `json:"required"`
	Default  any           `json:"default"`
	Options  []FieldOption `json:"options,omitempty"`
}

// FieldOption is one choice of a select field.
type FieldOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}
