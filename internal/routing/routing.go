// Package routing compiles the routing rules of the old product and answers,
// for a proxy repository and a request path, whether the request may go to
// the remote (decision 5.15 of the phase 5 plan). The rules apply to proxies
// only: the server asks for a proxy and for a proxy inside a group, never
// for a hosted repository.
//
// A matcher is compiled with the Go regexp package and must match the whole
// path with its leading slash, as String.matches of Java does in the old
// product; a path given without the slash gets it. Under BLOCK a path that
// any matcher matches is blocked, under ALLOW only such a path is allowed.
// A rule fails closed: a matcher that does not compile, one that Go reads
// otherwise than Java (a class inside a class, an intersection of classes),
// a mode other than ALLOW and BLOCK, and a name that two rules carry each
// block every path of the repositories of the rule, and so does a
// repository that names a rule that does not exist. Problems names them for
// the start log. testdata/vectors/routing.json cites the documentation and
// the source of the old product behind each of these rules.
package routing

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The modes of a rule, in the spelling of the old product.
const (
	ModeAllow = "ALLOW"
	ModeBlock = "BLOCK"
)

const javaOnlyClass = "holds a class inside a class or an intersection of classes, which Go reads other than Java"

// rule is one compiled rule.
type rule struct {
	allow    bool
	matchers []*regexp.Regexp
	broken   string // why the rule blocks every path, empty for a working rule
}

// Rules are the compiled routing rules and the rule each repository names.
// They are safe for use by several goroutines. A nil *Rules allows every
// path.
type Rules struct {
	rules    map[string]*rule
	links    map[string]string
	problems []string
}

// Compile compiles the rules and keeps the links; it never fails, as a rule
// it cannot use blocks every path. Nothing of r is kept.
func Compile(r model.Routing) *Rules {
	rs := &Rules{rules: map[string]*rule{}, links: maps.Clone(r.Links)}
	count := map[string]int{}
	for _, x := range r.Rules {
		count[x.Name]++
	}
	for _, x := range r.Rules {
		if _, done := rs.rules[x.Name]; done {
			continue
		}
		c := compile(x)
		if n := count[x.Name]; n > 1 {
			c = &rule{broken: fmt.Sprintf("the name is given to %d rules", n)}
		}
		rs.rules[x.Name] = c
		if c.broken != "" {
			rs.problems = append(rs.problems, fmt.Sprintf("routing rule %q blocks every path: %s", x.Name, c.broken))
		}
	}
	for repository, name := range rs.links {
		if _, ok := rs.rules[name]; name != "" && !ok {
			rs.problems = append(rs.problems, fmt.Sprintf(
				"repository %q names the routing rule %q, which does not exist; every path of it is blocked", repository, name))
		}
	}
	slices.Sort(rs.problems)
	return rs
}

// Allowed reports whether a request for path may go to the remote of the
// repository. A repository without a rule is allowed.
func (rs *Rules) Allowed(repository, path string) bool {
	if rs == nil {
		return true
	}
	name := rs.links[repository]
	if name == "" {
		return true
	}
	r, ok := rs.rules[name]
	return ok && r.allows(path)
}

// Problems are the rules that block every path and the repositories that
// name a rule that does not exist, one sorted line each, for the start log.
func (rs *Rules) Problems() []string {
	if rs == nil {
		return nil
	}
	return slices.Clone(rs.problems)
}

// Test answers for one rule and one path, as the rule would answer for a
// repository that names it; the preview of phase 6 calls it. For a rule
// that blocks every path it answers false and says why.
func Test(r model.RoutingRule, path string) (bool, error) {
	c := compile(r)
	if c.broken != "" {
		return false, errors.New(c.broken)
	}
	return c.allows(path), nil
}

