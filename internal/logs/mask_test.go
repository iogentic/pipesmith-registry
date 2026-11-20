package logs

import (
	"reflect"
	"slices"
	"testing"
)

// The values of token, password, secret, key and access_token become ***;
// the name match ignores case and decodes percent escapes once; order,
// separators, parameters without a value and repeated names stay.
func TestMaskQuery(t *testing.T) {
	for _, c := range []struct{ query, want string }{
		{"", ""},
		{"a=1&b=2", "a=1&b=2"},
		{"x=%20&y=1", "x=%20&y=1"},
		{"token=abc", "token=***"},
		{"password=p&secret=s&key=k&access_token=a", "password=***&secret=***&key=***&access_token=***"},
		{"TOKEN=abc;Password=p;sEcReT=s", "TOKEN=***;Password=***;sEcReT=***"},
		{"%74oken=t&pass%77ord=p&%53ECRET=s&k%65y=k&access%5Ftoken=a&%6b%45%79=k", "%74oken=***&pass%77ord=***&%53ECRET=***&k%65y=***&access%5Ftoken=***&%6b%45%79=***"},
		{"a=1&token=x&a=2&token=y;key=z", "a=1&token=***&a=2&token=***;key=***"},
		{"token&key=&flag", "token&key=***&flag"},
		{"&&token=x;;&", "&&token=***;;&"},
		{"token=a=b&c=d", "token=***&c=d"},
		{"q=token%3Dx&r=key", "q=token%3Dx&r=key"},
		{"tokens=x&mykey=y&key_id=z&access-token=w&token_=v", "tokens=x&mykey=y&key_id=z&access-token=w&token_=v"},
		{"%2574oken=x&%zzkey=y&ke%y=z&key%=w", "%2574oken=x&%zzkey=y&ke%y=z&key%=w"},
		{"=x&token", "=x&token"},
		{"\u212aey=kelvin&%E2%84%AAEY=kelvin", "\u212aey=***&%E2%84%AAEY=***"},
	} {
		if got := MaskQuery(c.query); got != c.want {
			t.Errorf("MaskQuery(%q) = %q, want %q", c.query, got, c.want)
		}
	}
}

// MaskTarget masks the query after the first ? of a target and leaves a
// target without one as it is.
func TestMaskTarget(t *testing.T) {
	for _, c := range []struct{ target, want string }{
		{"/api/health", "/api/health"},
		{"/api/health?", "/api/health?"},
		{"/api/health?demo=1", "/api/health?demo=1"},
		{"/repository/demo-raw/a%3Fb?key=k&demo", "/repository/demo-raw/a%3Fb?key=***&demo"},
		{"/a?token=x?key=y", "/a?token=***"},
	} {
		if got := MaskTarget(c.target); got != c.want {
			t.Errorf("MaskTarget(%q) = %q, want %q", c.target, got, c.want)
		}
	}
}

// Request holds no header but the user agent and the content length, so
// Authorization, Cookie and Set-Cookie cannot reach a line. A new field of
// Request must be added to this list.
func TestRequestHoldsNoOtherHeader(t *testing.T) {
	want := []string{"ID string", "Time time.Time", "Client string", "Subject string", "Realm string", "Method string",
		"Path string", "Query string", "Protocol string", "Status int", "ContentLength string", "RequestBytes int64",
		"ResponseBytes int64", "Duration time.Duration", "UserAgent string", "Repository string", "Format string", "Port int"}
	typ := reflect.TypeOf(Request{})
	var got []string
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name+" "+typ.Field(i).Type.String())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the fields of Request:\n%q\nwant\n%q", got, want)
	}
}
