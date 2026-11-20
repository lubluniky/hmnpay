package endpointid

import "testing"

func TestIdentity(t *testing.T) {
	one := [][]string{
		{"https://abc.example.org", "https://ABC.example.org:443", "https://abc.example.org.", "https://abc.example.org/v1/KEY", "https://user:pw@abc.example.org:0443/?k=1"},
		{"https://bücher.example", "https://xn--bcher-kva.example", "https://BÜCHER.example."},
		{"https://[2001:db8::1]", "https://[2001:0db8::1]", "https://[2001:db8:0:0:0:0:0:1]:443"},
		{"http://192.0.2.7", "http://[::ffff:192.0.2.7]", "http://192.0.2.7:80"},
	}
	for _, group := range one {
		for _, u := range group[1:] {
			if Identity(u) != Identity(group[0]) {
				t.Errorf("%s is %s, %s is %s: one endpoint", group[0], Identity(group[0]), u, Identity(u))
			}
		}
	}
	two := [][2]string{
		{"https://abc.example.org", "http://abc.example.org"},
		{"https://abc.example.org", "https://abc.example.org:444"},
		{"https://abc.example.org", "https://abd.example.org"},
	}
	for _, p := range two {
		if Identity(p[0]) == Identity(p[1]) {
			t.Errorf("%s and %s are two endpoints, both %s", p[0], p[1], Identity(p[0]))
		}
	}
	// RFC 3492 sample strings.
	for in, want := range map[string]string{"bücher": "bcher-kva", "münchen": "mnchen-3ya", "日本語": "wgv71a119e", "ü": "tda"} {
		if got, ok := punycode(in); !ok || got != want {
			t.Errorf("punycode(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Identity("not a url"); got != "invalid-url" {
		t.Errorf("Identity of no URL: %q", got)
	}
}
