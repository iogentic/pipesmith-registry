package maven

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// A port of the parts of expat 2.6.3 that ElementTree.fromstring of Python
// 3.12 uses: a parser with namespace processing and "}" as separator, the
// internal subset of a DOCTYPE without reading anything external, internal
// entities expanded, comments and processing instructions dropped. The
// functions keep the names of the expat functions they port where that
// helps to compare them.

// errNotWellFormed is any error of expat; ElementTree raises ParseError for
// it and _parse of maven.py gives None.
var errNotWellFormed = errors.New("not well-formed")

// btype is the byte type of a character in expat's tables.
type btype uint8

const (
	btOther btype = iota
	btNonXML
	btLT
	btAmp
	btRSqb
	btCR
	btLF
	btGT
	btQuot
	btApos
	btEquals
	btQuest
	btExcl
	btSol
	btSemi
	btNum
	btLSqb
	btS
	btNmstrt
	btColon
	btHex
	btDigit
	btName
	btMinus
	btPercnt
	btLPar
	btRPar
	btAst
	btPlus
	btComma
	btVerbar
)

// asciiTypes is asciitab.h of a parser with namespaces: ':' is btColon.
var asciiTypes = func() (t [128]btype) {
	for i := range t {
		switch c := byte(i); {
		case c == '\t' || c == ' ':
			t[i] = btS
		case c == '\n':
			t[i] = btLF
		case c == '\r':
			t[i] = btCR
		case c < 0x20:
			t[i] = btNonXML
		case c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F':
			t[i] = btHex
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_':
			t[i] = btNmstrt
		case c >= '0' && c <= '9':
			t[i] = btDigit
		}
	}
	for c, b := range map[byte]btype{
		'!': btExcl, '"': btQuot, '#': btNum, '%': btPercnt, '&': btAmp, '\'': btApos, '(': btLPar, ')': btRPar,
		'*': btAst, '+': btPlus, ',': btComma, '-': btMinus, '.': btName, '/': btSol, ':': btColon, ';': btSemi,
		'<': btLT, '=': btEquals, '>': btGT, '?': btQuest, '[': btLSqb, ']': btRSqb, '|': btVerbar,
	} {
		t[c] = b
	}
	return t
}()

// classify gives the byte type of a character. Above ASCII it is btNmstrt,
// btName or btOther after the name tables of expat. In the text of an
// entity CR is white space, as expat reads that text with its internal
// encoding.
func classify(r rune, latin1Names, internal bool) btype {
	if r < 0x80 {
		if r == '\r' && internal {
			return btS
		}
		return asciiTypes[r]
	}
	if latin1Names && r <= 0xFF {
		switch {
		case inRuns(latin1NameStartRuns, r):
			return btNmstrt
		case inRuns(latin1NameRuns, r):
			return btName
		}
		return btOther
	}
	if r <= 0xFFFF {
		switch {
		case inRuns(nameStartRuns, r):
			return btNmstrt
		case inRuns(nameRuns, r):
			return btName
		}
	}
	return btOther
}

func isNameStartType(t btype) bool {
	return t == btNmstrt || t == btHex
}

func isNameType(t btype) bool {
	return t == btNmstrt || t == btHex || t == btDigit || t == btName || t == btMinus
}

func isSpaceType(t btype) bool {
	return t == btS || t == btCR || t == btLF
}

// source is text the parser reads: the document as UTF-8, or the
// replacement text of an entity.
type source struct {
	b        []byte
	pos      int
	internal bool
	latin1   bool
}

func (s *source) typeAt(i int) (btype, rune, int) {
	if c := s.b[i]; c < 0x80 {
		if c == '\r' && s.internal {
			return btS, '\r', 1
		}
		return asciiTypes[c], rune(c), 1
	}
	r, n := utf8.DecodeRune(s.b[i:])
	return classify(r, s.latin1, s.internal), r, n
}

func (s *source) has(i int) bool {
	return i < len(s.b)
}

func (s *source) hasPrefix(i int, prefix string) bool {
	return i <= len(s.b) && len(s.b)-i >= len(prefix) && string(s.b[i:i+len(prefix)]) == prefix
}

// skipName reads the characters of a name from i on, as CHECK_NAME_CASES
// does, and returns the index after them.
func (s *source) skipName(i int) int {
	for s.has(i) {
		t, _, n := s.typeAt(i)
		if !isNameType(t) {
			break
		}
		i += n
	}
	return i
}

