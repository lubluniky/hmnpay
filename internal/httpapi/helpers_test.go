package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

const testOrigin = "https://pay.example.com"

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type tenv struct {
	*coretest.Env
	t    *testing.T
	h    *Handler
	logs *syncBuffer
}

type setup struct {
	opts    []func(*Options)
	envOpts []coretest.EnvOption
}

func withOpts(f func(*Options)) func(*setup) { return func(s *setup) { s.opts = append(s.opts, f) } }
func withEnv(o ...coretest.EnvOption) func(*setup) {
	return func(s *setup) { s.envOpts = append(s.envOpts, o...) }
}

// newTenv builds a real core.Service on the in-memory store and the HTTP
// adapter on top of it. Rate limits are generous unless a test tightens
// them.
func newTenv(t *testing.T, fs ...func(*setup)) *tenv {
	t.Helper()
	var s setup
	for _, f := range fs {
		f(&s)
	}
	env := coretest.NewEnv(t, s.envOpts...)
	logs := &syncBuffer{}
	big := Rate{Burst: 100000, Every: 1}
	opts := Options{
		PublicURL: testOrigin,
		Logger:    slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Limits: Limits{
			LoginPerIP: big, LoginPerAccount: big, LoginGlobal: big, AuthFailuresPerIP: big, SetupPerIP: big,
			PublicReadPerIP: big, LinkPaymentPerIP: big, LinkPaymentPerIPPerLink: big, LinkPaymentPerLink: big, LinkPaymentGlobal: big,
		},
	}
	for _, f := range s.opts {
		f(&opts)
	}
	h, err := New(env.Service, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(h.Close)
	return &tenv{Env: env, t: t, h: h, logs: logs}
}

type hdr map[string]string

// req is a test request.
type req struct {
	method, path string
	body         any // nil, string (raw) or a value to marshal
	h            hdr
	remote       string
}

func (e *tenv) do(rq req) *httptest.ResponseRecorder {
	e.t.Helper()
	var body io.Reader
	switch b := rq.body.(type) {
	case nil:
	case string:
		body = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(rq.method, rq.path, body)
	if rq.body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r.RemoteAddr = "192.0.2.10:4321"
	if rq.remote != "" {
		r.RemoteAddr = rq.remote
	}
	// Requests arrive for the instance's own host name, as they do through
	// a reverse proxy (or for localhost when no public URL is configured).
	r.Host = "localhost:8080"
	if e.h.publicHost != "" {
		r.Host = strings.TrimPrefix(strings.TrimPrefix(e.h.publicOrigin, "https://"), "http://")
	}
	for k, v := range rq.h {
		if k == "Host" {
			r.Host = v
			continue
		}
		if v == "" {
			r.Header.Del(k)
			continue
		}
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// Credential headers.
func bearer(token string) hdr { return hdr{"Authorization": "Bearer " + token} }

// session returns headers for a browser request with the operator's
// session cookie, from the instance's own origin.
func (e *tenv) session() hdr {
	return hdr{"Cookie": cookieSecure + "=" + e.Token, "Origin": testOrigin, "Sec-Fetch-Site": "same-origin"}
}

func (e *tenv) key(scopes ...core.Scope) string {
	e.t.Helper()
	_, secret := e.APIKey(scopes...)
	return secret
}

func merge(hs ...hdr) hdr {
	out := hdr{}
	for _, h := range hs {
		for k, v := range h {
			out[k] = v
		}
	}
	return out
}

func decodeBody[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return v
}

type errResp struct {
	Error struct {
		Code    string       `json:"code"`
		Message string       `json:"message"`
		Fields  []fieldError `json:"fields"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d; body %s", w.Code, status, w.Body.String())
	}
}

func wantError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) errResp {
	t.Helper()
	wantStatus(t, w, status)
	e := decodeBody[errResp](t, w)
	if e.Error.Code != code {
		t.Fatalf("error code %q, want %q; body %s", e.Error.Code, code, w.Body.String())
	}
	if e.RequestID == "" || e.RequestID != w.Header().Get("X-Request-Id") {
		t.Fatalf("request id %q vs header %q", e.RequestID, w.Header().Get("X-Request-Id"))
	}
	return e
}

func findCookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}
