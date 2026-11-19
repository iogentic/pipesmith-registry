package index

import (
	"container/heap"
	"database/sql"
	"errors"
	"strings"
)

// ErrStop ends WalkPrefix early without an error.
var ErrStop = errors.New("stop the walk")

// walkStream is one sorted cursor of WalkPrefix: the files of one store
// below the directory, ordered by parent and name.
type walkStream struct {
	rows         *sql.Rows
	rank         int // position of the store in name order
	asset        Asset
	parent, name string
}

func (s *walkStream) next(repo string) (bool, error) {
	if !s.rows.Next() {
		return false, s.rows.Err()
	}
	var parent, name string
	var sha1, contentType sql.NullString
	a := Asset{Repo: repo, Store: s.asset.Store}
	if err := s.rows.Scan(&parent, &name, &a.Location, &a.Size, &sha1, &contentType, &a.CreatedMS); err != nil {
		return false, err
	}
	a.Path = name
	if parent != "" {
		a.Path = parent + "/" + name
	}
	a.SHA1, a.ContentType = optional(sha1), optional(contentType)
	s.asset, s.parent, s.name = a, parent, name
	return true, nil
}

// walkMerge orders the heads by parent, name, the newer row first, then the
// store whose name sorts first: the tie rule of Lookup.
type walkMerge []*walkStream

func (m walkMerge) Len() int { return len(m) }
func (m walkMerge) Less(i, j int) bool {
	a, b := m[i], m[j]
	if a.parent != b.parent {
		return a.parent < b.parent
	}
	if a.name != b.name {
		return a.name < b.name
	}
	if a.asset.CreatedMS != b.asset.CreatedMS {
		return a.asset.CreatedMS > b.asset.CreatedMS
	}
	return a.rank < b.rank
}
func (m walkMerge) Swap(i, j int) { m[i], m[j] = m[j], m[i] }
func (m *walkMerge) Push(x any)   { *m = append(*m, x.(*walkStream)) }
func (m *walkMerge) Pop() any {
	old := *m
	last := old[len(old)-1]
	*m = old[:len(old)-1]
	return last
}

// WalkPrefix calls fn with the winning row of every path below the
// directory, over all stores, in one read transaction, ordered by parent
// and then by name. The directory is taken without leading and trailing
// slashes; empty is the root of the repository. fn ends the walk with
// ErrStop, which WalkPrefix does not return; any other error of fn is
// returned as it is.
func (ix *Index) WalkPrefix(repo, directory string, fn func(Asset) error) error {
	return ix.WalkPrefixEnding(repo, directory, "", fn)
}

// WalkPrefixEnding is WalkPrefix over the rows whose name ends with ending;
// the store leaves out every other row before fn sees it. An empty ending
// takes every row.
func (ix *Index) WalkPrefixEnding(repo, directory, ending string, fn func(Asset) error) error {
	directory = strings.Trim(directory, "/")
	err := ix.read(func(tx *sql.Tx) error {
		list, err := storeList(tx)
		if err != nil {
			return err
		}
		var opened []*walkStream
		defer func() {
			for _, s := range opened {
				_ = s.rows.Close() // cleanup of the merge streams
			}
		}()
		for rank, s := range list {
			query, args := walkQuery(filesTable(s.id), repo, directory, ending)
			rows, err := tx.QueryContext(bg, query, args...)
			if err != nil {
				return err
			}
			opened = append(opened, &walkStream{rows: rows, rank: rank, asset: Asset{Store: s.name}})
		}
		heads := walkMerge{}
		for _, s := range opened {
			ok, err := s.next(repo)
			if err != nil {
				return err
			}
			if ok {
				heads = append(heads, s)
			}
		}
		heap.Init(&heads)
		previousParent, previousName, first := "", "", true
		for heads.Len() > 0 {
			s := heads[0]
			asset, parent, name := s.asset, s.parent, s.name
			ok, err := s.next(repo)
			if err != nil {
				return err
			}
			if ok {
				heap.Fix(&heads, 0)
			} else {
				heap.Pop(&heads)
			}
			if !first && parent == previousParent && name == previousName {
				continue // the first row of a path is the winner
			}
			first, previousParent, previousName = false, parent, name
			if err := fn(asset); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, ErrStop) {
		return nil
	}
	return err
}

// walkQuery is the statement of WalkPrefixEnding for the files table of one
// store, with its values, ordered by parent and name.
func walkQuery(table, repo, directory, ending string) (string, []any) {
	const columns = "SELECT parent, name, location, size, sha1, content_type, created_ms FROM "
	nameRule, names := "", []any{}
	if ending != "" {
		nameRule, names = " AND name GLOB ?", []any{"*" + globLiteral(ending)}
	}
	if directory == "" {
		return columns + table + " WHERE repo = ?" + nameRule + " ORDER BY parent, name", append([]any{repo}, names...) // #nosec G202 -- the table name is filesTable of an integer store id and nameRule a constant; every value is a placeholder.
	}
	// parent is the directory, or lies in the range of the directory and
	// "/": "0" is the byte after "/". Each part is a search of the primary
	// key (repo, parent, name) with parent bound; an OR of both would let
	// SQLite bind repo only and read the whole repository.
	exact := columns + table + " WHERE repo = ? AND parent = ?" + nameRule                 // #nosec G202 -- the table name is filesTable of an integer store id and nameRule a constant; every value is a placeholder.
	below := columns + table + " WHERE repo = ? AND parent >= ? AND parent < ?" + nameRule // #nosec G202 -- the table name is filesTable of an integer store id and nameRule a constant; every value is a placeholder.
	args := append([]any{repo, directory}, names...)
	args = append(append(args, repo, directory+"/", directory+"0"), names...)
	return exact + " UNION ALL " + below + " ORDER BY parent, name", args
}

// globLiteral quotes the characters GLOB reads as patterns, each in a
// bracket of its own, so the text matches itself.
func globLiteral(text string) string {
	var b strings.Builder
	for _, r := range text {
		switch r {
		case '*', '?', '[':
			b.WriteByte('[')
			b.WriteRune(r)
			b.WriteByte(']')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