func (r *rule) allows(path string) bool {
	if r.broken != "" {
		return false
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	matches := slices.ContainsFunc(r.matchers, func(re *regexp.Regexp) bool { return re.MatchString(path) })
	return matches == r.allow
}

func compile(r model.RoutingRule) *rule {
	c := &rule{}
	switch r.Mode {
	case ModeAllow:
		c.allow = true
	case ModeBlock:
	default:
		c.broken = fmt.Sprintf("the mode %q is neither ALLOW nor BLOCK", r.Mode)
		return c
	}
	for i, m := range r.Matchers {
		re, fault := compileMatcher(m)
		if fault != "" {
			c.broken = fmt.Sprintf("matcher %d %s", i+1, fault)
			return c
		}
		c.matchers = append(c.matchers, re)
	}
	return c
}

// compileMatcher compiles one matcher, or says why it cannot be used.
func compileMatcher(m string) (*regexp.Regexp, string) {
	if javaOnly(m) {
		return nil, javaOnlyClass
	}
	re, err := whole(m)
	if err != nil {
		return nil, fmt.Sprintf("does not compile: %v", err)
	}
	return re, ""
}

// Fault is a matcher that cannot be used, by its index from 0.
type Fault struct {
	Index   int
	Message string
}

// Explanation is the answer of one rule for one path (contract S17).
// Matcher is the index from 0 of the first matcher that matched, -1 for
// none. Mode says why the mode is refused, empty for ALLOW and BLOCK.
// Faults are the matchers that cannot be used, in their order. A rule with
// a fault or a refused mode allows no path, as Test answers.
type Explanation struct {
	Allowed bool
	Matcher int
	Mode    string
	Faults  []Fault
}

// Explain answers for one rule and one path as Test does, and names the
// matcher that decided and every matcher that cannot be used. The faults
// are those of every matcher, not only the first.
func Explain(r model.RoutingRule, path string) Explanation {
	e := Explanation{Matcher: -1}
	allow := r.Mode == ModeAllow
	if !allow && r.Mode != ModeBlock {
		e.Mode = fmt.Sprintf("the mode %q is neither ALLOW nor BLOCK", r.Mode)
	}
	matchers := make([]*regexp.Regexp, len(r.Matchers))
	for i, m := range r.Matchers {
		re, fault := compileMatcher(m)
		if fault != "" {
			e.Faults = append(e.Faults, Fault{Index: i, Message: "the matcher " + fault})
		}
		matchers[i] = re
	}
	if e.Mode != "" || len(e.Faults) > 0 {
		return e
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	for i, re := range matchers {
		if re.MatchString(path) {
			e.Matcher = i
			break
		}
	}
	e.Allowed = (e.Matcher >= 0) == allow
	return e
}

// whole compiles a matcher that must match the whole text: the parsed
// expression between the start and the end of the text. The tree, not the
// text, gets the anchors, so no text of the matcher (an open \Q, a
// stray bracket) can reach past them.
func whole(m string) (*regexp.Regexp, error) {
	parsed, err := syntax.Parse(m, syntax.Perl)
	if err != nil {
		return nil, err
	}
	anchored := &syntax.Regexp{Op: syntax.OpConcat, Sub: []*syntax.Regexp{{Op: syntax.OpBeginText}, parsed,
		{Op: syntax.OpEndText}}}
	return regexp.Compile(anchored.String())
}

// javaOnly reports a class inside a class or && inside a class. Java reads
// them as a union and an intersection of classes, Go as plain characters of
// one class, so the same text would match other paths. Escapes and the
// quoted text of \Q to \E are no class.
func javaOnly(m string) bool {
	inClass := false
	for i := 0; i < len(m); i++ {
		switch c := m[i]; {
		case c == '\\' && i+1 < len(m) && m[i+1] == 'Q':
			end := strings.Index(m[i+2:], `\E`)
			if end < 0 {
				return false
			}
			i += 2 + end + 1
		case c == '\\':
			i++
		case !inClass && c == '[':
			inClass = true
			// a ] right after [ or [^ is a character of the class
			j := i + 1
			if j < len(m) && m[j] == '^' {
				j++
			}
			if j < len(m) && m[j] == ']' {
				i = j
			}
		case inClass && c == '[':
			return true
		case inClass && c == '&' && i+1 < len(m) && m[i+1] == '&':
			return true
		case inClass && c == ']':
			inClass = false
		}
	}
	return false
}
