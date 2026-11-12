package maven

import "strings"

// The prolog of a document: XmlPrologTok of xmltok_impl.c, the state
// machine of xmlrole.c and what doProlog of xmlparse.c does with its roles.
// Parameter entities are never read (ElementTree leaves expat at
// XML_PARAM_ENTITY_PARSING_NEVER), so the internal subset is only the
// declarations of the document itself.

type ptok int8

const (
	ptNone ptok = iota
	ptInvalid
	ptPrologS
	ptPI
	ptXMLDecl
	ptComment
	ptDeclOpen
	ptInstanceStart
	ptOpenBracket
	ptCloseBracket
	ptCondSectOpen
	ptCondSectClose
	ptDeclClose
	ptParamEntityRef
	ptPercent
	ptName
	ptPrefixedName
	ptNmtoken
	ptNameQuestion
	ptNameAsterisk
	ptNamePlus
	ptPoundName
	ptLiteral
	ptOpenParen
	ptCloseParen
	ptCloseParenQuestion
	ptCloseParenAsterisk
	ptCloseParenPlus
	ptOr
	ptComma
)

// commentEnd is scanComment for "<!--" at i: the index after "-->".
func (s *source) commentEnd(i int) (int, bool) {
	i += 4
	for {
		if !s.has(i + 1) {
			return 0, false
		}
		if s.b[i] == '-' && s.b[i+1] == '-' {
			if !s.has(i+2) || s.b[i+2] != '>' {
				return 0, false
			}
			return i + 3, true
		}
		_, _, n := s.typeAt(i)
		i += n
	}
}

// piEnd is scanPi for "<?" at i. xmlDecl is true for the target "xml",
// which expat takes for an XML declaration; a target that is "xml" in
// another case is an error.
func (s *source) piEnd(i int) (end int, xmlDecl, ok bool) {
	i += 2
	if !s.has(i) {
		return 0, false, false
	}
	t, _, n := s.typeAt(i)
	if !isNameStartType(t) {
		return 0, false, false
	}
	target := i
	i = s.skipName(i + n)
	if !s.has(i) {
		return 0, false, false
	}
	if name := string(s.b[target:i]); len(name) == 3 && (name[0]|0x20) == 'x' && (name[1]|0x20) == 'm' &&
		(name[2]|0x20) == 'l' {
		if name != "xml" {
			return 0, false, false
		}
		xmlDecl = true
	}
	t, _, _ = s.typeAt(i)
	switch {
	case t == btQuest:
		if !s.has(i+1) || s.b[i+1] != '>' {
			return 0, false, false
		}
		return i + 2, xmlDecl, true
	case isSpaceType(t):
		for i++; s.has(i); {
			if s.b[i] == '?' && s.has(i+1) && s.b[i+1] == '>' {
				return i + 2, xmlDecl, true
			}
			_, _, n := s.typeAt(i)
			i += n
		}
	}
	return 0, false, false
}

