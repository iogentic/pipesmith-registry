package catalog

import (
	"bufio"
	"cmp"
	"container/heap"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The staging of a fill is an external sort: the entries of a scan go into
// sorted runs of runEntries each, files in a folder of the data directory,
// and Commit merges the runs. Its cost grows with the entries times the log
// of their count; a B-tree that took them one by one in the order of the
// walk would slow down once it outgrew its cache.

// runEntries is the size of a run; a variable so that a test can make many runs.
var runEntries = 1 << 17

// staged is one entry of a scan; seq is its place in the walk.
type staged struct {
	blob
	seq uint64
}

// compareStaged orders by key, then the newer blob first, then the earlier
// entry: the first entry of a key is the one the index keeps, the newer
// blob, on a tie the first one.
func compareStaged(a, b *staged) int {
	if c := cmp.Compare(a.repo, b.repo); c != 0 {
		return c
	}
	if c := cmp.Compare(a.path, b.path); c != 0 {
		return c
	}
	if c := cmp.Compare(b.created, a.created); c != 0 {
		return c
	}
	return cmp.Compare(a.seq, b.seq)
}

type staging struct {
	dir  string
	buf  []staged
	runs []string
	seq  uint64
}

func (s *staging) add(e stores.Entry) error {
	s.seq++
	s.buf = append(s.buf, staged{blob: blob{key: key{e.Repo, e.Path}, size: e.Size, created: e.CreatedMS, sha1: pointer(e.SHA1),
		contentType: pointer(e.ContentType), uploader: pointer(e.Uploader), address: pointer(e.UploaderIP)}, seq: s.seq})
	if len(s.buf) < runEntries {
		return nil
	}
	return s.spill()
}

// spill writes the entries in memory as a sorted run.
func (s *staging) spill() error {
	slices.SortFunc(s.buf, func(a, b staged) int { return compareStaged(&a, &b) })
	name := filepath.Join(s.dir, fmt.Sprintf("run-%05d", len(s.runs)))
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a run in the staging folder of this fill.
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	var scratch [binary.MaxVarintLen64]byte
	for i := range s.buf {
		writeStaged(w, scratch[:], &s.buf[i])
	}
	err = w.Flush()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	s.runs = append(s.runs, name)
	clear(s.buf)
	s.buf = s.buf[:0]
	return nil
}

func writeText(w *bufio.Writer, scratch []byte, text string) {
	_, _ = w.Write(scratch[:binary.PutUvarint(scratch, uint64(len(text)))]) // an error of the writer stays and Flush returns it
	_, _ = w.WriteString(text)
}

func writeNull(w *bufio.Writer, scratch []byte, text sql.NullString) {
	if !text.Valid {
		_ = w.WriteByte(0) // as above
		return
	}
	_ = w.WriteByte(1) // as above
	writeText(w, scratch, text.String)
}

func writeStaged(w *bufio.Writer, scratch []byte, e *staged) {
	writeText(w, scratch, e.repo)
	writeText(w, scratch, e.path)
	_, _ = w.Write(scratch[:binary.PutVarint(scratch, e.size)]) // as above
	_, _ = w.Write(scratch[:binary.PutVarint(scratch, e.created)])
	_, _ = w.Write(scratch[:binary.PutUvarint(scratch, e.seq)])
	for _, text := range []sql.NullString{e.sha1, e.contentType, e.uploader, e.address} {
		writeNull(w, scratch, text)
	}
}

func readText(r *bufio.Reader) (string, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return "", err
	}
	if n > uint64(r.Size()) { // #nosec G115 -- the size of a buffer is positive.
		buf := make([]byte, n)
		_, err := io.ReadFull(r, buf)
		return string(buf), err
	}
	peeked, err := r.Peek(int(n)) // #nosec G115 -- at most the size of the buffer, checked above.
	if err != nil {
		return "", err
	}
	text := string(peeked)
	_, err = r.Discard(len(peeked))
	return text, err
}

