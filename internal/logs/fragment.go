package logs

import "os"

// writeFile writes to an active file; a test replaces it by a writer that
// fails half way.
var writeFile = (*os.File).Write

// fragmentChunk is the most bytes cutFragment reads at a time.
const fragmentChunk = 64 << 10

// cutFragment cuts the active file f of size bytes back to its last line
// break, so a part of a line that a failed write or a crash left behind is
// not continued by the next line (finding 7a-4). It returns the new size.
func cutFragment(path string, f *os.File, size int64) (int64, error) {
	if size == 0 {
		return 0, nil
	}
	r, err := os.OpenFile(path, os.O_RDONLY|noFollow, 0) // #nosec G304 -- the active file of this package in the log directory of the operator.
	if err != nil {
		return size, err
	}
	defer func() { _ = r.Close() }() // read only
	end := size
	buf := make([]byte, fragmentChunk)
	for end > 0 {
		start := max(end-fragmentChunk, 0)
		chunk := buf[:end-start]
		if _, err := r.ReadAt(chunk, start); err != nil {
			return size, err
		}
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] == '\n' {
				return cutAt(f, size, start+int64(i)+1)
			}
		}
		end = start
	}
	return cutAt(f, size, 0)
}

// cutAt truncates f to keep bytes when it holds more.
func cutAt(f *os.File, size, keep int64) (int64, error) {
	if keep == size {
		return size, nil
	}
	if err := f.Truncate(keep); err != nil {
		return size, err
	}
	return keep, nil
}
