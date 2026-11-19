package npm

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// The streaming reader of a publish document (task 11 of the phase 4 plan).
// An npm publish is one JSON object whose _attachments hold the tarball as
// base64 text (libnpmpublish publish.js, npm/cli branch latest, read
// 2026-11-18). The reader decodes every other value into memory, at most
// MaxPublishDocument bytes of JSON in all, and streams the data of each
// attachment through a base64 decoder into a writer, so a tarball is never
// held in memory.

const (
	// MaxPublishDocument is the most JSON of a publish document outside the
	// data of its attachments: the largest stored package document the read
	// path takes.
	MaxPublishDocument = 16 << 20
	// maxPublishDepth is the deepest nesting of arrays and objects taken.
	maxPublishDepth = 512
	// maxKeyBytes is the longest key taken, quotes and escapes included.
	maxKeyBytes = 64 << 10
	readBuffer  = 64 << 10
)

// Attachment is one attachment of a publish document.
type Attachment struct {
	Name        string
	ContentType string // "" when the document gives none
	Length      int64  // the length the document gives, -1 for none
	Size        int64  // the bytes its data decoded to
}

// ReadPublish reads one publish document from r. open is called once per
// attachment, before its data, and gives the writer the decoded data goes
// to; an error of open, of r or of the writer ends the read and is
// returned as it is. A document this server does not take is a *Refusal.
func ReadPublish(r io.Reader, open func(name string) (io.Writer, error)) (*pyjson.Object, []Attachment, error) {
	sc := &scanner{r: bufio.NewReaderSize(r, readBuffer), budget: MaxPublishDocument}
	c, err := sc.next()
	if err != nil {
		return nil, nil, ended(err)
	}
	if c != '{' {
		return nil, nil, refused("a publish document is a JSON object")
	}
	document := pyjson.NewObject()
	var attachments []Attachment
	seen := false
	err = sc.members(func(key string) error {
		if key != "_attachments" {
			value, err := sc.value()
			if err == nil {
				document.Set(key, value)
			}
			return err
		}
		if seen {
			return refused("the document holds _attachments twice")
		}
		seen = true
		var err error
		attachments, err = sc.attachments(open)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	if _, err := sc.next(); err != io.EOF {
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, refused("the publish document goes on after its end")
	}
	return document, attachments, nil
}

// scanner reads JSON from a buffered reader; budget is what may still be
// read outside the data of attachments.
type scanner struct {
	r      *bufio.Reader
	budget int64
}

// ended turns the end of the input inside a document into a refusal: the
// body was complete, the document was not.
func ended(err error) error {
	if errors.Is(err, io.EOF) {
		return refused("the publish document ends before it is complete")
	}
	return err
}

func (sc *scanner) take() (byte, error) {
	if sc.budget <= 0 {
		return 0, refused("the publish document holds more than " + strconv.Itoa(MaxPublishDocument) + " bytes besides the data of its attachments")
	}
	c, err := sc.r.ReadByte()
	if err != nil {
		return 0, err
	}
	sc.budget--
	return c, nil
}

func space(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// next is the next byte that is no white space, taken; io.EOF at the end.
func (sc *scanner) next() (byte, error) {
	for {
		c, err := sc.take()
		if err != nil || !space(c) {
			return c, err
		}
	}
}

// expect takes the next byte that is no white space and refuses another
// than want.
func (sc *scanner) expect(want byte, what string) error {
	c, err := sc.next()
	if err != nil {
		return ended(err)
	}
	if c != want {
		return refused("the publish document has " + strconv.QuoteRune(rune(c)) + " where " + what + " belongs")
	}
	return nil
}

// members reads the members of an object whose "{" is taken, up to its
// "}": each key, then each takes its value.
func (sc *scanner) members(each func(key string) error) error {
	c, err := sc.next()
	if err != nil {
		return ended(err)
	}
	if c == '}' {
		return nil
	}
	for {
		if c != '"' {
			return refused("a key of the publish document is not a string")
		}
		key, err := sc.key()
		if err != nil {
			return err
		}
		if err := sc.expect(':', "a colon"); err != nil {
			return err
		}
		if err := each(key); err != nil {
			return err
		}
		if c, err = sc.next(); err != nil {
			return ended(err)
		}
		switch c {
		case '}':
			return nil
		case ',':
			if c, err = sc.next(); err != nil {
				return ended(err)
			}
		default:
			return refused("the publish document has " + strconv.QuoteRune(rune(c)) + " where a comma or a closing brace belongs")
		}
	}
}

// key reads a string whose opening quote is taken, decoded as json.loads
// decodes it.
func (sc *scanner) key() (string, error) {
	var raw bytes.Buffer
	raw.WriteByte('"')
	if err := sc.copyString(&raw, maxKeyBytes); err != nil {
		return "", err
	}
	value, err := pyjson.Decode(raw.Bytes())
	text, ok := value.(string)
	if err != nil || !ok {
		return "", refused("a key of the publish document is no valid string")
	}
	return text, nil
}

// copyString copies the rest of a string whose opening quote is taken, the
// closing quote included, at most limit bytes.
func (sc *scanner) copyString(out *bytes.Buffer, limit int) error {
	for {
		c, err := sc.take()
		if err != nil {
			return ended(err)
		}
		out.WriteByte(c)
		if out.Len() > limit {
			return refused("a string of the publish document is longer than " + strconv.Itoa(limit) + " bytes")
		}
		switch c {
		case '"':
			return nil
		case '\\':
			escaped, err := sc.take()
			if err != nil {
				return ended(err)
			}
			out.WriteByte(escaped)
		}
	}
}

// value reads one value and decodes it as json.loads does.
func (sc *scanner) value() (any, error) {
	raw, err := sc.capture()
	if err != nil {
		return nil, err
	}
	value, err := pyjson.Decode(raw)
	if err != nil {
		return nil, refused("a value of the publish document is not JSON: " + err.Error())
	}
	return value, nil
}

// capture copies the bytes of one value: a string, an array or an object
// with its nesting checked, or a scalar up to the byte that ends it.
func (sc *scanner) capture() ([]byte, error) {
	c, err := sc.next()
	if err != nil {
		return nil, ended(err)
	}
	var raw bytes.Buffer
	raw.WriteByte(c)
	switch c {
	case '"':
		if err := sc.copyString(&raw, MaxPublishDocument); err != nil {
			return nil, err
		}
		return raw.Bytes(), nil
	case '{', '[':
		if err := sc.copyContainer(&raw, c); err != nil {
			return nil, err
		}
		return raw.Bytes(), nil
	}
	for {
		peek, err := sc.r.Peek(1)
		if err != nil || space(peek[0]) || peek[0] == ',' || peek[0] == '}' || peek[0] == ']' {
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			return raw.Bytes(), nil
		}
		c, err := sc.take()
		if err != nil {
			return nil, ended(err)
		}
		raw.WriteByte(c)
	}
}

// copyContainer copies an array or an object whose opening bracket is
// copied, up to its closing bracket.
func (sc *scanner) copyContainer(raw *bytes.Buffer, open byte) error {
	closers := []byte{closer(open)}
	for len(closers) > 0 {
		c, err := sc.take()
		if err != nil {
			return ended(err)
		}
		raw.WriteByte(c)
		switch c {
		case '"':
			if err := sc.copyString(raw, MaxPublishDocument); err != nil {
				return err
			}
		case '{', '[':
			if len(closers) >= maxPublishDepth {
				return refused("the publish document is nested deeper than " + strconv.Itoa(maxPublishDepth) + " levels")
			}
			closers = append(closers, closer(c))
		case '}', ']':
			if c != closers[len(closers)-1] {
				return refused("the publish document closes a bracket it did not open")
			}
			closers = closers[:len(closers)-1]
		}
	}
	return nil
}

func closer(open byte) byte {
	if open == '{' {
		return '}'
	}
	return ']'
}

// attachments reads the object of _attachments: each attachment an object
// of content_type, data and length; the data streams to the writer of open.
func (sc *scanner) attachments(open func(name string) (io.Writer, error)) ([]Attachment, error) {
	if err := sc.expect('{', "the object of _attachments"); err != nil {
		return nil, err
	}
	var out []Attachment
	err := sc.members(func(name string) error {
		if err := sc.expect('{', "the object of an attachment"); err != nil {
			return err
		}
		w, err := open(name)
		if err != nil {
			return err
		}
		a := Attachment{Name: name, Length: -1}
		data := false
		err = sc.members(func(key string) error {
			switch key {
			case "data":
				if data {
					return refused("attachment " + strconv.Quote(name) + " holds data twice")
				}
				data = true
				if err := sc.expect('"', "the text of the data"); err != nil {
					return err
				}
				n, err := sc.streamData(w, name)
				a.Size = n
				return err
			case "content_type":
				value, err := sc.value()
				text, ok := value.(string)
				if err == nil && !ok {
					err = refused("content_type of attachment " + strconv.Quote(name) + " is not a text")
				}
				a.ContentType = text
				return err
			case "length":
				value, err := sc.value()
				n, ok := value.(int64)
				if err == nil && (!ok || n < 0) {
					err = refused("length of attachment " + strconv.Quote(name) + " is no count of bytes")
				}
				a.Length = n
				return err
			}
			_, err := sc.capture()
			return err
		})
		if err != nil {
			return err
		}
		if !data {
			return refused("attachment " + strconv.Quote(name) + " holds no data")
		}
		out = append(out, a)
		return nil
	})
	return out, err
}

// streamData decodes the base64 text of a string whose opening quote is
// taken into w and takes the string to its closing quote.
func (sc *scanner) streamData(w io.Writer, name string) (int64, error) {
	text := &stringReader{sc: sc}
	n, err := io.CopyBuffer(w, base64.NewDecoder(base64.StdEncoding, text), make([]byte, readBuffer))
	var corrupt base64.CorruptInputError
	if errors.As(err, &corrupt) {
		return n, refused("the data of attachment " + strconv.Quote(name) + " is not base64")
	}
	if err != nil {
		return n, err
	}
	if rest, err := io.Copy(io.Discard, text); err != nil || rest != 0 || !text.done {
		if err != nil {
			return n, err
		}
		return n, refused("the data of attachment " + strconv.Quote(name) + " goes on after its end")
	}
	return n, nil
}

// stringReader gives the characters of a JSON string with its escapes
// decoded, up to its closing quote, which it takes. Its bytes do not count
// against the budget of the scanner.
type stringReader struct {
	sc      *scanner
	pending []byte
	done    bool
	err     error
}

func (s *stringReader) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	for n < len(p) && !s.done {
		buffered := s.sc.r.Buffered()
		if buffered == 0 {
			if n > 0 {
				return n, nil
			}
			if _, err := s.sc.r.Peek(1); err != nil {
				s.err = ended(err)
				return 0, s.err
			}
			buffered = s.sc.r.Buffered()
		}
		chunk, _ := s.sc.r.Peek(buffered)
		i := 0
		for i < len(chunk) && n < len(p) {
			c := chunk[i]
			if c == '"' || c == '\\' || c < 0x20 {
				break
			}
			p[n] = c
			n++
			i++
		}
		_, _ = s.sc.r.Discard(i) // the bytes were peeked
		if i == len(chunk) || n == len(p) {
			continue
		}
		switch c := chunk[i]; {
		case c == '"':
			_, _ = s.sc.r.Discard(1)
			s.done = true
		case c < 0x20:
			s.err = refused("a string of the publish document holds a control character")
			return n, s.err
		default:
			_, _ = s.sc.r.Discard(1)
			decoded, err := s.escape()
			if err != nil {
				s.err = err
				return n, err
			}
			k := copy(p[n:], decoded)
			n += k
			s.pending = append(s.pending, decoded[k:]...)
		}
	}
	if n == 0 && s.done {
		return 0, io.EOF
	}
	return n, nil
}

// escape decodes one escape whose backslash is taken.
func (s *stringReader) escape() ([]byte, error) {
	c, err := s.sc.r.ReadByte()
	if err != nil {
		return nil, ended(err)
	}
	switch c {
	case '"', '\\', '/':
		return []byte{c}, nil
	case 'b':
		return []byte{'\b'}, nil
	case 'f':
		return []byte{'\f'}, nil
	case 'n':
		return []byte{'\n'}, nil
	case 'r':
		return []byte{'\r'}, nil
	case 't':
		return []byte{'\t'}, nil
	case 'u':
		var hex [4]byte
		if _, err := io.ReadFull(s.sc.r, hex[:]); err != nil {
			return nil, ended(err)
		}
		value, err := strconv.ParseUint(string(hex[:]), 16, 16)
		if err != nil {
			return nil, refused("a string of the publish document holds an escape that is no \\u and four hex digits")
		}
		return utf8.AppendRune(nil, rune(value)), nil
	}
	return nil, refused("a string of the publish document holds an unknown escape")
}
