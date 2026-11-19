package release

import (
	"errors"
	"strings"
	"testing"
)

func TestSchemaAnswerPasses(t *testing.T) {
	for name, c := range map[string]struct {
		answer string
		want   Schema
	}{
		"two keys":           {`{"catalog":2,"state":1}` + "\n", Schema{Catalog: 2, State: 1}},
		"other order":        {`{"state":1,"catalog":2}` + "\n", Schema{Catalog: 2, State: 1}},
		"third key":          {`{"catalog":2,"state":1,"overview":3}` + "\n", Schema{Catalog: 2, State: 1}},
		"third key first":    {`{"overview":3,"catalog":2,"state":1}` + "\n", Schema{Catalog: 2, State: 1}},
		"object value":       {`{"catalog":4,"state":3,"files":{"a":[1,"x",null]}}` + "\n", Schema{Catalog: 4, State: 3}},
		"spaces":             {`{ "catalog": 2, "state": 1 }` + "\n", Schema{Catalog: 2, State: 1}},
		"nine digits":        {`{"catalog":999999999,"state":1}` + "\n", Schema{Catalog: 999999999, State: 1}},
		"further key string": {`{"catalog":2,"state":1,"note":"catalog 9"}` + "\n", Schema{Catalog: 2, State: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseSchema(c.answer)
			if err != nil || got != c.want {
				t.Fatalf("parseSchema(%q) = %+v, %v; want %+v", c.answer, got, err, c.want)
			}
		})
	}
}

func TestSchemaAnswerRefusals(t *testing.T) {
	for name, answer := range map[string]string{
		"nothing":            "",
		"newline only":       "\n",
		"no newline":         `{"catalog":2,"state":1}`,
		"two lines":          `{"catalog":2,"state":1}` + "\n\n",
		"line before":        "\n" + `{"catalog":2,"state":1}` + "\n",
		"carriage return":    `{"catalog":2,"state":1}` + "\r\n",
		"array":              `[2,1]` + "\n",
		"null":               "null\n",
		"number":             "2\n",
		"missing state":      `{"catalog":2}` + "\n",
		"missing catalog":    `{"state":1,"overview":3}` + "\n",
		"other case":         `{"Catalog":2,"state":1}` + "\n",
		"zero":               `{"catalog":0,"state":1}` + "\n",
		"negative":           `{"catalog":2,"state":-1}` + "\n",
		"fraction":           `{"catalog":2.0,"state":1}` + "\n",
		"exponent":           `{"catalog":2e0,"state":1}` + "\n",
		"string":             `{"catalog":"2","state":1}` + "\n",
		"null value":         `{"catalog":null,"state":1}` + "\n",
		"leading zero":       `{"catalog":02,"state":1}` + "\n",
		"ten digits":         `{"catalog":1000000000,"state":1}` + "\n",
		"catalog twice":      `{"catalog":2,"state":1,"catalog":3}` + "\n",
		"state twice":        `{"state":1,"catalog":2,"state":1}` + "\n",
		"further key twice":  `{"catalog":2,"state":1,"x":1,"x":1}` + "\n",
		"text after":         `{"catalog":2,"state":1} x` + "\n",
		"space after":        `{"catalog":2,"state":1} ` + "\n",
		"second object":      `{"catalog":2,"state":1}{"catalog":2,"state":1}` + "\n",
		"space before":       ` {"catalog":2,"state":1}` + "\n",
		"not closed":         `{"catalog":2,"state":1` + "\n",
		"bad further value":  `{"catalog":2,"state":1,"x":nope}` + "\n",
		"trailing comma":     `{"catalog":2,"state":1,}` + "\n",
		"newline inside":     "{\"catalog\":2,\n\"state\":1}\n",
		"key without value":  `{"catalog":2,"state"}` + "\n",
		"non string key":     `{2:2,"catalog":2,"state":1}` + "\n",
		"further key nested": `{"catalog":2,"state":1,"x":{"y":[1,2}}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseSchema(answer)
			if !errors.Is(err, ErrSchema) {
				t.Fatalf("parseSchema(%q) = %+v, %v; want ErrSchema", answer, got, err)
			}
		})
	}
}

// A release that names one more file in its answer gets the plan of the
// two key answer.
func TestSchemaPlanWithAThirdKey(t *testing.T) {
	plan := func(answer string) string {
		r := newRig(t, "0.1.0")
		r.dataFiles(1, 1)
		if err := r.u.Update(r.release(answering("0.2.0", answer)), false); err != nil {
			t.Fatalf("%s: %v\n%s", answer, err, r.out.String())
		}
		for _, line := range strings.Split(r.out.String(), "\n") {
			if strings.HasPrefix(line, "schema: ") {
				return line
			}
		}
		t.Fatalf("%s: no schema line in\n%s", answer, r.out.String())
		return ""
	}
	two := plan(`{"catalog":2,"state":1}`)
	three := plan(`{"catalog":2,"state":1,"overview":5}`)
	if two != "schema: state.sqlite 1 stays, catalog.sqlite 1 -> 2 migrates" || three != two {
		t.Fatalf("two keys %q, three keys %q", two, three)
	}
}
