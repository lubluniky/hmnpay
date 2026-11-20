package webhook

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

func TestCheckAddr(t *testing.T) {
	cases := []struct {
		addr     string
		category string // "" = allowed under the default policy
		private  bool   // allowed with AllowPrivateNetworks
	}{
		// public
		{"93.184.216.34", "", false},
		{"8.8.8.8", "", false},
		{"2606:4700:4700::1111", "", false},
		{"64:ff9b::808:808", "", false}, // NAT64 of 8.8.8.8
		{"2002:808:808::1", "", false},  // 6to4 of 8.8.8.8
		{"::ffff:8.8.8.8", "", false},   // mapped public
		{"172.32.0.1", "", false},       // just outside 172.16/12
		{"100.128.0.1", "", false},      // just outside CGNAT
		{"169.255.0.1", "", false},      // just outside link-local
		{"11.0.0.1", "", false},         // just outside 10/8
		{"223.255.255.255", "", false},  // just below multicast
		// loopback
		{"127.0.0.1", "loopback", false},
		{"127.255.255.254", "loopback", false},
		{"::1", "loopback", false},
		{"::ffff:127.0.0.1", "loopback", false},
		{"::ffff:0:127.0.0.1", "loopback", false}, // SIIT
		{"64:ff9b::7f00:1", "loopback", false},    // NAT64 smuggling 127.0.0.1
		{"2002:7f00:1::", "loopback", false},      // 6to4 smuggling 127.0.0.1
		// unspecified / this network
		{"0.0.0.0", "unspecified", false},
		{"0.1.2.3", "unspecified", false},
		{"::", "unspecified", false},
		{"::ffff:0.0.0.0", "unspecified", false},
		// link-local and metadata
		{"169.254.169.254", "metadata", false},
		{"::ffff:169.254.169.254", "metadata", false},
		{"64:ff9b::a9fe:a9fe", "metadata", false},
		{"169.254.1.1", "link_local", false},
		{"fe80::1", "link_local", false},
		{"fe80::1%eth0", "link_local", false},
		{"fd00:ec2::254", "metadata", false},   // AWS IMDS v6, inside ULA
		{"100.100.100.200", "metadata", false}, // Alibaba, inside CGNAT
		{"168.63.129.16", "metadata", false},   // Azure, public range
		{"fe80::a9fe:a9fe", "metadata", false}, // link-local alias
		{"fd20:ce::254", "metadata", false},    // GCP v6
		// multicast, broadcast, reserved
		{"224.0.0.1", "multicast", false},
		{"239.255.255.250", "multicast", false},
		{"ff02::1", "multicast", false},
		{"255.255.255.255", "broadcast", false},
		{"240.0.0.1", "reserved", false},
		{"192.0.2.1", "reserved", false},
		{"198.51.100.7", "reserved", false},
		{"203.0.113.9", "reserved", false},
		{"192.0.0.170", "reserved", false},
		{"2001:db8::1", "reserved", false},
		{"::127.0.0.1", "reserved", false}, // IPv4-compatible
		{"::8.8.8.8", "reserved", false},
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", "reserved", false}, // Teredo
		{"100::1", "reserved", false},
		// private (opt-in)
		{"10.0.0.1", "private", true},
		{"172.16.0.1", "private", true},
		{"172.31.255.255", "private", true},
		{"192.168.1.1", "private", true},
		{"100.64.0.1", "private", true},
		{"198.18.0.1", "private", true},
		{"fc00::1", "private", true},
		{"fd12:3456::1", "private", true},
		{"fec0::1", "private", true},
		{"64:ff9b:1::a00:1", "private", true},
		{"::ffff:10.0.0.1", "private", true},
		{"::ffff:192.168.0.1", "private", true},
		{"64:ff9b::a00:1", "private", true}, // NAT64 smuggling 10.0.0.1
		{"2002:c0a8:101::1", "private", true},
	}
	for _, c := range cases {
		addr := netip.MustParseAddr(c.addr)
		err := EgressPolicy{}.CheckAddr(addr)
		if c.category == "" {
			if err != nil {
				t.Errorf("%s: blocked (%v), want allowed", c.addr, err)
			}
			continue
		}
		var pe *PolicyError
		if !errors.As(err, &pe) || !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("%s: err = %v, want blocked (%s)", c.addr, err, c.category)
			continue
		}
		if pe.Category != c.category {
			t.Errorf("%s: category %q, want %q (%v)", c.addr, pe.Category, c.category, err)
		}
		err = EgressPolicy{AllowPrivateNetworks: true}.CheckAddr(addr)
		if c.private && err != nil {
			t.Errorf("%s: blocked with AllowPrivateNetworks: %v", c.addr, err)
		}
		if !c.private && err == nil {
			t.Errorf("%s: allowed with AllowPrivateNetworks, must stay blocked", c.addr)
		}
	}
}

