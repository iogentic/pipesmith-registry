package logs

import "unicode/utf8"

// The caps of the free text fields of a request line, in bytes of the value
// before escaping, the mark included (finding 7a-3). Path and query share
// targetCap, the path first. A field above its cap is cut at a character
// boundary and ends with cutMark.
const (
	targetCap = 8 << 10 // the path with its query
	agentCap  = 1 << 10 // the user agent
	smallCap  = 256     // every other text field
	cutMark   = "[cut]"
)

// capped is r with every text field cut to its cap.
func (r Request) capped() Request {
	r.Path = capText(r.Path, targetCap)
	if r.Query != "" {
		r.Query = capText(r.Query, max(targetCap-len(r.Path), len(cutMark)))
	}
	r.UserAgent = capText(r.UserAgent, agentCap)
	for _, field := range []*string{&r.ID, &r.Client, &r.Subject, &r.Realm, &r.Method, &r.Protocol, &r.ContentLength,
		&r.Repository, &r.Format} {
		*field = capText(*field, smallCap)
	}
	return r
}

// capText is text when it holds at most limit bytes, else its start up to a
// character boundary followed by cutMark, limit bytes at most.
func capText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit - len(cutMark)
	for i := end; i > 0 && i > end-utf8.UTFMax; i-- {
		if utf8.RuneStart(text[i]) {
			end = i
			break
		}
	}
	return text[:end] + cutMark
}
