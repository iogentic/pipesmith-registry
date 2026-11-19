package npm

import "math/big"

// Bounds of one archive beyond the rules of tarfile. An archive over a bound
// is unreadable: the ReadError of tarfile, which package_manifest catches.
const maxHeaderData = 1 << 20 // pax data, GNU long name or long link, sparse map

// maxInflated is the most bytes read from the gzip stream of one archive,
// rereads after a seek backwards included. A variable for the tests.
var maxInflated int64 = 1 << 30

func overBound(what string) error {
	return newError("ReadError", what+" is over its bound")
}

// headerData checks the length of the data of an extended header.
func headerData(n *big.Int) error {
	if n.Cmp(big.NewInt(maxHeaderData)) > 0 {
		return overBound("the data of an extended header")
	}
	return nil
}

// pull is one read of the gzip stream, counted against maxInflated.
func (s *stream) pull(p []byte) (int, error) {
	k, err := s.z.Read(p)
	s.inflated += int64(k)
	if s.inflated > maxInflated {
		return k, overBound("the inflated archive")
	}
	if s.budget != nil {
		s.budget.left -= int64(k)
		if s.budget.left < 0 {
			return k, overBound("the inflated archives of one package document")
		}
	}
	return k, err
}

// maxHeaderTotal is the most data of extended headers, GNU long names and
// long links and sparse maps of one archive in all.
const maxHeaderTotal = 4 << 20

// maxDocumentInflated is the most bytes inflated over all archives read
// for one package document. A variable for the tests.
var maxDocumentInflated int64 = 4 << 30

// inflateBudget is what is left of maxDocumentInflated for one document.
type inflateBudget struct {
	left int64
}

// spend counts n bytes of header data of the archive against maxHeaderTotal.
func (w *walker) spend(n int64) error {
	w.headerTotal += n
	if w.headerTotal > maxHeaderTotal {
		return overBound("the header data of the archive")
	}
	return nil
}
