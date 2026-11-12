package maven

import (
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/pystr"
)

// element is an xml.etree.ElementTree.Element after _parse of maven.py:
// tags without namespace, attributes without the namespaced ones, in the
// order expat reports them. An empty text or tail stands for None; the two
// behave alike everywhere maven.py and ElementTree look at them.
type element struct {
	tag        string
	attrs      []attr
	text, tail string
	children   []*element
}

type attr struct{ name, value string }

func (e *element) get(name string) string {
	for _, a := range e.attrs {
		if a.name == name {
			return a.value
		}
	}
	return ""
}

// find is Element.find for a path of child tags such as
// "./versioning/snapshot": the first match in document order.
func (e *element) find(tags ...string) *element {
	if len(tags) == 0 {
		return e
	}
	for _, child := range e.children {
		if child.tag == tags[0] {
			if found := child.find(tags[1:]...); found != nil {
				return found
			}
		}
	}
	return nil
}

// findAll is Element.findall for a path of child tags.
func (e *element) findAll(tags ...string) []*element {
	if len(tags) == 0 {
		return []*element{e}
	}
	var out []*element
	for _, child := range e.children {
		if child.tag == tags[0] {
			out = append(out, child.findAll(tags[1:]...)...)
		}
	}
	return out
}

// childText is _text: the stripped text of the first match, "" for None.
func (e *element) childText(tags ...string) string {
	if e == nil {
		return ""
	}
	node := e.find(tags...)
	if node == nil {
		return ""
	}
	return pystr.Strip(node.text)
}

func (e *element) depth() int {
	deepest := 0
	stack := []struct {
		e     *element
		level int
	}{{e, 1}}
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		deepest = max(deepest, top.level)
		for _, child := range top.e.children {
			stack = append(stack, struct {
				e     *element
				level int
			}{child, top.level + 1})
		}
	}
	return deepest
}

func blank(s string) bool {
	return pystr.Strip(s) == ""
}

// indent is ET.indent(root, space="  "): new lines and indentation in the
// texts and tails that hold only white space.
func indent(root *element) {
	if len(root.children) == 0 {
		return
	}
	indentations := []string{"\n"}
	var children func(e *element, level int)
	children = func(e *element, level int) {
		childLevel := level + 1
		if childLevel == len(indentations) {
			indentations = append(indentations, indentations[level]+"  ")
		}
		childIndentation := indentations[childLevel]
		if blank(e.text) {
			e.text = childIndentation
		}
		for _, child := range e.children {
			if len(child.children) > 0 {
				children(child, childLevel)
			}
			if blank(child.tail) {
				child.tail = childIndentation
			}
		}
		if last := e.children[len(e.children)-1]; blank(last.tail) {
			last.tail = indentations[level]
		}
	}
	children(root, 0)
}

var (
	cdataEscaper  = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	attribEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "\r", "&#13;",
		"\n", "&#10;", "\t", "&#09;")
)

// write is _serialize_xml with short empty elements.
func write(b *strings.Builder, e *element) {
	b.WriteString("<" + e.tag)
	for _, a := range e.attrs {
		b.WriteString(" " + a.name + `="` + attribEscaper.Replace(a.value) + `"`)
	}
	if e.text != "" || len(e.children) > 0 {
		b.WriteString(">")
		b.WriteString(cdataEscaper.Replace(e.text))
		for _, child := range e.children {
			write(b, child)
		}
		b.WriteString("</" + e.tag + ">")
	} else {
		b.WriteString(" />")
	}
	b.WriteString(cdataEscaper.Replace(e.tail))
}

// serialize is _serialize of maven.py: the declaration, the indented
// element and a new line. Texts become UTF-8; expat gives no lone
// surrogate, so the encoding cannot fail.
func serialize(root *element) []byte {
	indent(root)
	var b strings.Builder
	b.WriteString(XMLDeclaration)
	write(&b, root)
	b.WriteString("\n")
	return []byte(b.String())
}
