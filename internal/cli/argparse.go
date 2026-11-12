package cli

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mirosso25/pipesmith-registry/internal/pystr"
)

// This file ports the parts of argparse of Python 3.12.13 that the parser
// of cli.py uses: options with one value or none, positionals with one
// value or "?", sub commands, a group of options that exclude each other,
// abbreviations of long options and the help of each command. The messages
// and their order are those of argparse; the help texts of the commands are
// the ones argparse prints at 80 columns (helptexts.go).

// usageError is UsageError: wrong arguments, exit code 2.
type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

// helpExit is the SystemExit of a command's -h: its help is printed and the
// exit code is 0.
type helpExit struct{ text string }

func (e *helpExit) Error() string { return "help" }

// argError is ArgumentError: the message with the name of the argument.
type argError struct {
	action  *action
	message string
}

func (e *argError) Error() string {
	if name := actionName(e.action); name != "" {
		return "argument " + name + ": " + e.message
	}
	return e.message
}

// typeError is ArgumentTypeError of a type function.
type typeError struct{ message string }

func (e *typeError) Error() string { return e.message }

const unrecognizedKey = "\x00unrecognized"

type action struct {
	dest     string // "" for the help action, which stores nothing
	options  []string
	nargs    string // "" one value, "?" zero or one, "0" none, "parser" a sub command
	required bool
	choices  []string
	convert  func(string) (any, error) // nil keeps the text; an error other than *typeError is a ValueError
	typeName string
	metavar  string
	help     bool
	flag     bool // store_true
	commands map[string]*parser
}

type parser struct {
	prog        string
	actions     []*action
	options     map[string]*action
	optionOrder []string
	conflicts   map[*action][]*action
	defaults    map[string]any
	helpText    string
}

func newParser(prog string, help bool, helpText string) *parser {
	p := &parser{prog: prog, options: map[string]*action{}, conflicts: map[*action][]*action{}, defaults: map[string]any{},
		helpText: helpText}
	if help {
		p.add(&action{options: []string{"-h", "--help"}, nargs: "0", help: true})
	}
	return p
}

func (p *parser) add(a *action) *action {
	if len(a.options) == 0 && a.nargs == "" {
		a.required = true // a positional with one value, as argparse makes it
	}
	p.actions = append(p.actions, a)
	for _, o := range a.options {
		p.options[o] = a
		p.optionOrder = append(p.optionOrder, o)
	}
	return a
}

func (p *parser) exclusive(group ...*action) {
	for i, a := range group {
		p.conflicts[a] = append(append(p.conflicts[a], group[:i]...), group[i+1:]...)
	}
}

// actionName is _get_action_name.
func actionName(a *action) string {
	switch {
	case a == nil:
		return ""
	case len(a.options) > 0:
		return strings.Join(a.options, "/")
	case a.metavar != "":
		return a.metavar
	case a.dest != "":
		return a.dest
	case len(a.choices) > 0:
		return "{" + strings.Join(a.choices, ",") + "}"
	}
	return ""
}

func (p *parser) fail(message string) error {
	return &usageError{p.prog + ": " + message}
}

// parseArgs is parse_args: unrecognized arguments are an error of p.
func (p *parser) parseArgs(args []string) (map[string]any, error) {
	ns, extras, err := p.parseKnownArgs(args)
	if err != nil {
		return nil, err
	}
	if len(extras) > 0 {
		return nil, p.fail("unrecognized arguments: " + strings.Join(extras, " "))
	}
	return ns, nil
}

func (p *parser) parseKnownArgs(args []string) (map[string]any, []string, error) {
	ns := map[string]any{}
	for _, a := range p.actions {
		if a.dest != "" {
			if _, ok := ns[a.dest]; !ok {
				ns[a.dest] = defaultOf(a)
			}
		}
	}
	for key, value := range p.defaults {
		if _, ok := ns[key]; !ok {
			ns[key] = value
		}
	}
	extras, err := p.parse(args, ns)
	if err != nil {
		if ae, ok := err.(*argError); ok {
			return nil, nil, p.fail(ae.Error())
		}
		return nil, nil, err
	}
	if more, ok := ns[unrecognizedKey].([]string); ok {
		extras = append(extras, more...)
		delete(ns, unrecognizedKey)
	}
	return ns, extras, nil
}