func readNull(r *bufio.Reader) (sql.NullString, error) {
	flag, err := r.ReadByte()
	if err != nil || flag == 0 {
		return sql.NullString{}, err
	}
	text, err := readText(r)
	return sql.NullString{String: text, Valid: true}, err
}

// readStaged reads one entry of a run; io.EOF at the end of the run.
func readStaged(r *bufio.Reader) (staged, error) {
	var e staged
	var err error
	if e.repo, err = readText(r); err != nil {
		return e, err
	}
	if e.path, err = readText(r); err == nil {
		e.size, err = binary.ReadVarint(r)
	}
	if err == nil {
		e.created, err = binary.ReadVarint(r)
	}
	if err == nil {
		e.seq, err = binary.ReadUvarint(r)
	}
	for _, text := range []*sql.NullString{&e.sha1, &e.contentType, &e.uploader, &e.address} {
		if err == nil {
			*text, err = readNull(r)
		}
	}
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF // only a cut run ends inside an entry
	}
	return e, err
}

// cursor is the head of one sorted run.
type cursor struct {
	head staged
	next func() (staged, bool, error)
}

type cursors []*cursor

func (c cursors) Len() int           { return len(c) }
func (c cursors) Less(i, j int) bool { return compareStaged(&c[i].head, &c[j].head) < 0 }
func (c cursors) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }
func (c *cursors) Push(x any)        { *c = append(*c, x.(*cursor)) }
func (c *cursors) Pop() any {
	old := *c
	last := old[len(old)-1]
	*c = old[:len(old)-1]
	return last
}

// merged gives the entries of all runs in key order, the entry the index
// keeps once per key. It is a rowSource.
type merged struct {
	heap  cursors
	files []*os.File
	last  key
	any   bool
}

// source sorts the entries in memory as the last run and opens the runs on
// the disk for the merge.
func (s *staging) source() (*merged, error) {
	m := &merged{}
	slices.SortFunc(s.buf, func(a, b staged) int { return compareStaged(&a, &b) })
	mem := s.buf
	if err := m.add(func() (staged, bool, error) {
		if len(mem) == 0 {
			return staged{}, false, nil
		}
		e := mem[0]
		mem = mem[1:]
		return e, true, nil
	}); err != nil {
		return nil, err
	}
	for _, name := range s.runs {
		f, err := os.Open(name) // #nosec G304 -- a run this fill wrote in its staging folder.
		if err != nil {
			return nil, errors.Join(err, m.close())
		}
		m.files = append(m.files, f)
		r := bufio.NewReaderSize(f, 1<<17)
		if err := m.add(func() (staged, bool, error) {
			e, err := readStaged(r)
			if errors.Is(err, io.EOF) {
				return staged{}, false, nil
			}
			return e, err == nil, err
		}); err != nil {
			return nil, errors.Join(err, m.close())
		}
	}
	heap.Init(&m.heap)
	return m, nil
}

// add puts a run with its first entry on the heap; an empty run stays off.
func (m *merged) add(next func() (staged, bool, error)) error {
	head, ok, err := next()
	if err != nil || !ok {
		return err
	}
	m.heap = append(m.heap, &cursor{head: head, next: next})
	return nil
}

func (m *merged) next() (*blob, error) {
	for len(m.heap) > 0 {
		top := m.heap[0]
		e := top.head
		head, ok, err := top.next()
		if err != nil {
			return nil, err
		}
		if ok {
			top.head = head
			heap.Fix(&m.heap, 0)
		} else {
			heap.Pop(&m.heap)
		}
		if m.any && e.key == m.last {
			continue // the first entry of the key won
		}
		m.last, m.any = e.key, true
		return &e.blob, nil
	}
	return nil, nil
}

func (m *merged) close() error {
	var errs []error
	for _, f := range m.files {
		errs = append(errs, f.Close())
	}
	m.files = nil
	return errors.Join(errs...)
}
