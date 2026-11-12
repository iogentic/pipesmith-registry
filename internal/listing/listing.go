// Package listing renders the HTML folder listing of a repository path, as
// listing.py of the Python server does, byte for byte.
//
// Texts are Python str values in the form of internal/pystr: UTF-8 with a
// lone surrogate in its three byte form.
package listing

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/pystr"
)

const style = `:root { color-scheme: light dark; --bg: #ffffff; --fg: #1b1f24; --muted: #59636e; --line: #d1d9e0; --link: #0550ae; }
@media (prefers-color-scheme: dark) {
  :root { --bg: #0d1117; --fg: #e6edf3; --muted: #9198a1; --line: #30363d; --link: #79c0ff; }
}
body { margin: 0; padding: 24px; background: var(--bg); color: var(--fg);
  font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
h1 { font-size: 18px; margin: 0 0 4px; overflow-wrap: anywhere; }
p { margin: 0 0 16px; color: var(--muted); }
p.pager { margin: 16px 0 0; }
table { border-collapse: collapse; width: 100%; }
th, td { text-align: left; padding: 4px 12px 4px 0; border-bottom: 1px solid var(--line); vertical-align: top; }
th { color: var(--muted); font-weight: normal; }
td.size, th.size { text-align: right; }
a { color: var(--link); text-decoration: none; overflow-wrap: anywhere; }
a:hover { text-decoration: underline; }`

// escaper is html.escape with quote=True.
var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;")

func escape(s string) string {
	return escaper.Replace(s)
}

// The seconds datetime accepts: 0001-01-01 00:00:00 to 9999-12-31 23:59:59.
const (
	minSeconds = -62135596800
	maxSeconds = 253402300799
)

// formatTime is _time: the date in UTC, "" for None and for a time
// datetime cannot hold. For a whole number of milliseconds the float
// division of Python floors to the second within that range.
func formatTime(createdMS *int64) string {
	if createdMS == nil {
		return ""
	}
	seconds := *createdMS / 1000
	if *createdMS%1000 < 0 {
		seconds--
	}
	if seconds < minSeconds || seconds > maxSeconds {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format("2006-01-02 15:04:05") + " UTC"
}

func row(href, label, modified, size string) string {
	return `<tr><td><a href="` + escape(href) + `">` + escape(label) + `</a></td>` +
		"<td>" + escape(modified) + `</td><td class="size">` + escape(size) + "</td></tr>"
}

func alwaysSafe(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-' ||
		c == '~'
}

// quote is urllib.parse.quote(name, safe=""): the UTF-8 bytes with every
// byte but letters, digits and "_.-~" written as %XX. A lone surrogate
// raises UnicodeEncodeError, as str.encode("utf-8") does.
func quote(name string) (string, error) {
	runes := pystr.Runes(name)
	for i, r := range runes {
		if r < 0xD800 || r > 0xDFFF {
			continue
		}
		end := i + 1
		for end < len(runes) && runes[end] >= 0xD800 && runes[end] <= 0xDFFF {
			end++
		}
		message := fmt.Sprintf("'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed",
			r, i)
		if end-i > 1 {
			message = fmt.Sprintf("'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed",
				i, end-1)
		}
		return "", &pyjson.Error{Class: "UnicodeEncodeError", Message: message}
	}
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		if c := name[i]; alwaysSafe(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String(), nil
}

// Render is render(): a page "Index of /<directory>" with relative links,
// one row per child. offset is the position of the first child (nil for
// 0), nextOffset the position of the next page when one follows. The error
// is the UnicodeEncodeError of a child name with a lone surrogate.
func Render(repo, directory string, children []index.Child, offset, nextOffset *big.Int) (string, error) {
	directory = strings.Trim(directory, "/")
	title := "Index of /" + directory
	var pager strings.Builder
	if offset != nil && offset.Sign() > 0 {
		first := new(big.Int).Add(offset, big.NewInt(1))
		pager.WriteString(`<p class="pager">Entries from number ` + first.String() + `. <a href="./">First page</a></p>` +
			"\n")
	}
	if nextOffset != nil {
		pager.WriteString(`<p class="pager">More entries follow. <a href="?offset=` + nextOffset.String() +
			`">Next page</a></p>` + "\n")
	}
	var rows []string
	if directory != "" {
		rows = append(rows, row("../", "../", "", ""))
	}
	for _, child := range children {
		link, err := quote(child.Name)
		if err != nil {
			return "", err
		}
		if child.IsDir {
			rows = append(rows, row(link+"/", child.Name+"/", "", ""))
			continue
		}
		size := ""
		if child.Size != nil {
			size = strconv.FormatInt(*child.Size, 10)
		}
		rows = append(rows, row(link, child.Name, formatTime(child.CreatedMS), size))
	}
	return "<!DOCTYPE html>\n" +
		`<html lang="en">` + "\n" +
		"<head>\n" +
		`<meta charset="utf-8">` + "\n" +
		`<meta name="viewport" content="width=device-width, initial-scale=1">` + "\n" +
		"<title>" + escape(title) + "</title>\n" +
		"<style>\n" + style + "\n</style>\n" +
		"</head>\n" +
		"<body>\n" +
		"<h1>" + escape(title) + "</h1>\n" +
		"<p>Repository " + escape(repo) + "</p>\n" +
		"<table>\n" +
		`<thead><tr><th>Name</th><th>Last modified</th><th class="size">Size</th></tr></thead>` + "\n" +
		"<tbody>\n" + strings.Join(rows, "\n") + "\n</tbody>\n" +
		"</table>\n" +
		pager.String() +
		"</body>\n" +
		"</html>\n", nil
}
