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

// Codes of the rules of the tasks.
const (
	CodeScheduleInvalid Code = "schedule-invalid"  // a schedule of a task that tasks.Schedule.Trigger refuses (Q2)
	CodeTaskName        Code = "task-name"         // a task name that is empty or longer than 256 characters (Q3)
	CodeTaskType        Code = "task-type"         // a task type this server does not have (Q3)
	CodeTaskTypeChanged Code = "task-type-changed" // a save names another task type than the stored one (Q3)
	CodeTaskEnabled     Code = "task-enabled"      // a value of enabled that is no JSON boolean (Q17)
	CodePropertyUnknown Code = "property-unknown"  // a property that the task type does not define (Q4)
	CodePropertyMissing Code = "property-missing"  // a required property that is absent or empty (Q4)
	CodePropertyValue   Code = "property-value"    // a property value that is no string or does not fit its field (Q4)
)

// Codes of the rules of the users.
const (
	CodeUserDirectory Code = "user-directory" // a delete or an own account change of a user of another source than default (Q5, Q6)
	CodeUserOwn       Code = "user-own"       // a delete of the own user (Q5)
	CodeUserAnonymous Code = "user-anonymous" // a delete of the user that the anonymous access setting names (Q5)
	CodeUserStatus    Code = "user-status"    // a user status other than active, locked, disabled and changepassword (Q7)
)

// Codes of the identity changes (leg 6-identity).
const (
	CodeBodyInvalid      Code = "body-invalid"      // a request body that is no JSON of the shape of its request
	CodePasswordInvalid  Code = "password-invalid"  // a password that is empty, longer than 1024 bytes or holds a control character (I5)
	CodePrivilegeMissing Code = "privilege-missing" // a privilege of a role that does not exist (S22)
	CodeRoleInUse        Code = "role-in-use"       // a delete of a role that a user, a mapping, a role, the anonymous user or the default role holds (I4)
	CodeRealmInvalid     Code = "realm-invalid"     // a realm list that is empty, holds an unknown or repeated realm, or DefaultRole without a role (N16); a realm of a user delete that names no source (I12)
	CodeAnonymousSignIn  Code = "anonymous-sign-in" // anonymous access that would act as a user that signs in (R9, I10)
	CodeGrantExceeds     Code = "grant-exceeds"     // a change that gives a role or a user a permission the acting subject does not hold (I9)
)

// Codes of the LDAP connections (leg 6-identity, contract S26 and S27).
const (
	CodeLDAPField Code = "ldap-field" // a field of an LDAP connection form that is missing or holds a value the connection cannot take
	CodeLDAPOrder Code = "ldap-order" // a change of order that does not name every LDAP connection once
)

// Codes of the repository and store changes (leg 6-stores).
const (
	CodeRepositoryKind    Code = "repository-kind"    // a format or type outside the twelve kinds of A5
	CodeRemoteInvalid     Code = "remote-invalid"     // a proxy without a remote address of http or https with a host and no user part (ST8)
	CodeRemoteSecret      Code = "remote-secret"      // an upstream authentication this server cannot store or use (ST9)
	CodeWriteStore        Code = "write-store"        // a repository that would write into another store than local-write (ST2)
	CodeStoreUnknown      Code = "store-unknown"      // a repository that names no store or a store that does not exist
	CodeDockerPort        Code = "docker-port"        // a Docker port in a create or save, which the host sets (ST7)
	CodeMemberInUse       Code = "member-in-use"      // a delete of a repository that a group names as a member (ST5)
	CodePathInvalid       Code = "path-invalid"       // a store path outside <data>/stores/, holding files, or another store's (ST1)
	CodeStoreProtected    Code = "store-protected"    // a delete of local-write or of a store that config.json attaches (ST2)
	CodeRepositoryDefined Code = "repository-defined" // a delete of a repository that repositories.json of the host defines
)

// Codes of the routing rules, the capabilities and the read only mode
// (leg 6-system).
const (
	CodeRuleBound      Code = "rule-bound"      // a routing rule body, matcher list, matcher or test path above its bound (SY2)
	CodeRuleMode       Code = "rule-mode"       // a routing rule mode other than ALLOW and BLOCK
	CodeMatcherInvalid Code = "matcher-invalid" // a matcher that does not compile, or a rule without a matcher
	CodePreviewScope   Code = "preview-scope"   // a routing preview scope other than all, group and proxy
	CodeCapabilityType Code = "capability-type" // a capability type kinds.json does not know, or a save that changes the type (SY6)
	CodePropertySecret Code = "property-secret" // a capability property whose key the secret word rule marks (SY5, A11)
	CodeReadOnlyState  Code = "read-only-state" // a freeze, release or task run the state of the read only mode does not allow (SB2)
)

