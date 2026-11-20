// Package export writes the archives of the export of a folder: a zip of
// stored files and an OCI image layout as a tar. It knows nothing of the
// server or the index; the caller lists the entries and opens their bytes.
//
// Entry names come from index paths, and the unpacking tool of a user trusts
// them, so every name passes CheckEntryName before the first byte is written.
package export

import (
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// BadNameError is a stored path that no archive entry may carry.
type BadNameError struct {
	Name   string
	Reason string
}

func (e *BadNameError) Error() string {
	return fmt.Sprintf("the stored path %q cannot be an archive entry: %s", e.Name, e.Reason)
}

// CheckEntryName refuses a name that could lead out of the folder an
// archive is unpacked in, or that a tool reads in another way than it is
// written: an empty, "." or ".." segment, a leading "/" or a trailing one, a
// backslash, a drive letter as the first segment, a control character,
// bytes that are no UTF-8, and what Windows reads in another way: a segment
// that ends with a dot or a space, a colon, a device name, and a direction
// character, which a file manager shows as a reversed name.
func CheckEntryName(name string) error {
	refuse := func(reason string) error { return &BadNameError{Name: name, Reason: reason} }
	if !utf8.ValidString(name) {
		return refuse("it is not UTF-8")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return refuse("it holds a control character")
		}
	}
	if strings.Contains(name, `\`) {
		return refuse("it holds a backslash")
	}
	if strings.HasPrefix(name, "/") {
		return refuse("it is absolute")
	}
	segments := strings.Split(name, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return refuse("it holds an empty, '.' or '..' segment")
		}
	}
	if first := segments[0]; len(first) >= 2 && first[1] == ':' && (first[0]|0x20) >= 'a' && (first[0]|0x20) <= 'z' {
		return refuse("it starts with a drive letter")
	}
	for _, segment := range segments {
		switch {
		case strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " "):
			return refuse("a segment ends with a dot or a space, which Windows drops")
		case strings.Contains(segment, ":"):
			return refuse("it holds a colon, which Windows reads as a drive or a stream")
		case reservedOnWindows(segment):
			return refuse("a segment is a device name of Windows")
		case strings.ContainsFunc(segment, func(r rune) bool { return unicode.Is(unicode.Bidi_Control, r) }):
			return refuse("it holds a direction character")
		}
	}
	return nil
}

// reservedOnWindows reports a segment that Windows opens as a device: CON,
// PRN, AUX, NUL, CONIN$, CONOUT$, COM and LPT with a digit or a superscript
// one to three, in any case, also with an extension or spaces before it.
func reservedOnWindows(segment string) bool {
	stem, _, _ := strings.Cut(segment, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	for _, prefix := range []string{"COM", "LPT"} {
		if rest, ok := strings.CutPrefix(stem, prefix); ok {
			if r, size := utf8.DecodeRuneInString(rest); size == len(rest) && (r >= '0' && r <= '9' || r == '¹' || r == '²' || r == '³') {
				return true
			}
		}
	}
	return false
}

// safeName keeps letters, digits, ".", "_" and "-" of ASCII; every other
// character becomes "_". The result can stand in a header without quoting
// trouble.
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ArchiveName is "<repository>-<name of the folder, or root>.zip".
func ArchiveName(repository, folder string) string {
	folder = strings.Trim(folder, "/")
	last := "root"
	if folder != "" {
		last = path.Base(folder)
	}
	return safeName(repository+"-"+last) + ".zip"
}

// ImageArchiveName is "<image with _ for />-<tag, or the first 12 hex
// digits of the digest>.tar".
func ImageArchiveName(image, reference string) string {
	if hex, ok := strings.CutPrefix(reference, "sha256:"); ok && len(hex) >= 12 {
		reference = hex[:12]
	}
	return safeName(strings.ReplaceAll(image, "/", "_")+"-"+reference) + ".tar"
}

// NpmEntryName is the entry of a package tarball below the path of its
// package, so that two packages with a tarball of one file name stay apart:
// "<name>/-/<file>.tgz" gives "<name>/<file>.tgz" and
// "@<scope>/<name>/-/<file>.tgz" gives "@<scope>/<name>/<file>.tgz". Any
// other path is no tarball.
func NpmEntryName(p string) (string, bool) {
	parts := strings.Split(p, "/")
	if !strings.HasSuffix(p, ".tgz") {
		return "", false
	}
	switch {
	case len(parts) == 3 && parts[1] == "-" && !strings.HasPrefix(parts[0], "@") && parts[0] != "-":
		return parts[0] + "/" + parts[2], true
	case len(parts) == 4 && parts[2] == "-" && strings.HasPrefix(parts[0], "@") && len(parts[0]) > 1:
		return parts[0] + "/" + parts[1] + "/" + parts[3], true
	}
	return "", false
}

// UniqueNames reports the first name an unpack cannot hold beside the
// others, false with it: a name that repeats, or a name that is a folder of
// another entry ("a" beside "a/b", in either order).
func UniqueNames(names []string) (string, bool) {
	files := make(map[string]bool, len(names))
	folders := map[string]bool{}
	for _, name := range names {
		if files[name] || folders[name] {
			return name, false
		}
		for i := strings.IndexByte(name, '/'); i >= 0; i = nextSlash(name, i) {
			if files[name[:i]] {
				return name[:i], false
			}
			folders[name[:i]] = true
		}
		files[name] = true
	}
	return "", true
}

func nextSlash(name string, at int) int {
	if j := strings.IndexByte(name[at+1:], '/'); j >= 0 {
		return at + 1 + j
	}
	return -1
}

// storedEndings are the endings whose bytes are compressed already; a zip
// stores them without compression.
var storedEndings = map[string]bool{
	"jar": true, "war": true, "ear": true, "aar": true, "zip": true, "gz": true, "tgz": true, "bz2": true, "xz": true,
	"zst": true, "7z": true, "png": true, "jpg": true, "jpeg": true, "gif": true, "webp": true, "woff": true, "woff2": true,
	"nupkg": true, "whl": true, "apk": true, "deb": true, "rpm": true,
}

// Stored reports whether a zip entry of this name is stored without
// compression; the ending is compared without regard to case.
func Stored(name string) bool {
	base := path.Base(name)
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 {
		return false
	}
	return storedEndings[strings.ToLower(base[dot+1:])]
}