// prologTok is XmlPrologTok at s.pos: the token and the index after it. A
// token cut off by the end of the document counts as invalid, as no root
// can follow it.
func (s *source) prologTok() (ptok, int) {
	i := s.pos
	if !s.has(i) {
		return ptNone, i
	}
	t, _, n := s.typeAt(i)
	tok := ptName
	switch t {
	case btQuot, btApos:
		open := s.b[i]
		for j := i + 1; s.has(j); {
			if s.b[j] != open {
				_, _, n := s.typeAt(j)
				j += n
				continue
			}
			j++
			if !s.has(j) {
				return ptInvalid, 0
			}
			switch t, _, _ := s.typeAt(j); t {
			case btS, btCR, btLF, btGT, btPercnt, btLSqb:
				return ptLiteral, j
			}
			return ptInvalid, 0
		}
		return ptInvalid, 0
	case btLT:
		if !s.has(i + 1) {
			return ptInvalid, 0
		}
		next, _, _ := s.typeAt(i + 1)
		switch {
		case next == btExcl:
			return s.scanDecl(i)
		case next == btQuest:
			end, xmlDecl, ok := s.piEnd(i)
			switch {
			case !ok:
				return ptInvalid, 0
			case xmlDecl:
				return ptXMLDecl, end
			}
			return ptPI, end
		case isNameStartType(next), s.b[i+1] >= 0x80:
			// The content parser checks the start tag.
			return ptInstanceStart, i
		}
		return ptInvalid, 0
	case btS, btCR, btLF:
		return ptPrologS, s.skipSpace(i)
	case btPercnt:
		return s.scanPercent(i + 1)
	case btComma:
		return ptComma, i + 1
	case btLSqb:
		return ptOpenBracket, i + 1
	case btRSqb:
		if s.hasPrefix(i+1, "]>") {
			return ptCondSectClose, i + 3
		}
		return ptCloseBracket, i + 1
	case btLPar:
		return ptOpenParen, i + 1
	case btRPar:
		if !s.has(i + 1) {
			return ptInvalid, 0
		}
		switch t, _, _ := s.typeAt(i + 1); t {
		case btAst:
			return ptCloseParenAsterisk, i + 2
		case btQuest:
			return ptCloseParenQuestion, i + 2
		case btPlus:
			return ptCloseParenPlus, i + 2
		case btCR, btLF, btS, btGT, btComma, btVerbar, btRPar:
			return ptCloseParen, i + 1
		}
		return ptInvalid, 0
	case btVerbar:
		return ptOr, i + 1
	case btGT:
		return ptDeclClose, i + 1
	case btNum:
		if !s.has(i + 1) {
			return ptInvalid, 0
		}
		first, _, width := s.typeAt(i + 1)
		if !isNameStartType(first) {
			return ptInvalid, 0
		}
		j := s.skipName(i + 1 + width)
		if !s.has(j) {
			return ptInvalid, 0
		}
		switch t, _, _ := s.typeAt(j); t {
		case btCR, btLF, btS, btRPar, btGT, btPercnt, btVerbar:
			return ptPoundName, j
		}
		return ptInvalid, 0
	case btNmstrt, btHex:
	case btDigit, btName, btMinus, btColon:
		tok = ptNmtoken
	default:
		return ptInvalid, 0
	}
	i += n
	for s.has(i) {
		t, _, n := s.typeAt(i)
		switch t {
		case btNmstrt, btHex, btDigit, btName, btMinus:
			i += n
		case btGT, btRPar, btComma, btVerbar, btLSqb, btPercnt, btS, btCR, btLF:
			return tok, i
		case btColon:
			i++
			switch tok {
			case ptName:
				if !s.has(i) {
					return ptInvalid, 0
				}
				tok = ptPrefixedName
				if t, _, n := s.typeAt(i); isNameType(t) {
					i += n
				} else {
					tok = ptNmtoken
				}
			case ptPrefixedName:
				tok = ptNmtoken
			}
		case btPlus, btAst, btQuest:
			if tok == ptNmtoken {
				return ptInvalid, 0
			}
			return map[btype]ptok{btPlus: ptNamePlus, btAst: ptNameAsterisk, btQuest: ptNameQuestion}[t], i + 1
		default:
			return ptInvalid, 0
		}
	}
	return ptInvalid, 0
}

// scanDecl is scanDecl for "<!" at i.
func (s *source) scanDecl(i int) (ptok, int) {
	j := i + 2
	if !s.has(j) {
		return ptInvalid, 0
	}
	switch c := s.b[j]; {
	case c == '-':
		if !s.has(j+1) || s.b[j+1] != '-' {
			return ptInvalid, 0
		}
		end, ok := s.commentEnd(i)
		if !ok {
			return ptInvalid, 0
		}
		return ptComment, end
	case c == '[':
		return ptCondSectOpen, j + 1
	case c < 0x80 && isNameStartType(asciiTypes[c]):
	default:
		return ptInvalid, 0
	}
	for j++; s.has(j); j++ {
		c := s.b[j]
		switch {
		case c < 0x80 && isNameStartType(asciiTypes[c]):
		case c == '%':
			if !s.has(j + 1) {
				return ptInvalid, 0
			}
			if t, _, _ := s.typeAt(j + 1); isSpaceType(t) || t == btPercnt {
				return ptInvalid, 0
			}
			return ptDeclOpen, j
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			return ptDeclOpen, j
		default:
			return ptInvalid, 0
		}
	}
	return ptInvalid, 0
}

