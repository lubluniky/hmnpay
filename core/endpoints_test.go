package core_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// TestTwoURLsOfOneEndpointAreRefused: two URLs that differ only in their
// path or query (another API key, say) reach one endpoint, and would be
// counted as two opinions — one endpoint confirming its own answers, in the
// header and receipt checks and in the record of who scanned which blocks.
// The network settings refuse them, at setup as after it.
func TestTwoURLsOfOneEndpointAreRefused(t *testing.T) {
	for _, pair := range [][2]string{
		{"https://bsc.example.org/v1/key-a", "https://bsc.example.org/v1/key-b"},
		{"https://bsc.example.org", "https://BSC.example.org:443/?apikey=x"},
		{"http://10.0.0.5:8545", "http://10.0.0.5:8545/rpc"},
	} {
		e := coretest.NewEnv(t)
		_, err := e.Service.UpdateNetwork(e.Operator(), core.UpdateNetworkInput{RPCURLs: pair[:]})
		if err == nil || !strings.Contains(err.Error(), "same endpoint") || core.KindOf(err) != core.KindInvalid {
			t.Fatalf("%v: %v", pair, err)
		}
		in := e.SetupInput()
		in.RPCURLs = pair[:]
		fresh := coretest.NewEnv(t, coretest.WithoutSetup())
		if _, err := fresh.Service.CompleteSetup(t.Context(), in); err == nil || !strings.Contains(err.Error(), "same endpoint") {
			t.Fatalf("setup with %v: %v", pair, err)
		}
	}
	// Different ports are different endpoints; so is another scheme.
	e := coretest.NewEnv(t)
	if _, err := e.Service.UpdateNetwork(e.Operator(), core.UpdateNetworkInput{RPCURLs: []string{"http://10.0.0.5:8545", "http://10.0.0.5:8546"}}); err != nil {
		t.Fatal(err)
	}
}

// TestEndpointsThatLookLikeOneProviderAreNamed: hmnpay cannot know who runs
// an endpoint, but two hosts of one registrable domain — two of a provider's
// public nodes, typically — are one opinion as far as anyone can tell. The
// configuration is accepted, and the node status and the network settings
// say so.
func TestEndpointsThatLookLikeOneProviderAreNamed(t *testing.T) {
	e := coretest.NewEnv(t)
	st, err := e.Service.UpdateNetwork(e.Operator(), core.UpdateNetworkInput{RPCURLs: []string{
		"https://bsc-dataseed1.binance.org", "https://bsc-rpc.publicnode.com/", "https://bsc-dataseed2.binance.org/key",
		"https://rpc.node.example.co.uk", "https://other.example.co.uk",
		"http://127.0.0.1:8545", "http://localhost:8546", "http://127.0.0.1:8547",
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"https://bsc-dataseed1.binance.org", "https://bsc-dataseed2.binance.org"},
		{"https://rpc.node.example.co.uk", "https://other.example.co.uk"},
	}
	got := st.Network.SameProvider()
	if !slices.EqualFunc(got, want, func(a, b []string) bool { return slices.Equal(a, b) }) {
		t.Fatalf("same provider: %q", got)
	}
	warnings := st.Network.Warnings()
	if len(warnings) != 2 || !strings.Contains(warnings[0], "https://bsc-dataseed1.binance.org and https://bsc-dataseed2.binance.org look like the same provider") ||
		strings.Contains(strings.Join(warnings, " "), "key") {
		t.Fatalf("warnings: %q", warnings)
	}
	ns, err := e.Service.NodeStatus(e.Operator())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(ns.SameProvider, want, func(a, b []string) bool { return slices.Equal(a, b) }) || !slices.Equal(ns.Warnings, warnings) {
		t.Fatalf("node status: same provider %q, warnings %q", ns.SameProvider, ns.Warnings)
	}
	// Independent providers: nothing to say.
	if st, err := e.Service.UpdateNetwork(e.Operator(), core.UpdateNetworkInput{RPCURLs: []string{"https://bsc-dataseed.bnbchain.org", "https://bsc-rpc.publicnode.com"}}); err != nil || len(st.Network.Warnings()) != 0 {
		t.Fatalf("%v %q", err, st.Network.Warnings())
	}
}

// TestSpellingsOfOneEndpointAreOneEndpoint: the same-endpoint rule compares
// canonical identities — letter case, a trailing dot, an internationalised
// name against its ASCII form, the forms of one IPv6 address, a default port
// written out — so none of these pairs is accepted as two endpoints. A
// change of scheme or of port is another endpoint, of what looks like the
// same provider.
func TestSpellingsOfOneEndpointAreOneEndpoint(t *testing.T) {
	refused := [][2]string{
		{"https://abc.example.org", "https://ABC.example.org:443"},
		{"https://abc.example.org", "https://abc.example.org."},
		{"https://bücher.example", "https://xn--bcher-kva.example"},
		{"https://[2001:db8::1]", "https://[2001:0db8::1]"},
		{"http://192.0.2.7", "http://[::ffff:192.0.2.7]:80"},
	}
	for _, p := range refused {
		e := coretest.NewEnv(t)
		_, err := e.Service.UpdateNetwork(e.Operator(), core.UpdateNetworkInput{RPCURLs: p[:]})
		if f, ok := fieldError(err, "rpc_urls.1"); !ok || f.Code != "same_endpoint" {
			t.Errorf("%v: %v, want rpc_urls.1 same_endpoint", p, err)
		}
	}
	for _, p := range [][2]string{{"https://abc.example.org", "http://abc.example.org"}, {"https://abc.example.org", "https://abc.example.org:444"}} {
		e := coretest.NewEnv(t)
		st, err := e.Service.UpdateNetwork(e.Operator(), core.UpdateNetworkInput{RPCURLs: p[:]})
		if err != nil || len(st.Network.SameProvider()) != 1 {
			t.Errorf("%v: %v, same provider %v; want accepted, and named as one provider", p, err, st.Network.SameProvider())
		}
	}
}
