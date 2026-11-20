package admin

import (
	"fmt"
	"slices"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// CheckPreviewTarget checks the repository of a cleanup preview against the
// format of the policy (ruling Q8): the repository exists, is no group, and
// has the format of the policy unless the policy format is *. Else it is
// refused with 400 at repository, and the message names the reason. Names
// are compared as they are. A format outside PolicyFormats is refused by
// CheckFormatValue at policy.format, and that check comes first (ruling
// Q15): CheckPreviewTarget gives nil for such a format, so the preview gets
// no refusal at repository.
func CheckPreviewTarget(s *model.Snapshot, format, repository string) error {
	if !slices.Contains(PolicyFormats, format) {
		return nil
	}
	refuse := func(message string) error {
		return &Refusal{Code: CodePreviewRepository, Field: "repository", Message: message}
	}
	i := slices.IndexFunc(s.Repositories, func(r model.Repository) bool { return r.Name == repository })
	switch {
	case repository == "":
		return refuse("a preview needs the name of a repository")
	case i < 0:
		return refuse(fmt.Sprintf("the repository %s does not exist", repository))
	case s.Repositories[i].Type == "group":
		return refuse(fmt.Sprintf("the repository %s is a group and holds no assets of its own", repository))
	case format != AnyFormat && s.Repositories[i].Format != format:
		return refuse(fmt.Sprintf("the repository %s has the format %s, and the policy applies to the format %s",
			repository, s.Repositories[i].Format, format))
	}
	return nil
}

// PreviewPolicy gives the refusals of the checks of the policy of a preview,
// the criteria check and the policy format check, in the form of a preview
// (ruling Q8): copies whose fields have the prefix policy., so criteria.<name>
// becomes policy.criteria.<name> and format policy.format, as the contract
// names the ids of a preview. A refusal without a field, the request as a
// whole, gets policy.*. nil stays nil, and an error that holds anything but
// refusals passes unchanged, so that Answer gives 500 for it.
func PreviewPolicy(err error) error {
	var list []*Refusal
	if !collect(err, &list) {
		return err
	}
	out := make(Refusals, len(list))
	for i, r := range list {
		c := *r
		if c.Field == "" {
			c.Field = "*"
		}
		c.Field = "policy." + c.Field
		out[i] = &c
	}
	return out
}