// scanPercent is scanPercent after "%" at i.
func (s *source) scanPercent(i int) (ptok, int) {
	if !s.has(i) {
		return ptInvalid, 0
	}
	t, _, n := s.typeAt(i)
	switch {
	case isSpaceType(t), t == btPercnt:
		return ptPercent, i
	case !isNameStartType(t):
		return ptInvalid, 0
	}
	i = s.skipName(i + n)
	if s.has(i) && s.b[i] == ';' {
		return ptParamEntityRef, i + 1
	}
	return ptInvalid, 0
}

type roleHandler int8

const (
	hProlog0 roleHandler = iota
	hProlog1
	hProlog2
	hDoctype0
	hDoctype1
	hDoctype2
	hDoctype3
	hDoctype4
	hDoctype5
	hInternalSubset
	hEntity0
	hEntity1
	hEntity2
	hEntity3
	hEntity4
	hEntity5
	hEntity6
	hEntity7
	hEntity8
	hEntity9
	hEntity10
	hNotation0
	hNotation1
	hNotation2
	hNotation3
	hNotation4
	hAttlist0
	hAttlist1
	hAttlist2
	hAttlist3
	hAttlist4
	hAttlist5
	hAttlist6
	hAttlist7
	hAttlist8
	hAttlist9
	hElement0
	hElement1
	hElement2
	hElement3
	hElement4
	hElement5
	hElement6
	hElement7
	hDeclClose
	hError
)

type role int8

const (
	roleNone role = iota
	roleError
	roleInstanceStart
	roleDoctypePublicID
	roleDoctypeSystemID
	roleGeneralEntityName
	roleParamEntityName
	roleEntityValue
	roleEntityPublicID
	roleEntityNotationName
	roleNotationPublicID
	roleAttlistElementName
	roleAttributeName
	roleAttributeTypeCdata
	roleAttributeTypeID
	roleAttributeTypeOther
	roleImpliedAttributeValue
	roleRequiredAttributeValue
	roleDefaultAttributeValue
	roleFixedAttributeValue
	roleGroupOpen
	roleGroupSequence
	roleGroupChoice
	roleParamEntityRef
)

type roleState struct {
	handler roleHandler
	level   int
}

