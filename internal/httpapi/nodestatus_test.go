package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// TestNodeStatusFields: the node status says whether the watcher is
// starting, which endpoint is read from, which one verifies it and which
// one confirms its log scans, whether one endpoint is believed alone, and
// what holds the head — as the chain adapter reported it.
func TestNodeStatusFields(t *testing.T) {
	e := newTenv(t, withEnv(coretest.WithoutChainHead()))
	get := func() (nodeStatusDTO, string) {
		w := e.do(req{method: "GET", path: "/api/v1/node-status", h: e.session()})
		wantStatus(t, w, http.StatusOK)
		return decodeBody[nodeStatusDTO](t, w), w.Body.String()
	}

	// Before any report: nothing is known, and nothing is null.
	ns, body := get()
	if ns.Starting || ns.Healthy || ns.Warnings == nil || !strings.Contains(body, `"starting":false`) || !strings.Contains(body, `"warnings":[]`) {
		t.Fatalf("node status without a report: %s", body)
	}

	// Starting: not healthy, endpoints not yet asked are not failures.
	if err := e.Service.ReportChainStatus(e.System(), core.ChainStatus{
		Starting: true,
		Endpoints: []core.RPCEndpointStatus{
			{URL: "https://a.example", State: core.EndpointNotChecked},
			{URL: "https://b.example", State: core.EndpointHealthy, Healthy: true, InUse: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ns, body = get()
	if !ns.Starting || ns.Healthy || ns.Endpoints[0].State != "not_checked" || ns.Endpoints[0].Error != "" {
		t.Fatalf("node status while starting: %s", body)
	}
	w := e.do(req{method: "GET", path: "/readyz"})
	if !strings.Contains(w.Body.String(), `"chain_starting":true`) {
		t.Fatalf("readyz while starting: %s", w.Body.String())
	}

	// Running.
	if err := e.Service.ReportChainStatus(e.System(), core.ChainStatus{
		LatestBlock: 124,
		Endpoints: []core.RPCEndpointStatus{
			{URL: "https://a.example", State: core.EndpointHealthy, Healthy: true, InUse: true},
			{URL: "https://b.example", State: core.EndpointHealthy, Healthy: true},
		},
		Primary: "https://a.example", Verifier: "https://b.example", LogVerifier: "https://b.example",
		Solo: true, LogsUnchecked: true, Hold: "waiting for b", Warnings: []string{"the processed head is waiting: waiting for b"},
	}); err != nil {
		t.Fatal(err)
	}
	ns, body = get()
	if ns.Starting || ns.Primary != "https://a.example" || ns.Verifier != "https://b.example" || ns.LogVerifier != "https://b.example" ||
		!ns.Solo || !ns.LogsUnchecked || ns.Hold != "waiting for b" || len(ns.Warnings) != 1 || ns.Endpoints[1].State != "healthy" {
		t.Fatalf("node status: %s", body)
	}
	for _, field := range []string{`"primary":`, `"verifier":`, `"log_verifier":`, `"solo":`, `"logs_unchecked":`, `"hold":`, `"work":`, `"state":`, `"idle":false`, `"quiet":false`} {
		if !strings.Contains(body, field) {
			t.Errorf("node status lacks %s: %s", field, body)
		}
	}

	// Idle: nothing to watch, so the watcher looks at the chain every half
	// minute and the head is that far behind. The status says so, the node
	// is healthy, and a monitor that reads /readyz sees a healthy chain.
	if err := e.Service.ReportChainStatus(e.System(), core.ChainStatus{
		Idle:        true,
		LatestBlock: 1_000_000, // no head yet: as far behind as can be
		Endpoints: []core.RPCEndpointStatus{
			{URL: "https://a.example", State: core.EndpointHealthy, Healthy: true, InUse: true},
			{URL: "https://b.example", State: core.EndpointHealthy, Healthy: true},
		},
		Primary: "https://a.example", Verifier: "https://b.example",
	}); err != nil {
		t.Fatal(err)
	}
	ns, body = get()
	if !ns.Idle || ns.Quiet || !ns.Healthy || ns.Starting || ns.LagBlocks != 1_000_000 || !strings.Contains(body, `"idle":true`) || !strings.Contains(body, `"quiet":false`) {
		t.Fatalf("node status while idle: %s", body)
	}
	w = e.do(req{method: "GET", path: "/readyz"})
	if !strings.Contains(w.Body.String(), `"chain_healthy":true`) {
		t.Fatalf("readyz while idle: %s", w.Body.String())
	}

	// Quiet: idle, with the addresses of closed payments still scanned for
	// late funds. "idle" stays true — whatever reads it keeps reading the
	// lag as the pause it is — and "quiet" says there is a watch list.
	if err := e.Service.ReportChainStatus(e.System(), core.ChainStatus{
		Idle:        true,
		Quiet:       true,
		LatestBlock: 1_000_000,
		Endpoints: []core.RPCEndpointStatus{
			{URL: "https://a.example", State: core.EndpointHealthy, Healthy: true, InUse: true},
			{URL: "https://b.example", State: core.EndpointHealthy, Healthy: true},
		},
		Primary: "https://a.example", Verifier: "https://b.example", LogVerifier: "https://b.example",
	}); err != nil {
		t.Fatal(err)
	}
	ns, body = get()
	if !ns.Idle || !ns.Quiet || !ns.Healthy || ns.LagBlocks != 1_000_000 || !strings.Contains(body, `"idle":true`) || !strings.Contains(body, `"quiet":true`) {
		t.Fatalf("node status while quiet: %s", body)
	}
	w = e.do(req{method: "GET", path: "/readyz"})
	if !strings.Contains(w.Body.String(), `"chain_healthy":true`) {
		t.Fatalf("readyz while quiet: %s", w.Body.String())
	}
}
