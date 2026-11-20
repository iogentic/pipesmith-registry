package admin

import (
	"fmt"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// StoreUsers are the repositories that name the blob store in
// storage.blobStoreName or in write_store, the attribute of this server
// (ST2), sorted.
func StoreUsers(s *model.Snapshot, store string) []string {
	var names []string
	for _, r := range s.Repositories {
		storage, _ := r.Attributes["storage"].(map[string]any)
		name, _ := storage["blobStoreName"].(string)
		if target, _ := r.Attributes["write_store"].(string); name == store || target == store {
			names = append(names, r.Name)
		}
	}
	slices.Sort(names)
	return names
}

// PolicyUsers are the repositories that name the cleanup policy, sorted.
func PolicyUsers(s *model.Snapshot, policy string) []string {
	var names []string
	for _, r := range s.Repositories {
		if slices.Contains(policyNames(r), policy) {
			names = append(names, r.Name)
		}
	}
	slices.Sort(names)
	return names
}

// policyNames are the cleanup policies a repository names, read as
// internal/rest reads them: cleanup.policyNames, else the stored
// cleanup.policyName, as a list or as one name.
func policyNames(r model.Repository) []string {
	cleanup, _ := r.Attributes["cleanup"].(map[string]any)
	v, found := cleanup["policyNames"]
	if !found {
		v = cleanup["policyName"]
	}
	if name, ok := v.(string); ok {
		if name == "" {
			return nil
		}
		return []string{name}
	}
	return stringList(v)
}

// inUse is the 409 of a record that repositories use; the message names
// every repository, sorted.
func inUse(code Code, kind Kind, name string, users []string) error {
	users = slices.Compact(slices.Sorted(slices.Values(users)))
	if len(users) == 0 {
		return nil
	}
	count := "1 repository"
	if len(users) > 1 {
		count = fmt.Sprintf("%d repositories", len(users))
	}
	return &Refusal{Code: code, Message: fmt.Sprintf("the %s %s is in use by %s: %s",
		kind.noun(), name, count, strings.Join(users, ", "))}
}

// CheckStoreDelete refuses the delete of a blob store that a repository
// names in storage.blobStoreName (409).
func CheckStoreDelete(s *model.Snapshot, store string) error {
	return inUse(CodeStoreInUse, KindBlobStore, store, StoreUsers(s, store))
}

// CheckPolicyDelete refuses the delete of a cleanup policy that a
// repository names (409).
func CheckPolicyDelete(s *model.Snapshot, policy string) error {
	return inUse(CodePolicyInUse, KindCleanupPolicy, policy, PolicyUsers(s, policy))
}

// CheckRuleDelete refuses the delete of a routing rule in use (409).
// usedBy names the repositories that use the rule, because the snapshot
// holds no stored form of the rule of a repository yet.
func CheckRuleDelete(rule string, usedBy []string) error {
	return inUse(CodeRuleInUse, KindRoutingRule, rule, usedBy)
}

// AnyFormat is the format of a cleanup policy that applies to every
// format.
const AnyFormat = "*"

// PolicyFormats are the formats of a cleanup policy (ruling Q15).
var PolicyFormats = []string{AnyFormat, "maven2", "npm", "docker", "raw"}

// CheckFormatValue checks the format of a create or save of a cleanup
// policy (ruling Q15): one of PolicyFormats, compared as it is, else 400 at
// format; the empty value is refused too. stored is the format of the
// stored policy on a save and nil on a create and a preview; a format the
// stored policy holds is never refused (ruling R6). A preview passes the
// refusal through PreviewPolicy, which gives it the field policy.format.
func CheckFormatValue(format string, stored *string) error {
	if slices.Contains(PolicyFormats, format) || stored != nil && format == *stored {
		return nil
	}
	return &Refusal{Code: CodeFormatUnknown, Field: "format", Message: fmt.Sprintf(
		"the format %q is not known; use one of %s", format, strings.Join(PolicyFormats, ", "))}
}

// CheckPolicyFormat checks the save of a cleanup policy that has the
// format stored: a new format other than * is refused with 400 at format
// while a repository of a different format names the policy. The message
// names those repositories, sorted. A format the stored policy has already
// is never refused.
func CheckPolicyFormat(s *model.Snapshot, policy, stored, format string) error {
	if format == stored || format == AnyFormat {
		return nil
	}
	var clash []string
	for _, r := range s.Repositories {
		if r.Format != format && slices.Contains(policyNames(r), policy) {
			clash = append(clash, r.Name+" ("+r.Format+")")
		}
	}
	if len(clash) == 0 {
		return nil
	}
	slices.Sort(clash)
	return &Refusal{Code: CodePolicyFormat, Field: "format", Message: fmt.Sprintf(
		"the cleanup policy %s cannot take the format %s, because repositories of other formats apply it: %s",
		policy, format, strings.Join(clash, ", "))}
}