func defaultOf(a *action) any {
	if a.flag {
		return false
	}
	return nil
}

// optionTuple is (action, option_string, sep, explicit_arg); sep and
// explicit are nil for None.
type optionTuple struct {
	action       *action
	optionString string
	sep          *string
	explicit     *string
}

func str(s string) *string { return &s }

// firstChar splits off the first character as Python counts characters.
func firstChar(s string) (string, string) {
	_, width := utf8.DecodeRuneInString(s)
	return s[:width], s[width:]
}

// negativeNumber is _negative_number_matcher, '^-\d+$|^-\d*\.\d+$', with
// "$" also before a final newline and \d any decimal digit.
func negativeNumber(s string) bool {
	core := strings.TrimSuffix(s, "\n")
	if !strings.HasPrefix(core, "-") {
		return false
	}
	core = core[1:]
	decimal := func(t string) bool {
		if t == "" {
			return false
		}
		for _, r := range t {
			if pystr.DecimalValue(r) < 0 {
				return false
			}
		}
		return true
	}
	if decimal(core) {
		return true
	}
	whole, fraction, dot := strings.Cut(core, ".")
	return dot && (whole == "" || decimal(whole)) && decimal(fraction)
}

// parseOptional is _parse_optional: nil for a positional.
func (p *parser) parseOptional(arg string) []optionTuple {
	if arg == "" || arg[0] != '-' {
		return nil
	}
	if a, ok := p.options[arg]; ok {
		return []optionTuple{{action: a, optionString: arg}}
	}
	if utf8.RuneCountInString(arg) == 1 {
		return nil
	}
	if option, explicit, sep := strings.Cut(arg, "="); sep {
		if a, ok := p.options[option]; ok {
			return []optionTuple{{action: a, optionString: option, sep: str("="), explicit: str(explicit)}}
		}
	}
	if tuples := p.optionTuples(arg); len(tuples) > 0 {
		return tuples
	}
	if negativeNumber(arg) {
		return nil // this parser has no option that looks like a negative number
	}
	if strings.Contains(arg, " ") {
		return nil
	}
	return []optionTuple{{optionString: arg}}
}

// optionTuples is _get_option_tuples, abbreviations allowed.
func (p *parser) optionTuples(arg string) []optionTuple {
	var out []optionTuple
	prefix, explicit, found := strings.Cut(arg, "=")
	var sep, exp *string
	if found {
		sep, exp = str("="), str(explicit)
	}
	if strings.HasPrefix(arg, "--") {
		for _, o := range p.optionOrder {
			if strings.HasPrefix(o, prefix) {
				out = append(out, optionTuple{p.options[o], o, sep, exp})
			}
		}
		return out
	}
	second, rest := firstChar(arg[1:])
	short := "-" + second
	for _, o := range p.optionOrder {
		if o == short {
			out = append(out, optionTuple{p.options[o], o, str(""), str(rest)})
		} else if strings.HasPrefix(o, prefix) {
			out = append(out, optionTuple{p.options[o], o, sep, exp})
		}
	}
	return out
}

// nargsPattern is _get_nargs_pattern.
func nargsPattern(a *action) string {
	option := len(a.options) > 0
	switch a.nargs {
	case "?":
		if option {
			return "(A?)"
		}
		return "(-*A?-*)"
	case "0":
		if option {
			return "([AO]{0})"
		}
		return "((?:-*A){0}-*)"
	case "parser":
		if option {
			return "(A[AO]*)"
		}
		return "(-*A[-AO]*)"
	}
	if option {
		return "([A])"
	}
	return "(-*A-*)"
}

var patternCache = map[string]*regexp.Regexp{}

func compiled(pattern string) *regexp.Regexp {
	if re, ok := patternCache[pattern]; ok {
		return re
	}
	re := regexp.MustCompile("^" + pattern)
	patternCache[pattern] = re
	return re
}

// matchArgument is _match_argument.
func matchArgument(a *action, pattern string) (int, error) {
	m := compiled(nargsPattern(a)).FindStringSubmatchIndex(pattern)
	if m == nil {
		message := "expected one argument"
		if a.nargs == "?" {
			message = "expected at most one argument"
		}
		return 0, &argError{a, message}
	}
	return m[3] - m[2], nil
}