// qname reads a name of content at i: a first character, then name
// characters with at most one colon, which a first character follows.
func (s *source) qname(i int) (int, bool) {
	if !s.has(i) {
		return 0, false
	}
	t, _, n := s.typeAt(i)
	if !isNameStartType(t) {
		return 0, false
	}
	i += n
	hadColon := false
	for {
		i = s.skipName(i)
		if !s.has(i) || s.b[i] != ':' {
			return i, true
		}
		if hadColon {
			return 0, false
		}
		hadColon = true
		i++
		if !s.has(i) {
			return 0, false
		}
		if t, _, n := s.typeAt(i); !isNameStartType(t) {
			return 0, false
		} else {
			i += n
		}
	}
}

func (s *source) skipSpace(i int) int {
	for s.has(i) {
		if t, _, _ := s.typeAt(i); !isSpaceType(t) {
			break
		}
		i++
	}
	return i
}

// The fixed namespaces and the separator of ElementTree.
const (
	xmlNamespace   = "http://www.w3.org/XML/1998/namespace"
	xmlnsNamespace = "http://www.w3.org/2000/xmlns/"
	nsSeparator    = '}'
)

type entityDecl struct {
	text     string
	internal bool   // declared with a value
	notation string // an unparsed entity
	open     bool
}

// attID is an attribute name of expat: one per name for the whole
// document.
type attID struct {
	name           string
	prefix         string // before the first colon; "" with defaultNS for "xmlns"
	prefixed       bool
	xmlns          bool
	maybeTokenized bool
}

type defaultAtt struct {
	id      *attID
	value   *string
	isCdata bool
}

type elementDecl struct {
	defaults []defaultAtt
}

type openTag struct {
	rawName string
	bound   []string // prefixes this element binds, "" for the default namespace
}

const (
	amplificationThreshold = 8 << 20
	amplificationFactor    = 100
)

type parser struct {
	dec            *decoded
	doc            *source
	standalone     bool
	hasParamRefs   bool
	keepProcessing bool
	general        map[string]*entityDecl
	params         map[string]*entityDecl
	elements       map[string]*elementDecl
	attIDs         map[string]*attID
	bindings       map[string][]string
	tags           []openTag
	indirect       uint64
	tree           builder
}

// builder is the TreeBuilder of ElementTree: character data goes to the
// text of the element started last or the tail of the element ended last.
type builder struct {
	stack []*element
	root  *element
	last  *element
	tail  bool
	data  []byte
}

func (b *builder) flush() {
	if len(b.data) == 0 || b.last == nil {
		return
	}
	if b.tail {
		b.last.tail = string(b.data)
	} else {
		b.last.text = string(b.data)
	}
	b.data = b.data[:0]
}

func (b *builder) start(e *element) {
	b.flush()
	if len(b.stack) > 0 {
		parent := b.stack[len(b.stack)-1]
		parent.children = append(parent.children, e)
	} else {
		b.root = e
	}
	b.stack = append(b.stack, e)
	b.last, b.tail = e, false
}

func (b *builder) end() {
	b.flush()
	b.last = b.stack[len(b.stack)-1]
	b.stack = b.stack[:len(b.stack)-1]
	b.tail = true
}

// parseMetadata is _parse of maven.py: the root of the document with
// namespaces taken off tags and namespaced attributes dropped, nil when it
// does not parse or the root is no metadata element.
func parseMetadata(data []byte, codecs func(string) codecResult) (*element, error) {
	dec, ok, err := decodeDocument(data, codecs)
	if err != nil || !ok {
		return nil, err
	}
	p := &parser{
		dec:            dec,
		doc:            &source{b: dec.text, latin1: dec.latin1Names},
		standalone:     dec.standalone,
		keepProcessing: true,
		general:        map[string]*entityDecl{},
		params:         map[string]*entityDecl{},
		elements:       map[string]*elementDecl{},
		attIDs:         map[string]*attID{},
		bindings:       map[string][]string{"xml": {xmlNamespace}},
	}
	if err := p.prolog(); err != nil {
		return nil, nil
	}
	if err := p.content(p.doc, false); err != nil {
		return nil, nil
	}
	if err := p.epilog(); err != nil {
		return nil, nil
	}
	root := p.tree.root
	if root == nil || root.tag != "metadata" {
		return nil, nil
	}
	return root, nil
}

// directBytes is the count of document bytes expat has read, for the
// amplification limit.
func (p *parser) directBytes() uint64 {
	return uint64(p.dec.before) + uint64(float64(p.doc.pos)*p.dec.ratio) // #nosec G115 -- before is an offset into the document, never negative.
}