func TestAllowedPrefixes(t *testing.T) {
	p := EgressPolicy{AllowedPrefixes: []netip.Prefix{
		netip.MustParsePrefix("127.0.0.1/32"),
		netip.MustParsePrefix("10.1.0.0/16"),
		netip.MustParsePrefix("::1/128"),
		// An allowlist entry cannot unlock the always-blocked categories.
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("0.0.0.0/0"),
	}}
	for addr, allowed := range map[string]bool{
		"127.0.0.1":        true,
		"::ffff:127.0.0.1": true,
		"::1":              true,
		"127.0.0.2":        true, // 0.0.0.0/0 unlocks loopback and private space
		"10.1.2.3":         true,
		"10.2.0.1":         true, // 0.0.0.0/0 is an allowlist entry too
		"169.254.169.254":  false,
		"169.254.1.1":      false,
		"0.0.0.0":          false,
		"224.0.0.1":        false,
		"255.255.255.255":  false,
		"100.100.100.200":  false,
		"fd00:ec2::254":    false,
	} {
		err := p.CheckAddr(netip.MustParseAddr(addr))
		if allowed != (err == nil) {
			t.Errorf("%s: err = %v, want allowed=%v", addr, err, allowed)
		}
	}
	// A narrow allowlist leaves the rest of the private space closed.
	narrow := EgressPolicy{AllowedPrefixes: []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")}}
	if narrow.CheckAddr(netip.MustParseAddr("10.1.9.9")) != nil {
		t.Error("10.1.9.9 should be allowed")
	}
	if narrow.CheckAddr(netip.MustParseAddr("10.2.0.1")) == nil {
		t.Error("10.2.0.1 should stay blocked")
	}
	if narrow.CheckAddr(netip.MustParseAddr("127.0.0.1")) == nil {
		t.Error("loopback should stay blocked")
	}
}

func TestValidateURL(t *testing.T) {
	cases := []struct {
		url      string
		category string // "" = valid
		blocked  bool   // ErrBlockedDestination rather than ErrInvalidURL
	}{
		{"https://example.com/hooks", "", false},
		{"http://example.com/hooks", "", false},
		{"HTTPS://Example.COM:8443/x?y=1", "", false},
		{"https://xn--bcher-kva.example/", "", false},
		{"https://93.184.216.34/", "", false},
		{"https://[2606:4700:4700::1111]/", "", false},
		{"https://api.example.com./hook", "", false},
		{"https://1password.com/", "", false},      // numeric first label is fine
		{"https://cafe.0xdeadbeef.io/", "", false}, // only the last label matters

		{"ftp://example.com/", "scheme", false},
		{"gopher://example.com/", "scheme", false},
		{"file:///etc/passwd", "scheme", false},
		{"javascript:alert(1)", "scheme", false},
		{"https:example.com", "url", false},
		{"/relative", "scheme", false},
		{"https://", "url", false},
		{"https://user:s3cr3t@example.com/", "credentials", false},
		{"https://user@example.com/", "credentials", false},
		{"https://example.com:0/", "port", false},
		{"https://example.com:65536/", "port", false},
		{"https://exa mple.com/", "url", false},
		{"https://example.com/\nX-Injected: 1", "url", false},
		{"https://bücher.example/", "host", false},
		{"https://127。0。0。1/", "host", false},

		// numeric host tricks some resolvers read as 127.0.0.1
		{"http://2130706433/", "host", false},
		{"http://0x7f000001/", "host", false},
		{"http://0x7f.0.0.1/", "host", false},
		{"http://0177.0.0.1/", "host", false},
		{"http://127.1/", "host", false},
		{"http://127.000.000.001/", "host", false},
		{"http://017700000001/", "host", false},
		{"http://0x/", "host", false},

		// literal addresses
		{"http://127.0.0.1:8080/", "loopback", true},
		{"http://[::1]/", "loopback", true},
		{"http://[::ffff:127.0.0.1]/", "loopback", true},
		{"http://[::ffff:7f00:1]/", "loopback", true},
		{"http://[0:0:0:0:0:ffff:7f00:1]/", "loopback", true},
		{"http://[64:ff9b::7f00:1]/", "loopback", true},
		{"http://[::127.0.0.1]/", "reserved", true},
		{"http://169.254.169.254/latest/meta-data/", "metadata", true},
		{"http://[fd00:ec2::254]/", "metadata", true},
		{"http://[fe80::1%25en0]/", "link_local", true},
		{"http://0.0.0.0/", "unspecified", true},
		{"http://[::]/", "unspecified", true},
		{"http://10.0.0.5/", "private", true},
		{"http://192.168.1.1/", "private", true},
		{"http://[fd00::1]/", "private", true},
		{"http://224.0.0.1/", "multicast", true},
		{"http://localhost:3000/", "loopback", true},
		{"http://LOCALHOST./", "loopback", true},
		{"http://api.localhost/", "loopback", true},
	}
	for _, c := range cases {
		err := ValidateURL(c.url)
		if c.category == "" {
			if err != nil {
				t.Errorf("%q: %v, want valid", c.url, err)
			}
			continue
		}
		var pe *PolicyError
		if !errors.As(err, &pe) {
			t.Errorf("%q: err = %v, want a PolicyError (%s)", c.url, err, c.category)
			continue
		}
		if pe.Category != c.category {
			t.Errorf("%q: category %q, want %q (%v)", c.url, pe.Category, c.category, err)
		}
		if c.blocked != errors.Is(err, ErrBlockedDestination) || c.blocked == errors.Is(err, ErrInvalidURL) {
			t.Errorf("%q: sentinel mismatch for %v (blocked=%v)", c.url, err, c.blocked)
		}
		if strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%q: error leaks credentials: %v", c.url, err)
		}
	}
}

