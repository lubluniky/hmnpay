package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The Host rule (DNS-rebinding protection) covers every path the server
// answers, not only the API: a page of the interface asked for under a
// foreign host name is refused before the interface is reached.
func TestHostRuleCoversTheInterface(t *testing.T) {
	served := 0
	ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("interface: " + r.URL.Path))
	})
	e := newTenv(t, withOpts(func(o *Options) { o.Fallback = ui; o.AllowedHosts = []string{"internal.example"} }))
	get := func(path, host string) (int, string, http.Header) {
		t.Helper()
		w := e.do(req{method: "GET", path: path, h: hdr{"Host": host}})
		return w.Code, w.Body.String(), w.Header()
	}
	public := strings.TrimPrefix(testOrigin, "https://")

	for _, path := range []string{"/", "/login", "/payments", "/pay/pay_00000000000000000000000000", "/_next/static/chunks/app.js", "/anything/else"} {
		// A foreign name: refused, with the same answer as the API gives,
		// and the interface never sees the request.
		before := served
		code, body, h := get(path, "rebind.attacker.example")
		if code != http.StatusMisdirectedRequest || served != before {
			t.Errorf("GET %s with a foreign Host: %d (interface reached: %v), want 421 without reaching it", path, code, served != before)
		}
		if !strings.Contains(body, "host_not_allowed") || h.Get("X-Request-Id") == "" || h.Get("X-Frame-Options") != "DENY" || h.Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s with a foreign Host: body %q, headers %v", path, body, h)
		}
		// The names the server answers for reach it, as before.
		for _, host := range []string{public, "localhost:8080", "127.0.0.1:8080", "[::1]:8080", "192.168.1.20", "internal.example", "INTERNAL.example.:443"} {
			before := served
			if code, body, _ := get(path, host); code != http.StatusOK || served != before+1 || body != "interface: "+path {
				t.Errorf("GET %s with Host %q: %d %q (interface reached: %v)", path, host, code, body, served != before)
			}
		}
	}
	// The API and the health check are as they were.
	if code, _, _ := get("/api/v1/setup", "rebind.attacker.example"); code != http.StatusMisdirectedRequest {
		t.Errorf("API with a foreign Host: %d", code)
	}
	if code, _, _ := get("/healthz", "10.0.0.7:8080"); code != http.StatusOK {
		t.Errorf("health check by IP: %d", code)
	}
	if code, _, _ := get("/healthz", "rebind.attacker.example"); code != http.StatusMisdirectedRequest {
		t.Errorf("health check with a foreign Host: %d", code)
	}

	// "*" switches the rule off for everything, as documented.
	any := newTenv(t, withOpts(func(o *Options) { o.Fallback = ui; o.AllowedHosts = []string{"*"} }))
	if w := any.do(req{method: "GET", path: "/login", h: hdr{"Host": "whatever.example"}}); w.Code != http.StatusOK {
		t.Errorf("AllowedHosts=*: %d", w.Code)
	}
}