// tolerated is accountingDiffTolerated: past 8 MiB of output the output
// may be at most 100 times the input.
func (p *parser) tolerated() bool {
	if p.indirect == 0 {
		return true
	}
	direct := p.directBytes()
	total := direct + p.indirect
	if total < amplificationThreshold {
		return true
	}
	factor := float32(22+p.indirect) / 22
	if direct > 0 {
		factor = float32(total) / float32(direct)
	}
	return factor <= amplificationFactor
}

func (p *parser) expanded(n int) error {
	p.indirect += uint64(n) // #nosec G115 -- n is the length of an entity text, never negative.
	if !p.tolerated() {
		return errNotWellFormed
	}
	return nil
}

func (p *parser) token() error {
	if !p.tolerated() {
		return errNotWellFormed
	}
	return nil
}

var predefined = map[string]byte{"lt": '<', "gt": '>', "amp": '&', "quot": '"', "apos": '\''}

// reference reads a reference at i, just after "&": an entity name or a
// character reference, up to and with ";". It returns the end, the name
// ("" for a character reference) and the character.
func (s *source) reference(i int) (end int, name string, char rune, ok bool) {
	if !s.has(i) {
		return 0, "", 0, false
	}
	if s.b[i] == '#' {
		i++
		hex := s.has(i) && s.b[i] == 'x'
		if hex {
			i++
		}
		start := i
		value := 0
		for ; s.has(i) && s.b[i] != ';'; i++ {
			c := s.b[i]
			switch {
			case c >= '0' && c <= '9':
				if hex {
					value = value<<4 | int(c-'0')
				} else {
					value = value*10 + int(c-'0')
				}
			case hex && c >= 'a' && c <= 'f':
				value = value<<4 | int(c-'a'+10)
			case hex && c >= 'A' && c <= 'F':
				value = value<<4 | int(c-'A'+10)
			default:
				return 0, "", 0, false
			}
			if value >= 0x110000 {
				value = 0x110000 // checkCharRefNumber refuses it; the syntax goes on
			}
		}
		if !s.has(i) || i == start {
			return 0, "", 0, false
		}
		return i + 1, "", rune(value), true
	}
	t, _, n := s.typeAt(i)
	if !isNameStartType(t) {
		return 0, "", 0, false
	}
	start := i
	i = s.skipName(i + n)
	if !s.has(i) || s.b[i] != ';' {
		return 0, "", 0, false
	}
	return i + 1, string(s.b[start:i]), 0, true
}

// charRef is checkCharRefNumber: the character of a character reference
// or false.
func charRef(r rune) (rune, bool) {
	if r >= 0x110000 || r >= 0xD800 && r <= 0xDFFF || r == 0xFFFE || r == 0xFFFF ||
		r < 0x80 && asciiTypes[r] == btNonXML {
		return 0, false
	}
	return r, true
}

// entity is the entity a reference names in content or an attribute
// value, nil when it is skipped without error.
func (p *parser) entity(name string) (*entityDecl, error) {
	e := p.general[name]
	if e == nil {
		if !p.hasParamRefs || p.standalone {
			return nil, errNotWellFormed
		}
		return nil, nil
	}
	if e.open || e.notation != "" || !e.internal {
		return nil, errNotWellFormed
	}
	return e, nil
}

// attrValue is appendAttributeValue: s from its position to its end is the
// text of an attribute value.
func (p *parser) attrValue(s *source, isCdata bool, out []byte) ([]byte, error) {
	space := func() {
		if isCdata || len(out) > 0 && out[len(out)-1] != ' ' {
			out = append(out, ' ')
		}
	}
	for s.has(s.pos) {
		t, _, n := s.typeAt(s.pos)
		switch t {
		case btAmp:
			end, name, char, ok := s.reference(s.pos + 1)
			if !ok {
				return nil, errNotWellFormed
			}
			s.pos = end
			if name == "" {
				c, ok := charRef(char)
				if !ok {
					return nil, errNotWellFormed
				}
				if c == ' ' && !isCdata && (len(out) == 0 || out[len(out)-1] == ' ') {
					continue
				}
				out = utf8.AppendRune(out, c)
				continue
			}
			if c, ok := predefined[name]; ok {
				out = append(out, c)
				continue
			}
			e, err := p.entity(name)
			if err != nil {
				return nil, err
			}
			if e == nil {
				continue // undefined, and the internal subset may lack declarations
			}
			if err := p.expanded(len(e.text)); err != nil {
				return nil, err
			}
			e.open = true
			out, err = p.attrValue(&source{b: []byte(e.text), internal: true}, isCdata, out)
			e.open = false
			if err != nil {
				return nil, err
			}
		case btLT:
			return nil, errNotWellFormed
		case btS:
			space()
			s.pos += n
		case btLF:
			space()
			s.pos++
		case btCR:
			s.pos++
			if s.has(s.pos) && s.b[s.pos] == '\n' {
				s.pos++
			}
			space()
		default:
			out = append(out, s.b[s.pos:s.pos+n]...)
			s.pos += n
		}
	}
	return out, nil
}