func TestValidateURLOptions(t *testing.T) {
	strict := EgressPolicy{RequireHTTPS: true, AllowedPorts: []int{443, 8443}}
	if err := strict.ValidateURL("http://example.com/"); err == nil {
		t.Error("http accepted with RequireHTTPS")
	}
	if err := strict.ValidateURL("https://example.com:8443/"); err != nil {
		t.Errorf("8443: %v", err)
	}
	if err := strict.ValidateURL("https://example.com/"); err != nil {
		t.Errorf("default 443: %v", err)
	}
	if err := strict.ValidateURL("https://example.com:22/"); !errors.Is(err, ErrBlockedDestination) {
		t.Errorf("port 22: %v", err)
	}
	private := EgressPolicy{AllowPrivateNetworks: true}
	if err := private.ValidateURL("http://10.0.0.5:8080/hook"); err != nil {
		t.Errorf("private allowed: %v", err)
	}
	if err := private.ValidateURL("http://127.0.0.1/"); err == nil {
		t.Error("loopback must stay blocked with AllowPrivateNetworks")
	}
	if err := private.ValidateURL("http://169.254.169.254/"); err == nil {
		t.Error("metadata must stay blocked with AllowPrivateNetworks")
	}
	local := EgressPolicy{AllowedPrefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	if err := local.ValidateURL("http://localhost:3000/"); err != nil {
		t.Errorf("localhost with loopback allowlisted: %v", err)
	}
}

// fakeResolver answers from a table and can change its answers between
// lookups (DNS rebinding).
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]string // host → successive answers (the last repeats)
	calls   map[string]int
}

func (r *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	seq, ok := r.answers[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	i := min(r.calls[host], len(seq)-1)
	r.calls[host]++
	var out []netip.Addr
	for _, s := range seq[i] {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

// recordingDial records what the guard dials, without touching the network.
type recordingDial struct {
	mu     sync.Mutex
	dialed []string
}

func (d *recordingDial) dial(_ context.Context, _, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	d.mu.Unlock()
	c1, c2 := net.Pipe()
	go c2.Close()
	return c1, nil
}

func TestGuardDialTime(t *testing.T) {
	res := &fakeResolver{answers: map[string][][]string{
		"public.example":   {{"93.184.216.34"}},
		"internal.example": {{"10.0.0.7"}},
		"meta.example":     {{"169.254.169.254"}},
		"loop.example":     {{"127.0.0.1"}},
		"mapped.example":   {{"::ffff:127.0.0.1"}},
		"mixed.example":    {{"93.184.216.34", "127.0.0.1"}},
		"v6loop.example":   {{"2606:4700::1", "::1"}},
		// Rebinding: the first answer is public, the next is loopback.
		"rebind.example": {{"93.184.216.34"}, {"127.0.0.1"}},
		"dual.example":   {{"2606:4700::1", "93.184.216.34"}},
	}}
	rec := &recordingDial{}
	g := newGuard(EgressPolicy{Resolver: res}, rec.dial)
	ctx := context.Background()

	dial := func(addr string) error {
		c, err := g.DialContext(ctx, "tcp", addr)
		if c != nil {
			c.Close()
		}
		return err
	}
	if err := dial("public.example:443"); err != nil {
		t.Fatalf("public: %v", err)
	}
	for _, host := range []string{"internal.example", "meta.example", "loop.example", "mapped.example", "mixed.example", "v6loop.example"} {
		if err := dial(host + ":443"); !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("%s: err = %v, want blocked", host, err)
		}
	}
	// Literal addresses that reach the dialer are checked too.
	for _, addr := range []string{"127.0.0.1:80", "[::1]:80", "[::ffff:169.254.169.254]:80", "0.0.0.0:80"} {
		if err := dial(addr); !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("%s: err = %v, want blocked", addr, err)
		}
	}
	// Numeric host tricks never reach the resolver.
	if err := dial("2130706433:80"); !errors.Is(err, ErrInvalidURL) {
		t.Errorf("decimal host: %v", err)
	}
	// Rebinding: every connection resolves and checks again; the address
	// checked is the address dialled.
	if err := dial("rebind.example:443"); err != nil {
		t.Fatalf("rebind first: %v", err)
	}
	if err := dial("rebind.example:443"); !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("rebind second: err = %v, want blocked", err)
	}
	if err := dial("dual.example:443"); err != nil {
		t.Fatalf("dual: %v", err)
	}
	if err := dial("nxdomain.example:443"); err == nil {
		t.Fatal("nxdomain dialled")
	}
	want := []string{"93.184.216.34:443", "93.184.216.34:443", "[2606:4700::1]:443"}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if strings.Join(rec.dialed, " ") != strings.Join(want, " ") {
		t.Fatalf("dialled %v, want %v (only checked IPs, never names)", rec.dialed, want)
	}
}

