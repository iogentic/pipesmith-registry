package admin

import "net/http"

// Code names one kind of refusal. Every code has a row in the table of
// answers; a test reads the constants of this type from the source of the
// package and fails for a code without a row.
type Code string

// Codes of the rules of this package.
const (
	CodeNameInvalid       Code = "name-invalid"       // a name or id outside the pattern of its kind
	CodeNameExists        Code = "name-exists"        // a create names a record that exists
	CodeNameChanged       Code = "name-changed"       // a save names another name or id than the stored one
	CodeMemberMissing     Code = "member-missing"     // a member of a group does not exist
	CodeMemberFormat      Code = "member-format"      // a member of a group has another format
	CodeMemberCycle       Code = "member-cycle"       // a group would contain itself
	CodeRoleMissing       Code = "role-missing"       // a contained role does not exist
	CodeRoleCycle         Code = "role-cycle"         // a role would contain itself
	CodeRoleReadOnly      Code = "role-read-only"     // a built in role does not change
	CodeStoreInUse        Code = "store-in-use"       // a repository uses the blob store
	CodePolicyInUse       Code = "policy-in-use"      // a repository names the cleanup policy
	CodeRuleInUse         Code = "rule-in-use"        // a repository uses the routing rule
	CodePolicyFormat      Code = "policy-format"      // a repository of another format names the cleanup policy
	CodeLastAdministrator Code = "last-administrator" // no user would hold the full permission set (A10)
	CodeStoreKind         Code = "store-kind"         // a blob store of another kind than file (A6)
	CodePathRequired      Code = "path-required"      // a blob store needs a path
	CodePathChanged       Code = "path-changed"       // the path of a blob store is fixed (A6)
	CodeQuotaType         Code = "quota-type"         // a soft quota of an unknown type
	CodeQuotaLimit        Code = "quota-limit"        // a soft quota below 1048576 bytes
	CodeCriterionUnknown  Code = "criterion-unknown"  // a cleanup criterion this server does not know (A9)
	CodeCriterionValue    Code = "criterion-value"    // a cleanup criterion that is no whole number of 1 or more
	CodeCriteriaMissing   Code = "criteria-missing"   // a cleanup policy without a criterion
	CodeNotOffered        Code = "not-offered"        // a value that would switch on a function this server does not have (A23)
)

// Codes that later legs return; no rule of this package returns them.
const (
	CodeNotFound       Code = "not-found"        // the object does not exist
	CodeForbidden      Code = "forbidden"        // the subject lacks the right
	CodeRequestToken   Code = "request-token"    // the request token is missing or wrong
	CodeNotBuilt       Code = "not-built"        // the function is outside the built areas
	CodeUnavailable    Code = "unavailable"      // the server is read only or not ready
	CodeTaskNotRunning Code = "task-not-running" // a stop of a task that is not running
	CodeTaskRunning    Code = "task-running"     // a start of a task that runs already
	CodeTaskDisabled   Code = "task-disabled"    // a start of a disabled task
)

// form is the form of an answer body.
type form string

const (
	formFields      form = "fields"       // 400: a list of objects with id and message
	formMessage     form = "message"      // an object with message
	formMessageArea form = "message-area" // 501: an object with message and area
)

// answer is a row of the table of answers. message is the message of every
// refusal of the code, where the contract fixes it.
type answer struct {
	status  int
	form    form
	message string
}

// answers maps each code to the status and the body form of its answer, as
// section Answers of the phase 6 plan words them. The 405 is the row the
// contract adds for the start of a disabled task.
var answers = map[Code]answer{
	CodeNameInvalid:       {http.StatusBadRequest, formFields, ""},
	CodeNameExists:        {http.StatusConflict, formMessage, ""},
	CodeNameChanged:       {http.StatusBadRequest, formFields, ""},
	CodeMemberMissing:     {http.StatusBadRequest, formFields, ""},
	CodeMemberFormat:      {http.StatusBadRequest, formFields, ""},
	CodeMemberCycle:       {http.StatusBadRequest, formFields, ""},
	CodeRoleMissing:       {http.StatusBadRequest, formFields, ""},
	CodeRoleCycle:         {http.StatusBadRequest, formFields, ""},
	CodeRoleReadOnly:      {http.StatusBadRequest, formFields, ""},
	CodeStoreInUse:        {http.StatusConflict, formMessage, ""},
	CodePolicyInUse:       {http.StatusConflict, formMessage, ""},
	CodeRuleInUse:         {http.StatusConflict, formMessage, ""},
	CodePolicyFormat:      {http.StatusBadRequest, formFields, ""},
	CodeLastAdministrator: {http.StatusConflict, formMessage, ""},
	CodeStoreKind:         {http.StatusNotImplemented, formMessageArea, ""},
	CodePathRequired:      {http.StatusBadRequest, formFields, ""},
	CodePathChanged:       {http.StatusBadRequest, formFields, ""},
	CodeQuotaType:         {http.StatusBadRequest, formFields, ""},
	CodeQuotaLimit:        {http.StatusBadRequest, formFields, ""},
	CodeCriterionUnknown:  {http.StatusBadRequest, formFields, ""},
	CodeCriterionValue:    {http.StatusBadRequest, formFields, ""},
	CodeCriteriaMissing:   {http.StatusBadRequest, formFields, ""},
	CodeNotOffered:        {http.StatusBadRequest, formFields, ""},
	CodeNotFound:          {http.StatusNotFound, formMessage, ""},
	CodeForbidden:         {http.StatusForbidden, formMessage, ""},
	CodeRequestToken:      {http.StatusForbidden, formMessage, "missing or wrong request token"},
	CodeNotBuilt:          {http.StatusNotImplemented, formMessageArea, ""},
	CodeUnavailable:       {http.StatusServiceUnavailable, formMessage, ""},
	CodeTaskNotRunning:    {http.StatusConflict, formMessage, ""},
	CodeTaskRunning:       {http.StatusConflict, formMessage, ""},
	CodeTaskDisabled:      {http.StatusMethodNotAllowed, formMessage, ""},
}