// storeAttributeValue is appendAttributeValue with the trailing space of
// a tokenized value chopped.
func (p *parser) storeAttributeValue(s *source, isCdata bool) (string, error) {
	out, err := p.attrValue(s, isCdata, nil)
	if err != nil {
		return "", err
	}
	if !isCdata && len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	return string(out), nil
}

// attributeID is getAttributeId.
func (p *parser) attributeID(name string) *attID {
	if id := p.attIDs[name]; id != nil {
		return id
	}
	id := &attID{name: name}
	switch {
	case name == "xmlns":
		id.xmlns, id.prefixed = true, true
	case strings.HasPrefix(name, "xmlns:"):
		id.xmlns, id.prefixed, id.prefix = true, true, name[len("xmlns:"):]
	default:
		if prefix, _, ok := strings.Cut(name, ":"); ok {
			id.prefixed, id.prefix = true, prefix
		}
	}
	p.attIDs[name] = id
	return id
}

func (p *parser) uri(prefix string) (string, bool) {
	stack := p.bindings[prefix]
	if len(stack) == 0 || stack[len(stack)-1] == "" {
		return "", false
	}
	return stack[len(stack)-1], true
}

// addBinding is addBinding of expat for xmlns (defaultNS) or xmlns:prefix.
func (p *parser) addBinding(prefix string, defaultNS bool, uri string, tag *openTag) error {
	if uri == "" && !defaultNS {
		return errNotWellFormed
	}
	mustBeXML := false
	if !defaultNS {
		if prefix == "xmlns" {
			return errNotWellFormed
		}
		mustBeXML = prefix == "xml"
	}
	if strings.ContainsRune(uri, nsSeparator) {
		return errNotWellFormed
	}
	if mustBeXML != (uri == xmlNamespace) || uri == xmlnsNamespace {
		return errNotWellFormed
	}
	key := prefix
	if defaultNS {
		key = ""
	}
	p.bindings[key] = append(p.bindings[key], uri)
	tag.bound = append(tag.bound, key)
	return nil
}

type rawAtt struct {
	name       string
	start, end int // the value between the quotes
}

// startTag scans a start tag at s.pos ("<" and a name start): the name,
// the attributes and whether it closes itself. s.pos ends after the tag.
func (s *source) startTag() (name string, atts []rawAtt, empty bool, err error) {
	i := s.pos + 1
	end, ok := s.qname(i)
	if !ok {
		return "", nil, false, errNotWellFormed
	}
	name = string(s.b[i:end])
	i = end
	for {
		if !s.has(i) {
			return "", nil, false, errNotWellFormed
		}
		t, _, _ := s.typeAt(i)
		switch {
		case t == btGT:
			s.pos = i + 1
			return name, atts, false, nil
		case t == btSol:
			if !s.has(i+1) || s.b[i+1] != '>' {
				return "", nil, false, errNotWellFormed
			}
			s.pos = i + 2
			return name, atts, true, nil
		case !isSpaceType(t):
			return "", nil, false, errNotWellFormed
		}
		i = s.skipSpace(i)
		if !s.has(i) {
			return "", nil, false, errNotWellFormed
		}
		if t, _, _ := s.typeAt(i); t == btGT || t == btSol {
			continue
		}
		nameEnd, ok := s.qname(i)
		if !ok {
			return "", nil, false, errNotWellFormed
		}
		att := rawAtt{name: string(s.b[i:nameEnd])}
		i = s.skipSpace(nameEnd)
		if !s.has(i) || s.b[i] != '=' {
			return "", nil, false, errNotWellFormed
		}
		i = s.skipSpace(i + 1)
		if !s.has(i) || s.b[i] != '"' && s.b[i] != '\'' {
			return "", nil, false, errNotWellFormed
		}
		open := s.b[i]
		i++
		att.start = i
		for {
			if !s.has(i) {
				return "", nil, false, errNotWellFormed
			}
			c := s.b[i]
			if c == open {
				break
			}
			switch c {
			case '<':
				return "", nil, false, errNotWellFormed
			case '&':
				refEnd, _, _, ok := s.reference(i + 1)
				if !ok {
					return "", nil, false, errNotWellFormed
				}
				i = refEnd
			default:
				_, _, n := s.typeAt(i)
				i += n
			}
		}
		att.end = i
		atts = append(atts, att)
		i++
		// After the value: white space, "/" or ">".
		if !s.has(i) {
			return "", nil, false, errNotWellFormed
		}
		if t, _, _ := s.typeAt(i); !isSpaceType(t) && t != btSol && t != btGT {
			return "", nil, false, errNotWellFormed
		}
	}
}

