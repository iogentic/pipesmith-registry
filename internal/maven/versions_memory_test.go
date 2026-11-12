package maven

import (
	"runtime"
	"strings"
	"testing"
)

// allocated is the number of heap bytes fn allocates.
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestVersionOrderMemory: Python compares two versions at a time and keeps
// no token lists, so a version of many tokens costs memory in proportion to
// its text. The comparisons here allocate less than their input.
func TestVersionOrderMemory(t *testing.T) {
	long := strings.Repeat("1.", 200_000) + "x"
	for _, c := range []struct {
		name     string
		versions []string
	}{
		{"sort", []string{long, "2", long + "-A", "1." + long}},
		{"newest", []string{"2", long, long + ".0.1"}},
		{"compare", []string{long, long + ".0"}},
	} {
		input := 0
		for _, v := range c.versions {
			input += len(v)
		}
		var err error
		bytes := allocated(func() {
			switch c.name {
			case "sort":
				err = sortVersions(c.versions)
			case "newest":
				_, err = newest(c.versions)
			default:
				_, err = CompareVersions(c.versions[0], c.versions[1])
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		if bytes > uint64(input) {
			t.Errorf("%s allocates %d bytes for versions of %d bytes", c.name, bytes, input)
		}
	}
	allocs := testing.AllocsPerRun(3, func() { _, _ = CompareVersions(long, long+"-1") })
	if allocs > 50 {
		t.Errorf("CompareVersions makes %.0f allocations", allocs)
	}
}

// TestVersionOrderOfLongVersions: long runs of zeros against short
// versions, with the results compare_versions of the reference gave on
// 2026-11-12. Zeros before a word or at the end of a segment drop out, the
// others stay.
func TestVersionOrderOfLongVersions(t *testing.T) {
	zeros := strings.Repeat("0.", 50_000)
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1." + zeros + "1", "1.0.1", -1},
		{"1." + zeros + "1", "1.1", -1},
		{"1." + zeros + "a", "1-a", 0},
		{"1." + zeros + "a", "1.0.0.a", 0},
		{"1." + zeros + "0-1", "1-1", 0},
		{"1." + zeros + "0-0." + zeros + "2", "1-0.2", -1},
		{"1." + zeros + "0", "1", 0},
		{"1." + strings.Repeat("00.", 1000) + "1", "1.0.1", -1},
		{strings.Repeat("1.", 100_000) + "2", strings.Repeat("1.", 100_000) + "10", -1},
	} {
		got, err := CompareVersions(c.a, c.b)
		if err != nil || got != c.want {
			t.Errorf("CompareVersions(%.30q, %.30q) = %d, %v; want %d", c.a, c.b, got, err, c.want)
		}
		back, err := CompareVersions(c.b, c.a)
		if err != nil || back != -c.want {
			t.Errorf("CompareVersions(%.30q, %.30q) = %d, %v; want %d", c.b, c.a, back, err, -c.want)
		}
	}
}