// Codes of the rules of a cleanup preview and of the format of a policy.
const (
	CodePreviewRepository Code = "preview-repository" // the repository of a preview is missing, a group or of another format (Q8)
	CodeFormatUnknown     Code = "format-unknown"     // a cleanup policy format other than *, maven2, npm, docker and raw (Q15)
)

// Codes that later legs return. TaskRefusal returns not-found, unavailable
// and the three task codes for the refusals of the scheduler (Q1); no other
// rule of this package returns them.
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
	CodeScheduleInvalid:   {http.StatusBadRequest, formFields, ""},
	CodeTaskName:          {http.StatusBadRequest, formFields, ""},
	CodeTaskType:          {http.StatusBadRequest, formFields, ""},
	CodeTaskTypeChanged:   {http.StatusBadRequest, formFields, ""},
	CodeTaskEnabled:       {http.StatusBadRequest, formFields, ""},
	CodePropertyUnknown:   {http.StatusBadRequest, formFields, ""},
	CodePropertyMissing:   {http.StatusBadRequest, formFields, ""},
	CodePropertyValue:     {http.StatusBadRequest, formFields, ""},
	CodeUserDirectory:     {http.StatusConflict, formMessage, ""},
	CodeUserOwn:           {http.StatusConflict, formMessage, ""},
	CodeUserAnonymous:     {http.StatusConflict, formMessage, ""},
	CodeUserStatus:        {http.StatusBadRequest, formFields, ""},
	CodeBodyInvalid:       {http.StatusBadRequest, formFields, ""},
	CodePasswordInvalid:   {http.StatusBadRequest, formFields, ""},
	CodePrivilegeMissing:  {http.StatusBadRequest, formFields, ""},
	CodeRoleInUse:         {http.StatusConflict, formMessage, ""},
	CodeRealmInvalid:      {http.StatusBadRequest, formFields, ""},
	CodeAnonymousSignIn:   {http.StatusConflict, formMessage, ""},
	CodeGrantExceeds:      {http.StatusForbidden, formMessage, ""},
	CodeLDAPField:         {http.StatusBadRequest, formFields, ""},
	CodeLDAPOrder:         {http.StatusBadRequest, formFields, ""},
	CodePreviewRepository: {http.StatusBadRequest, formFields, ""},
	CodeFormatUnknown:     {http.StatusBadRequest, formFields, ""},
	CodeRepositoryKind:    {http.StatusNotImplemented, formMessageArea, ""},
	CodeRemoteInvalid:     {http.StatusBadRequest, formFields, ""},
	CodeRemoteSecret:      {http.StatusBadRequest, formFields, ""},
	CodeWriteStore:        {http.StatusBadRequest, formFields, ""},
	CodeStoreUnknown:      {http.StatusBadRequest, formFields, ""},
	CodeDockerPort:        {http.StatusBadRequest, formFields, ""},
	CodeMemberInUse:       {http.StatusConflict, formMessage, ""},
	CodePathInvalid:       {http.StatusBadRequest, formFields, ""},
	CodeStoreProtected:    {http.StatusConflict, formMessage, ""},
	CodeRepositoryDefined: {http.StatusConflict, formMessage, ""},
	CodeRuleBound:         {http.StatusBadRequest, formFields, ""},
	CodeRuleMode:          {http.StatusBadRequest, formFields, ""},
	CodeMatcherInvalid:    {http.StatusBadRequest, formFields, ""},
	CodePreviewScope:      {http.StatusBadRequest, formFields, ""},
	CodeCapabilityType:    {http.StatusBadRequest, formFields, ""},
	CodePropertySecret:    {http.StatusBadRequest, formFields, ""},
	CodeReadOnlyState:     {http.StatusConflict, formMessage, ""},
	CodeNotFound:          {http.StatusNotFound, formMessage, ""},
	CodeForbidden:         {http.StatusForbidden, formMessage, ""},
	CodeRequestToken:      {http.StatusForbidden, formMessage, "missing or wrong request token"},
	CodeNotBuilt:          {http.StatusNotImplemented, formMessageArea, ""},
	CodeUnavailable:       {http.StatusServiceUnavailable, formMessage, ""},
	CodeTaskNotRunning:    {http.StatusConflict, formMessage, ""},
	CodeTaskRunning:       {http.StatusConflict, formMessage, ""},
	CodeTaskDisabled:      {http.StatusMethodNotAllowed, formMessage, ""},
}