// storeAtts handles a start tag as storeAtts of expat and the start
// handler of ElementTree: values, namespace declarations, defaults of the
// internal subset, the checks of names and the element of the tree.
func (p *parser) storeAtts(s *source, rawName string, atts []rawAtt) error {
	decl := p.elements[rawName]
	tag := openTag{rawName: rawName}
	type reported struct {
		id    *attID
		value string
	}
	var list []reported
	seen := map[*attID]bool{}
	for _, att := range atts {
		id := p.attributeID(att.name)
		if seen[id] {
			return errNotWellFormed
		}
		seen[id] = true
		isCdata := true
		if id.maybeTokenized && decl != nil {
			for _, d := range decl.defaults {
				if d.id == id {
					isCdata = d.isCdata
					break
				}
			}
		}
		value, err := p.storeAttributeValue(&source{b: s.b[att.start:att.end], internal: s.internal,
			latin1: s.latin1}, isCdata)
		if err != nil {
			return err
		}
		if id.xmlns {
			if err := p.addBinding(id.prefix, id.prefix == "", value, &tag); err != nil {
				return err
			}
			continue
		}
		list = append(list, reported{id, value})
	}
	if decl != nil {
		for _, d := range decl.defaults {
			if seen[d.id] || d.value == nil {
				continue
			}
			seen[d.id] = true
			if d.id.xmlns {
				if err := p.addBinding(d.id.prefix, d.id.prefix == "", *d.value, &tag); err != nil {
					return err
				}
				continue
			}
			list = append(list, reported{d.id, *d.value})
		}
	}
	e := &element{tag: rawName}
	expandedNames := map[string]bool{}
	for _, att := range list {
		if !att.id.prefixed {
			e.attrs = append(e.attrs, attr{att.id.name, att.value})
			continue
		}
		uri, ok := p.uri(att.id.prefix)
		if !ok {
			return errNotWellFormed
		}
		expandedName := uri + string(nsSeparator) + att.id.name[len(att.id.prefix)+1:]
		if expandedNames[expandedName] {
			return errNotWellFormed
		}
		expandedNames[expandedName] = true
	}
	if prefix, local, ok := strings.Cut(rawName, ":"); ok {
		if _, bound := p.uri(prefix); !bound {
			return errNotWellFormed
		}
		e.tag = local
	}
	p.tags = append(p.tags, tag)
	p.tree.start(e)
	return nil
}

func (p *parser) endTag() {
	tag := p.tags[len(p.tags)-1]
	for _, key := range tag.bound {
		p.bindings[key] = p.bindings[key][:len(p.bindings[key])-1]
	}
	p.tags = p.tags[:len(p.tags)-1]
	p.tree.end()
}

