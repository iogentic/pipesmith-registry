package export

import (
	"regexp"
	"strings"
	"testing"
)

// winDevice is the stem of a name that the Win32 layer opens as a device,
// after the rules of "Naming Files, Paths, and Namespaces" of Windows: the
// part before the first dot, with trailing spaces dropped, in any case.
var winDevice = regexp.MustCompile(`(?i)^(CON|PRN|AUX|NUL|CONIN\$|CONOUT\$|COM[0-9¹²³]|LPT[0-9¹²³])$`)

// winRead is the name Windows creates for an entry name, and false when it
// opens something else: a device, a drive or a stream. Each segment loses
// its trailing dots and spaces; a direction character is kept but shown
// reversed, so it counts as another reading.
func winRead(name string) (string, bool) {
	segments := strings.Split(name, "/")
	for i, segment := range segments {
		if strings.ContainsRune(segment, ':') {
			return "", false
		}
		stem := strings.TrimRight(strings.SplitN(segment, ".", 2)[0], " ")
		if winDevice.MatchString(stem) {
			return "", false
		}
		if strings.ContainsAny(segment, "‎‏‪‫‬‭‮⁦⁧⁨⁩؜") {
			return "", false
		}
		segments[i] = strings.TrimRight(segment, ". ")
	}
	return strings.Join(segments, "/"), true
}

// TestAcceptedNamesReadAsWrittenOnWindows stands in for an unpack on
// Windows: every name CheckEntryName lets into an archive is the name the
// Win32 rules create.
func TestAcceptedNamesReadAsWrittenOnWindows(t *testing.T) {
	names := []string{
		"docs/readme.txt", "docs/trailing.", "docs/trailing..", "docs/trailing ", "docs/dot. /x", "docs/CON",
		"docs/con.txt", "docs/Con .log", "docs/NUL.tar.gz", "docs/COM1", "docs/com9.x", "docs/LPT0", "docs/COM¹",
		"docs/LPT³.txt", "docs/CONIN$", "docs/conout$.x", "aux/readme.txt", "prn", "docs/doc.pdf:evil.exe",
		"docs/C:evil.dll", "C:evil.dll", "docs/a‮txt.exe", "docs/a⁦b", "docs/CONSOLE", "docs/COM10",
		"docs/nul-device.txt", "docs/auxiliary", "docs/.hidden", "docs/a b/c", "docs/x.y.z",
	}
	for _, c := range loadNameVectors(t).Entries {
		names = append(names, c.Name)
	}
	accepted, refused := 0, 0
	for _, name := range names {
		if CheckEntryName(name) != nil {
			refused++
			continue
		}
		accepted++
		if read, ok := winRead(name); !ok || read != name {
			t.Errorf("CheckEntryName(%q) accepts a name Windows reads as %q (same name: %v)", name, read, ok)
		}
	}
	if accepted < 10 || refused < 20 {
		t.Fatalf("%d names accepted and %d refused; the list lost its cases", accepted, refused)
	}
}