// matchArgumentsPartial is _match_arguments_partial.
func matchArgumentsPartial(actions []*action, pattern string) []int {
	for i := len(actions); i > 0; i-- {
		var b strings.Builder
		for _, a := range actions[:i] {
			b.WriteString(nargsPattern(a))
		}
		m := compiled(b.String()).FindStringSubmatchIndex(pattern)
		if m == nil {
			continue
		}
		result := make([]int, 0, i)
		for g := 1; g <= i; g++ {
			result = append(result, m[2*g+1]-m[2*g])
		}
		if m[1] < len(pattern) && pattern[m[1]] == 'O' {
			for len(result) > 0 && result[len(result)-1] == 0 {
				result = result[:len(result)-1]
			}
		}
		return result
	}
	return nil
}

func (p *parser) getValue(a *action, text string) (any, error) {
	if a.convert == nil {
		return text, nil
	}
	value, err := a.convert(text)
	if err != nil {
		if te, ok := err.(*typeError); ok {
			return nil, &argError{a, te.message}
		}
		return nil, &argError{a, fmt.Sprintf("invalid %s value: %s", a.typeName, pystr.Repr(text))}
	}
	return value, nil
}

func checkValue(a *action, value string) error {
	if a.choices != nil && !slices.Contains(a.choices, value) {
		return &argError{a, fmt.Sprintf("invalid choice: %s (choose from %s)", pystr.Repr(value), strings.Join(a.choices, ", "))}
	}
	return nil
}

// getValues is _get_values for the kinds of nargs of this parser.
func (p *parser) getValues(a *action, args []string) (any, error) {
	switch {
	case len(args) == 0 && a.nargs == "?":
		return nil, nil // the default of a positional
	case len(args) == 1 && (a.nargs == "" || a.nargs == "?"):
		value, err := p.getValue(a, args[0])
		if err != nil {
			return nil, err
		}
		if s, ok := value.(string); ok {
			if err := checkValue(a, s); err != nil {
				return nil, err
			}
		}
		return value, nil
	case a.nargs == "parser":
		if err := checkValue(a, args[0]); err != nil {
			return nil, err
		}
		return args, nil
	}
	return []string{}, nil
}

// call performs the action on the namespace.
func (p *parser) call(a *action, ns map[string]any, value any) error {
	switch {
	case a.help:
		return &helpExit{p.helpText}
	case a.flag:
		ns[a.dest] = true
	case a.nargs == "parser":
		values := value.([]string)
		ns[a.dest] = values[0]
		sub, ok := a.commands[values[0]]
		if !ok {
			return &argError{a, fmt.Sprintf("unknown parser %s (choices: %s)", pystr.Repr(values[0]), strings.Join(a.choices, ", "))}
		}
		subNS, extras, err := sub.parseKnownArgs(values[1:])
		if err != nil {
			return err
		}
		for key, v := range subNS {
			ns[key] = v
		}
		if len(extras) > 0 {
			more, _ := ns[unrecognizedKey].([]string)
			ns[unrecognizedKey] = append(more, extras...)
		}
	default:
		ns[a.dest] = value
	}
	return nil
}

