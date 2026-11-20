package proxy

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The tag lists of the Docker proxies and the tokens of a client are kept
// per name a client chooses, so both have bounds: a loop over new names
// keeps memory flat.
func TestTagListsAreBounded(t *testing.T) {
	p := New(Config{})
	start := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for i := range maxTagLists + 50 {
		p.keepTags(tagKey{"demo-docker-proxy", fmt.Sprintf("demo/name-%d", i)},
			&tagList{tags: []string{"1.0"}, found: true, checked: start.Add(time.Duration(i) * time.Second)})
	}
	if n := len(p.tagLists); n != maxTagLists {
		t.Fatalf("%d tag lists kept, want %d", n, maxTagLists)
	}
	if p.keptTags(tagKey{"demo-docker-proxy", "demo/name-0"}) != nil || p.keptTags(tagKey{"demo-docker-proxy",
		fmt.Sprintf("demo/name-%d", maxTagLists+49)}) == nil {
		t.Fatal("the oldest list is kept or the newest is not")
	}
	// the bytes of the lists have a bound too
	big := make([]string, 64)
	for i := range big {
		big[i] = strings.Repeat("t", 64<<10)
	}
	for i := range 40 {
		p.keepTags(tagKey{"demo-docker-proxy", fmt.Sprintf("demo/big-%d", i)}, &tagList{tags: big, found: true, checked: start.Add(time.Hour)})
	}
	if p.tagBytes > maxTagListTotal {
		t.Fatalf("%d bytes of tag lists kept, the bound is %d", p.tagBytes, maxTagListTotal)
	}
	if p.tagBytes != countTagBytes(p) {
		t.Fatalf("the count of %d bytes differs from the lists, %d", p.tagBytes, countTagBytes(p))
	}
}

func countTagBytes(p *Proxies) int {
	n := 0
	for _, list := range p.tagLists {
		n += list.size()
	}
	return n
}

func TestTokensAreBounded(t *testing.T) {
	tk := newTokens()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for i := range maxTokens * 3 {
		c := challenge{realm: "https://auth.example.test/token", service: "demo", scope: fmt.Sprintf("repository:demo/name-%d:pull", i)}
		tk.keep(c, fmt.Sprintf("demo/name-%d", i), token{value: "demo-token", until: now.Add(time.Minute)})
	}
	if len(tk.byScope) > maxTokens || len(tk.byName) > maxTokens {
		t.Fatalf("%d tokens and %d names kept, the bound is %d", len(tk.byScope), len(tk.byName), maxTokens)
	}
	last := challenge{realm: "https://auth.example.test/token", service: "demo", scope: fmt.Sprintf("repository:demo/name-%d:pull", maxTokens*3-1)}
	if tk.forChallenge(last, now, "") != "demo-token" {
		t.Fatal("the newest token is not kept")
	}
}