// next is the handler of xmlrole.c for one token; text is the token.
func (st *roleState) next(tok ptok, text string) role {
	if tok == ptPrologS && st.handler != hProlog0 {
		return roleNone
	}
	is := func(handler roleHandler, r role) role {
		st.handler = handler
		return r
	}
	name := tok == ptName || tok == ptPrefixedName
	switch st.handler {
	case hProlog0, hProlog1:
		switch {
		case tok == ptPrologS, tok == ptPI, tok == ptComment:
			return is(hProlog1, roleNone)
		case tok == ptDeclOpen && text == "<!DOCTYPE":
			return is(hDoctype0, roleNone)
		case tok == ptInstanceStart:
			return is(hError, roleInstanceStart)
		}
	case hProlog2:
		switch {
		case tok == ptPI, tok == ptComment:
			return roleNone
		case tok == ptInstanceStart:
			return is(hError, roleInstanceStart)
		}
	case hDoctype0:
		if name {
			return is(hDoctype1, roleNone)
		}
	case hDoctype1:
		switch {
		case tok == ptOpenBracket:
			return is(hInternalSubset, roleNone)
		case tok == ptDeclClose:
			return is(hProlog2, roleNone)
		case tok == ptName && text == "SYSTEM":
			return is(hDoctype3, roleNone)
		case tok == ptName && text == "PUBLIC":
			return is(hDoctype2, roleNone)
		}
	case hDoctype2:
		if tok == ptLiteral {
			return is(hDoctype3, roleDoctypePublicID)
		}
	case hDoctype3:
		if tok == ptLiteral {
			return is(hDoctype4, roleDoctypeSystemID)
		}
	case hDoctype4:
		switch tok {
		case ptOpenBracket:
			return is(hInternalSubset, roleNone)
		case ptDeclClose:
			return is(hProlog2, roleNone)
		}
	case hDoctype5:
		if tok == ptDeclClose {
			return is(hProlog2, roleNone)
		}
	case hInternalSubset:
		switch {
		case tok == ptDeclOpen && text == "<!ENTITY":
			return is(hEntity0, roleNone)
		case tok == ptDeclOpen && text == "<!ATTLIST":
			return is(hAttlist0, roleNone)
		case tok == ptDeclOpen && text == "<!ELEMENT":
			return is(hElement0, roleNone)
		case tok == ptDeclOpen && text == "<!NOTATION":
			return is(hNotation0, roleNone)
		case tok == ptPI, tok == ptComment:
			return roleNone
		case tok == ptParamEntityRef:
			return roleParamEntityRef
		case tok == ptCloseBracket:
			return is(hDoctype5, roleNone)
		}
	case hEntity0:
		switch tok {
		case ptPercent:
			return is(hEntity1, roleNone)
		case ptName:
			return is(hEntity2, roleGeneralEntityName)
		}
	case hEntity1:
		if tok == ptName {
			return is(hEntity7, roleParamEntityName)
		}
	case hEntity2, hEntity7:
		system, public, value := hEntity4, hEntity3, hDeclClose
		if st.handler == hEntity7 {
			system, public = hEntity9, hEntity8
		}
		switch {
		case tok == ptName && text == "SYSTEM":
			return is(system, roleNone)
		case tok == ptName && text == "PUBLIC":
			return is(public, roleNone)
		case tok == ptLiteral:
			return is(value, roleEntityValue)
		}
	case hEntity3:
		if tok == ptLiteral {
			return is(hEntity4, roleEntityPublicID)
		}
	case hEntity4:
		if tok == ptLiteral {
			return is(hEntity5, roleNone)
		}
	case hEntity5:
		switch {
		case tok == ptDeclClose:
			return is(hInternalSubset, roleNone)
		case tok == ptName && text == "NDATA":
			return is(hEntity6, roleNone)
		}
	case hEntity6:
		if tok == ptName {
			return is(hDeclClose, roleEntityNotationName)
		}
	case hEntity8:
		if tok == ptLiteral {
			return is(hEntity9, roleEntityPublicID)
		}
	case hEntity9:
		if tok == ptLiteral {
			return is(hEntity10, roleNone)
		}
	case hEntity10:
		if tok == ptDeclClose {
			return is(hInternalSubset, roleNone)
		}
	case hNotation0:
		if tok == ptName {
			return is(hNotation1, roleNone)
		}
	case hNotation1:
		switch {
		case tok == ptName && text == "SYSTEM":
			return is(hNotation3, roleNone)
		case tok == ptName && text == "PUBLIC":
			return is(hNotation2, roleNone)
		}
	case hNotation2:
		if tok == ptLiteral {
			return is(hNotation4, roleNotationPublicID)
		}
	case hNotation3:
		if tok == ptLiteral {
			return is(hDeclClose, roleNone)
		}
	case hNotation4:
		switch tok {
		case ptLiteral:
			return is(hDeclClose, roleNone)
		case ptDeclClose:
			return is(hInternalSubset, roleNone)
		}
	case hAttlist0:
		if name {
			return is(hAttlist1, roleAttlistElementName)
		}
	case hAttlist1:
		switch {
		case tok == ptDeclClose:
			return is(hInternalSubset, roleNone)
		case name:
			return is(hAttlist2, roleAttributeName)
		}
	case hAttlist2:
		switch {
		case tok == ptName && text == "CDATA":
			return is(hAttlist8, roleAttributeTypeCdata)
		case tok == ptName && text == "ID":
			return is(hAttlist8, roleAttributeTypeID)
		case tok == ptName && (text == "IDREF" || text == "IDREFS" || text == "ENTITY" || text == "ENTITIES" ||
			text == "NMTOKEN" || text == "NMTOKENS"):
			return is(hAttlist8, roleAttributeTypeOther)
		case tok == ptName && text == "NOTATION":
			return is(hAttlist5, roleNone)
		case tok == ptOpenParen:
			return is(hAttlist3, roleNone)
		}
	case hAttlist3:
		if tok == ptNmtoken || name {
			return is(hAttlist4, roleNone)
		}
	case hAttlist4:
		switch tok {
		case ptCloseParen:
			return is(hAttlist8, roleNone)
		case ptOr:
			return is(hAttlist3, roleNone)
		}
	case hAttlist5:
		if tok == ptOpenParen {
			return is(hAttlist6, roleNone)
		}
	case hAttlist6:
		if tok == ptName {
			return is(hAttlist7, roleNone)
		}
	case hAttlist7:
		switch tok {
		case ptCloseParen:
			return is(hAttlist8, roleNone)
		case ptOr:
			return is(hAttlist6, roleNone)
		}
	case hAttlist8:
		switch {
		case tok == ptPoundName && text == "#IMPLIED":
			return is(hAttlist1, roleImpliedAttributeValue)
		case tok == ptPoundName && text == "#REQUIRED":
			return is(hAttlist1, roleRequiredAttributeValue)
		case tok == ptPoundName && text == "#FIXED":
			return is(hAttlist9, roleNone)
		case tok == ptLiteral:
			return is(hAttlist1, roleDefaultAttributeValue)
		}
	case hAttlist9:
		if tok == ptLiteral {
			return is(hAttlist1, roleFixedAttributeValue)
		}
	case hElement0:
		if name {
			return is(hElement1, roleNone)
		}
	case hElement1:
		switch {
		case tok == ptName && (text == "EMPTY" || text == "ANY"):
			return is(hDeclClose, roleNone)
		case tok == ptOpenParen:
			st.level = 1
			return is(hElement2, roleGroupOpen)
		}
	case hElement2:
		switch {
		case tok == ptPoundName && text == "#PCDATA":
			return is(hElement3, roleNone)
		case tok == ptOpenParen:
			st.level = 2
			return is(hElement6, roleGroupOpen)
		case name, tok == ptNameQuestion, tok == ptNameAsterisk, tok == ptNamePlus:
			return is(hElement7, roleNone)
		}
	case hElement3:
		switch tok {
		case ptCloseParen, ptCloseParenAsterisk:
			return is(hDeclClose, roleNone)
		case ptOr:
			return is(hElement4, roleGroupChoice)
		}
	case hElement4:
		if name {
			return is(hElement5, roleNone)
		}
	case hElement5:
		switch tok {
		case ptCloseParenAsterisk:
			return is(hDeclClose, roleNone)
		case ptOr:
			return is(hElement4, roleGroupChoice)
		}
	case hElement6:
		switch {
		case tok == ptOpenParen:
			st.level++
			return roleGroupOpen
		case name, tok == ptNameQuestion, tok == ptNameAsterisk, tok == ptNamePlus:
			return is(hElement7, roleNone)
		}
	case hElement7:
		switch tok {
		case ptCloseParen, ptCloseParenAsterisk, ptCloseParenQuestion, ptCloseParenPlus:
			st.level--
			if st.level == 0 {
				st.handler = hDeclClose
			}
			return roleNone
		case ptComma:
			return is(hElement6, roleGroupSequence)
		case ptOr:
			return is(hElement6, roleGroupChoice)
		}
	case hDeclClose:
		if tok == ptDeclClose {
			return is(hInternalSubset, roleNone)
		}
	}
	st.handler = hError
	return roleError
}