// parse is _parse_known_args: it fills ns and returns the extras.
func (p *parser) parse(args []string, ns map[string]any) ([]string, error) {
	indices := map[int][]optionTuple{}
	pattern := make([]byte, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			pattern = append(pattern, '-')
			for range args[i+1:] {
				pattern = append(pattern, 'A')
			}
			break
		}
		if tuples := p.parseOptional(args[i]); tuples != nil {
			indices[i] = tuples
			pattern = append(pattern, 'O')
		} else {
			pattern = append(pattern, 'A')
		}
	}
	patternText := string(pattern)
	seen := map[*action]bool{}
	seenNonDefault := map[*action]bool{}
	takeAction := func(a *action, argStrings []string) error {
		seen[a] = true
		value, err := p.getValues(a, argStrings)
		if err != nil {
			return err
		}
		if len(a.options) > 0 || len(argStrings) > 0 {
			seenNonDefault[a] = true
			for _, c := range p.conflicts[a] {
				if seenNonDefault[c] {
					return &argError{a, "not allowed with argument " + actionName(c)}
				}
			}
		}
		return p.call(a, ns, value)
	}
	var extras []string
	type taken struct {
		a    *action
		args []string
	}
	consumeOptional := func(start int) (int, error) {
		tuples := indices[start]
		if len(tuples) > 1 {
			names := make([]string, len(tuples))
			for i, t := range tuples {
				names[i] = t.optionString
			}
			return 0, &argError{nil, fmt.Sprintf("ambiguous option: %s could match %s", args[start], strings.Join(names, ", "))}
		}
		t := tuples[0]
		a, optionString, sep, explicit := t.action, t.optionString, t.sep, t.explicit
		var actions []taken
		stop := start + 1
		for {
			if a == nil {
				extras = append(extras, args[start])
				return start + 1, nil
			}
			if explicit != nil {
				count, err := matchArgument(a, "A")
				if err != nil {
					return 0, err
				}
				_, afterDash := firstChar(optionString)
				second, _ := firstChar(afterDash)
				switch {
				case count == 0 && second != "-" && *explicit != "":
					if sep != nil && *sep != "" || strings.HasPrefix(*explicit, "-") {
						return 0, &argError{a, "ignored explicit argument " + pystr.Repr(*explicit)}
					}
					actions = append(actions, taken{a, nil})
					head, rest := firstChar(*explicit)
					optionString = optionString[:1] + head
					if next, ok := p.options[optionString]; ok {
						a = next
						switch {
						case rest == "":
							sep, explicit = nil, nil
						case rest[0] == '=':
							sep, explicit = str("="), str(rest[1:])
						default:
							sep, explicit = str(""), str(rest)
						}
						continue
					}
					extras = append(extras, optionString[:1]+*explicit)
				case count == 1:
					actions = append(actions, taken{a, []string{*explicit}})
				default:
					return 0, &argError{a, "ignored explicit argument " + pystr.Repr(*explicit)}
				}
				break
			}
			count, err := matchArgument(a, patternText[start+1:])
			if err != nil {
				return 0, err
			}
			stop = start + 1 + count
			actions = append(actions, taken{a, args[start+1 : stop]})
			break
		}
		for _, t := range actions {
			if err := takeAction(t.a, t.args); err != nil {
				return 0, err
			}
		}
		return stop, nil
	}
	var positionals []*action
	for _, a := range p.actions {
		if len(a.options) == 0 {
			positionals = append(positionals, a)
		}
	}
	consumePositionals := func(start int) (int, error) {
		counts := matchArgumentsPartial(positionals, patternText[start:])
		for i, count := range counts {
			a := positionals[i]
			taken := append([]string(nil), args[start:start+count]...)
			if a.nargs == "parser" {
				if patternText[start] == '-' {
					taken = taken[1:]
				}
			} else if strings.Contains(patternText[start:start+count], "-") {
				if j := slices.Index(taken, "--"); j >= 0 {
					taken = slices.Delete(taken, j, j+1)
				}
			}
			start += count
			if err := takeAction(a, taken); err != nil {
				return 0, err
			}
		}
		positionals = positionals[len(counts):]
		return start, nil
	}
	maxOption := -1
	for i := range indices {
		maxOption = max(maxOption, i)
	}
	start := 0
	for start <= maxOption {
		next := len(args)
		for i := range indices {
			if i >= start && i < next {
				next = i
			}
		}
		if start != next {
			end, err := consumePositionals(start)
			if err != nil {
				return nil, err
			}
			if end > start {
				start = end
				continue
			}
			start = end
		}
		if _, ok := indices[start]; !ok {
			extras = append(extras, args[start:next]...)
			start = next
		}
		var err error
		if start, err = consumeOptional(start); err != nil {
			return nil, err
		}
	}
	stop, err := consumePositionals(start)
	if err != nil {
		return nil, err
	}
	extras = append(extras, args[stop:]...)
	var missing []string
	for _, a := range p.actions {
		if !seen[a] && a.required {
			missing = append(missing, actionName(a))
		}
	}
	if len(missing) > 0 {
		return nil, &argError{nil, "the following arguments are required: " + strings.Join(missing, ", ")}
	}
	return extras, nil
}