// content is doContent: the document from the start tag of the root to its
// end tag, or the replacement text of an entity, which must hold whole
// elements.
func (p *parser) content(s *source, entity bool) error {
	startLevel := len(p.tags)
	for {
		if !entity {
			if err := p.token(); err != nil {
				return err
			}
		}
		if !s.has(s.pos) {
			if !entity || len(p.tags) != startLevel {
				return errNotWellFormed
			}
			return nil
		}
		t, _, n := s.typeAt(s.pos)
		switch t {
		case btLT:
			if err := p.markup(s, startLevel); err != nil {
				return err
			}
			if !entity && len(p.tags) == 0 {
				return nil
			}
		case btAmp:
			end, name, char, ok := s.reference(s.pos + 1)
			if !ok {
				return errNotWellFormed
			}
			s.pos = end
			if name == "" {
				c, ok := charRef(char)
				if !ok {
					return errNotWellFormed
				}
				p.tree.data = utf8.AppendRune(p.tree.data, c)
				continue
			}
			if c, ok := predefined[name]; ok {
				p.tree.data = append(p.tree.data, c)
				continue
			}
			e, err := p.entity(name)
			if err != nil {
				return err
			}
			if e == nil {
				return errNotWellFormed // ElementTree: undefined entity
			}
			if err := p.expanded(len(e.text)); err != nil {
				return err
			}
			e.open = true
			err = p.content(&source{b: []byte(e.text), internal: true}, true)
			e.open = false
			if err != nil {
				return err
			}
		case btRSqb:
			if s.hasPrefix(s.pos, "]]>") {
				return errNotWellFormed
			}
			p.tree.data = append(p.tree.data, ']')
			s.pos++
		case btCR:
			s.pos++
			if s.has(s.pos) && s.b[s.pos] == '\n' {
				s.pos++
			}
			p.tree.data = append(p.tree.data, '\n')
		default:
			start := s.pos
			s.pos += n
			for s.has(s.pos) {
				c := s.b[s.pos]
				if c == '<' || c == '&' || c == ']' || c == '\r' && !s.internal {
					break
				}
				s.pos++
			}
			p.tree.data = append(p.tree.data, s.b[start:s.pos]...)
		}
	}
}

// markup handles what starts with "<" in content.
func (p *parser) markup(s *source, startLevel int) error {
	switch {
	case s.hasPrefix(s.pos, "<!--"):
		end, ok := s.commentEnd(s.pos)
		if !ok {
			return errNotWellFormed
		}
		s.pos = end
	case s.hasPrefix(s.pos, "<![CDATA["):
		i := s.pos + len("<![CDATA[")
		for {
			if !s.has(i) {
				return errNotWellFormed
			}
			if s.hasPrefix(i, "]]>") {
				s.pos = i + 3
				return nil
			}
			if s.b[i] == '\r' && !s.internal {
				i++
				if s.has(i) && s.b[i] == '\n' {
					i++
				}
				p.tree.data = append(p.tree.data, '\n')
				continue
			}
			_, _, n := s.typeAt(i)
			p.tree.data = append(p.tree.data, s.b[i:i+n]...)
			i += n
		}
	case s.hasPrefix(s.pos, "<!"):
		return errNotWellFormed
	case s.hasPrefix(s.pos, "<?"):
		end, xmlDecl, ok := s.piEnd(s.pos)
		if !ok || xmlDecl {
			return errNotWellFormed
		}
		s.pos = end
	case s.hasPrefix(s.pos, "</"):
		i := s.pos + 2
		if !s.has(i) {
			return errNotWellFormed
		}
		if t, _, _ := s.typeAt(i); !isNameStartType(t) {
			return errNotWellFormed
		}
		end := i
		for s.has(end) {
			t, _, n := s.typeAt(end)
			if !isNameType(t) && t != btColon {
				break
			}
			end += n
		}
		name := string(s.b[i:end])
		j := s.skipSpace(end)
		if !s.has(j) || s.b[j] != '>' {
			return errNotWellFormed
		}
		s.pos = j + 1
		if len(p.tags) == startLevel || p.tags[len(p.tags)-1].rawName != name {
			return errNotWellFormed
		}
		p.endTag()
	default:
		name, atts, empty, err := s.startTag()
		if err != nil {
			return err
		}
		if s == p.doc {
			if err := p.token(); err != nil {
				return err
			}
		}
		if err := p.storeAtts(s, name, atts); err != nil {
			return err
		}
		if empty {
			p.endTag()
		}
	}
	return nil
}

// epilog is epilogProcessor: after the root only white space, comments and
// processing instructions.
func (p *parser) epilog() error {
	s := p.doc
	for s.has(s.pos) {
		if err := p.token(); err != nil {
			return err
		}
		t, _, _ := s.typeAt(s.pos)
		switch {
		case isSpaceType(t):
			s.pos = s.skipSpace(s.pos)
		case s.hasPrefix(s.pos, "<!--"):
			end, ok := s.commentEnd(s.pos)
			if !ok {
				return errNotWellFormed
			}
			s.pos = end
		case s.hasPrefix(s.pos, "<?"):
			end, xmlDecl, ok := s.piEnd(s.pos)
			if !ok || xmlDecl {
				return errNotWellFormed
			}
			s.pos = end
		default:
			return errNotWellFormed
		}
	}
	return p.token()
}