func TestGuardPortsAndControl(t *testing.T) {
	res := &fakeResolver{answers: map[string][][]string{"public.example": {{"93.184.216.34"}}}}
	rec := &recordingDial{}
	g := newGuard(EgressPolicy{Resolver: res, AllowedPorts: []int{443}}, rec.dial)
	if _, err := g.DialContext(context.Background(), "tcp", "public.example:25"); !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("port 25: %v", err)
	}
	// The socket-level Control hook refuses blocked addresses on its own.
	control := controlFunc(EgressPolicy{})
	if err := control("tcp4", "127.0.0.1:80", nil); !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("control loopback: %v", err)
	}
	if err := control("tcp6", "[fe80::1]:80", nil); !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("control link-local: %v", err)
	}
	if err := control("tcp4", "93.184.216.34:443", nil); err != nil {
		t.Fatalf("control public: %v", err)
	}
	// And the real dialer uses it: connecting to a live loopback listener
	// fails before connect(2).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	real := newGuard(EgressPolicy{}, nil)
	real.policy.AllowedPrefixes = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")} // pass the first check …
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := real.DialContext(ctx, "tcp", ln.Addr().String()); !errors.Is(err, ErrBlockedDestination) {
		// … the dialer's Control still has the strict policy.
		t.Fatalf("Control did not refuse loopback: %v", err)
	}
}

// The static URL check is what wiring plugs into the core, so that every
// transport — not just the HTTP API — refuses the same endpoint URLs when
// an endpoint is saved.
func TestValidateURLAsCoreHook(t *testing.T) {
	env := coretest.NewEnv(t, coretest.WithConfig(func(c *core.Config) { c.ValidateWebhookURL = ValidateURL }))
	create := func(url string) error {
		_, err := env.Service.CreateWebhookEndpoint(env.Operator(), core.WebhookEndpointInput{
			URL: url, Events: []core.EventType{core.EventPaymentPaid}, Enabled: true,
		})
		return err
	}
	for _, url := range []string{
		"http://169.254.169.254/latest/meta-data",
		"http://127.0.0.1:8080/hook",
		"http://localhost/hook",
		"https://10.0.0.5/hook",
		"http://2130706433/",
	} {
		err := create(url)
		fields := core.FieldErrors(err)
		if core.KindOf(err) != core.KindInvalid || len(fields) != 1 || fields[0].Field != "url" || fields[0].Code != "not_allowed" {
			t.Errorf("%s: %v, want a validation error on url (not_allowed)", url, err)
			continue
		}
		if !strings.HasPrefix(fields[0].Message, "webhook: ") {
			t.Errorf("%s: message %q", url, fields[0].Message)
		}
	}
	// Credentials are refused by the core itself, before the policy is asked.
	if f := core.FieldErrors(create("https://user:pw@hooks.example.com/x")); len(f) != 1 || f[0].Code != "credentials" {
		t.Errorf("credentials: %+v", f)
	}
	if err := create("https://hooks.example.com/x"); err != nil {
		t.Errorf("a public https URL was refused: %v", err)
	}
}