// isPublicID is XmlIsPublicId for the text between the quotes.
func isPublicID(text []byte) bool {
	for _, c := range text {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == ' ' || c == '\r' || c == '\n':
		case strings.IndexByte("-'()+,./:=?;!*#@$_%", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// storeEntityValue is storeEntityValue of the internal subset: character
// references replaced, entity references kept as they are, new lines
// normalized; a parameter entity reference or a lone "%" is an error.
func (p *parser) storeEntityValue(s *source) (string, error) {
	var out []byte
	for s.has(s.pos) {
		t, _, n := s.typeAt(s.pos)
		switch t {
		case btPercnt:
			return "", errNotWellFormed
		case btAmp:
			end, name, char, ok := s.reference(s.pos + 1)
			if !ok {
				return "", errNotWellFormed
			}
			if name == "" {
				c, ok := charRef(char)
				if !ok {
					return "", errNotWellFormed
				}
				out = append(out, string(c)...)
			} else {
				out = append(out, s.b[s.pos:end]...)
			}
			s.pos = end
		case btLF:
			out = append(out, '\n')
			s.pos++
		case btCR:
			out = append(out, '\n')
			s.pos++
			if s.has(s.pos) && s.b[s.pos] == '\n' {
				s.pos++
			}
		default:
			out = append(out, s.b[s.pos:s.pos+n]...)
			s.pos += n
		}
	}
	return string(out), nil
}

func (p *parser) defineAttribute(decl *elementDecl, id *attID, isCdata, isID bool, value *string) {
	if value != nil || isID {
		for _, d := range decl.defaults {
			if d.id == id {
				return
			}
		}
	}
	decl.defaults = append(decl.defaults, defaultAtt{id: id, value: value, isCdata: isCdata})
	if !isCdata {
		id.maybeTokenized = true
	}
}

// prolog is doProlog up to the start tag of the root; s.pos ends on its
// "<".
func (p *parser) prolog() error {
	s := p.doc
	st := roleState{handler: hProlog0}
	if p.dec.declared {
		st.handler = hProlog1
	}
	var (
		declEntity  *entityDecl
		declElement *elementDecl
		declAtt     *attID
		isCdata     bool
		isID        bool
		connectors  = map[int]byte{}
	)
	for {
		if err := p.token(); err != nil {
			return err
		}
		tok, end := s.prologTok()
		if tok == ptNone || tok == ptInvalid {
			return errNotWellFormed
		}
		start := s.pos
		text := string(s.b[start:end])
		literal := func() *source {
			return &source{b: s.b[start+1 : end-1], latin1: s.latin1}
		}
		if tok != ptInstanceStart {
			s.pos = end
		}
		switch st.next(tok, text) {
		case roleError:
			return errNotWellFormed
		case roleInstanceStart:
			return nil
		case roleDoctypePublicID:
			p.hasParamRefs = true
			if !isPublicID(literal().b) {
				return errNotWellFormed
			}
		case roleEntityPublicID, roleNotationPublicID:
			if !isPublicID(literal().b) {
				return errNotWellFormed
			}
		case roleDoctypeSystemID:
			p.hasParamRefs = true
		case roleGeneralEntityName:
			// A declaration of a predefined entity is ignored, and so is
			// any after the first of a name.
			declEntity = nil
			if _, ok := predefined[text]; !ok && p.keepProcessing && p.general[text] == nil {
				declEntity = &entityDecl{}
				p.general[text] = declEntity
			}
		case roleParamEntityName:
			declEntity = nil
			if p.keepProcessing && p.params[text] == nil {
				declEntity = &entityDecl{}
				p.params[text] = declEntity
			}
		case roleEntityValue:
			if p.keepProcessing {
				value, err := p.storeEntityValue(literal())
				if err != nil {
					return err
				}
				if declEntity != nil {
					declEntity.text, declEntity.internal = value, true
				}
			}
		case roleEntityNotationName:
			if p.keepProcessing && declEntity != nil {
				declEntity.notation = text
			}
		case roleAttlistElementName:
			declElement = p.elements[text]
			if declElement == nil {
				declElement = &elementDecl{}
				p.elements[text] = declElement
			}
		case roleAttributeName:
			declAtt, isCdata, isID = p.attributeID(text), false, false
		case roleAttributeTypeCdata:
			isCdata = true
		case roleAttributeTypeID:
			isID = true
		case roleImpliedAttributeValue, roleRequiredAttributeValue:
			if p.keepProcessing {
				p.defineAttribute(declElement, declAtt, isCdata, isID, nil)
			}
		case roleDefaultAttributeValue, roleFixedAttributeValue:
			if p.keepProcessing {
				value, err := p.storeAttributeValue(literal(), isCdata)
				if err != nil {
					return err
				}
				p.defineAttribute(declElement, declAtt, isCdata, false, &value)
			}
		case roleGroupOpen:
			connectors[st.level] = 0
		case roleGroupSequence:
			if connectors[st.level] == '|' {
				return errNotWellFormed
			}
			connectors[st.level] = ','
		case roleGroupChoice:
			if connectors[st.level] == ',' {
				return errNotWellFormed
			}
			connectors[st.level] = '|'
		case roleParamEntityRef:
			p.hasParamRefs = true
			p.keepProcessing = p.standalone
		}
	}
}
