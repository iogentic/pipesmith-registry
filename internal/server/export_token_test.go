package server

import (
	"net/http"
	"testing"
)

// The export with the subject of an npm token: the permission of a download
// decides as for Basic credentials, and the account of one export at a
// time is the same user, whether the request carries Basic credentials or a
// token of that user, so a second way to sign in gives no second export.
func TestExportWithAnNpmToken(t *testing.T) {
	a := newTokenApp(t, tokenSnapshot(t))
	token := a.npmToken(t, npmUser, npmPass)
	bearer := []string{"Authorization", "Bearer " + token}
	got := do(t, a.addr(), "GET", exportTarget("demo-npm", "form", "npm"), nil, bearer...)
	status(t, got, 200)
	if rows := readZip(t, got.body); len(rows) != 1 || rows[0].name != "widget/widget-1.2.3.tgz" {
		t.Fatalf("the export with the token holds %v", rows)
	}
	status(t, do(t, a.addr(), "GET", exportTarget("demo-maven", "dry", "1"), nil, bearer...), 403)
	status(t, do(t, a.addr(), "GET", exportTarget("demo-npm", "dry", "1"), nil), 401)

	byToken, _ := http.NewRequest("GET", "/api/export/demo-npm", nil)
	byToken.Header.Set("Authorization", "Bearer "+token)
	byBasic, _ := http.NewRequest("GET", "/api/export/demo-npm", nil)
	byBasic.SetBasicAuth(npmUser, npmPass)
	tokenKey, basicKey := a.srv.exportAccount(&request{r: byToken}), a.srv.exportAccount(&request{r: byBasic})
	if tokenKey == "" || tokenKey != basicKey {
		t.Fatalf("the account of the token is %q, of Basic credentials %q", tokenKey, basicKey)
	}
	// an export of the user with Basic credentials streams: the token of the
	// same user meets the bound of one export per account
	a.srv.exports.mu.Lock()
	a.srv.exports.running[basicKey] = true
	a.srv.exports.mu.Unlock()
	dry := dryOf(t, do(t, a.addr(), "GET", exportTarget("demo-npm", "form", "npm", "dry", "1"), nil, bearer...))
	if dry["busy"] != true || dry["own_export"] != true {
		t.Fatalf("dry with the token while the user exports: %v", dry)
	}
	status(t, do(t, a.addr(), "GET", exportTarget("demo-npm", "form", "npm"), nil, bearer...), 429)
	a.srv.exports.mu.Lock()
	delete(a.srv.exports.running, basicKey)
	a.srv.exports.mu.Unlock()
	status(t, do(t, a.addr(), "GET", exportTarget("demo-npm", "form", "npm"), nil, bearer...), 200)
}
