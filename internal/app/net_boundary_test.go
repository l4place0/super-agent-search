package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicSpecialPurposePolicy(t *testing.T) {
	// Conservative allow policy: block whole special-purpose ranges, including
	// globally reachable exceptions. This is not an exhaustive IANA registry.
	blocked := []string{"192.0.0.9", "192.88.99.1", "198.18.0.1", "198.19.255.255", "255.255.255.255", "::", "::1", "::ffff:10.0.0.1", "64:ff9b::808:808", "64:ff9b:1::1", "100::1", "100:0:0:1::1", "2001::1", "2001:2::1", "2001:20::1", "2001:db8::1", "2002:7f00:1::1", "3fff::1", "5f00::1", "fc00::1", "fdff::1", "fe80::1", "fec0::1", "ff02::1"}
	for _, raw := range blocked {
		if publicIP(net.ParseIP(raw)) {
			t.Errorf("allowed special address %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "::ffff:8.8.8.8", "2001:4860:4860::8888", "2606:4700:4700::1111"} {
		if !publicIP(net.ParseIP(raw)) {
			t.Errorf("blocked ordinary public address %s", raw)
		}
	}
	if publicIP(nil) {
		t.Fatal("nil IP allowed")
	}
}
func TestPublicDNSValidatesEntireAnswerBeforeDial(t *testing.T) {
	for _, answer := range [][]string{{"8.8.8.8", "127.0.0.1"}, {"10.0.0.1", "8.8.8.8"}, {"2606:4700:4700::1111", "::1"}, {"::ffff:192.168.0.1"}, nil} {
		calls := 0
		n := publicNetwork{lookup: func(context.Context, string) ([]net.IPAddr, error) {
			var out []net.IPAddr
			for _, raw := range answer {
				out = append(out, net.IPAddr{IP: net.ParseIP(raw)})
			}
			return out, nil
		}, dial: func(context.Context, string, string) (net.Conn, error) {
			calls++
			return nil, errors.New("should not dial")
		}}
		if _, err := n.dialContext(context.Background(), "tcp", "untrusted.test:443"); err == nil {
			t.Fatal("unsafe DNS accepted")
		}
		if calls != 0 {
			t.Fatal("dial happened before full DNS validation")
		}
	}
}
func TestPublicDNSPinningRebindingAndFallback(t *testing.T) {
	lookups, dials := 0, []string{}
	n := publicNetwork{lookup: func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		if lookups > 1 {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("1.1.1.1")}}, nil
	}, dial: func(_ context.Context, network, address string) (net.Conn, error) {
		dials = append(dials, address)
		if len(dials) == 1 {
			return nil, errors.New("first public address unreachable")
		}
		left, right := net.Pipe()
		right.Close()
		return left, nil
	}}
	conn, err := n.dialContext(context.Background(), "tcp", "rebinding.test:443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if lookups != 1 || strings.Join(dials, ",") != "8.8.8.8:443,1.1.1.1:443" {
		t.Fatalf("not pinned to validated addresses: %d %v", lookups, dials)
	}
	if _, err := n.dialContext(context.Background(), "tcp", "rebinding.test:443"); err == nil {
		t.Fatal("rebound private DNS accepted")
	}
	if len(dials) != 2 {
		t.Fatal("rebound DNS reached dialer")
	}
}
func TestPublicRedirectRevalidatesDestination(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1/private", "http://[::1]/private", "http://private.test/private"} {
		t.Run(target, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target, 302) }))
			defer upstream.Close()
			dials := 0
			n := publicNetwork{lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
				if host == "public.test" {
					return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
				}
				return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
			}, dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				dials++
				if address != "8.8.8.8:80" {
					t.Errorf("unsafe address dialled: %s", address)
				}
				return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
			}}
			client := newHTTPClientWithNetwork(n)
			defer client.public.CloseIdleConnections()
			transport := client.public.Transport.(*http.Transport)
			if transport.Proxy != nil {
				t.Fatal("public fetch uses environment proxy")
			}
			if res, err := client.public.Get("http://public.test/start"); err == nil {
				res.Body.Close()
				t.Fatal("private redirect followed")
			}
			if dials != 1 {
				t.Fatalf("redirect reached dialer %d times", dials)
			}
		})
	}
}
func TestDoHMalformedBoundedAndContext(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       int
	}{
		{"valid", `{"Status":0,"Answer":[{"type":1,"data":"8.8.8.8"},{"type":5,"data":"alias.test"}]}`, 200, 1},
		{"mixed", `{"Status":0,"Answer":[{"type":1,"data":"8.8.8.8"},{"type":1,"data":"10.0.0.1"}]}`, 200, 2},
		{"empty", `{"Status":0}`, 200, 0},
		{"invalid-ip", `{"Status":0,"Answer":[{"type":1,"data":"not-an-ip"}]}`, 200, 0},
		{"failure", `{"Status":2}`, 200, -1},
		{"non-2xx", `secret body`, 503, -1},
		{"malformed", `{"Status":`, 200, -1},
		{"oversized", strings.Repeat("x", (64<<10)+1), 200, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("name") != "test.example" || r.URL.Query().Get("type") != "A" || r.Header.Get("Accept") != "application/dns-json" {
					t.Error("incorrect DoH request")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
			})}
			answer, err := resolveDoH(context.Background(), "test.example", "https://doh.invalid/query", client)
			if tc.want < 0 {
				if err == nil {
					t.Fatal("bad DoH accepted")
				}
				if strings.Contains(err.Error(), "secret body") {
					t.Fatal("DoH body leaked")
				}
				return
			}
			if err != nil || len(answer) != tc.want {
				t.Fatalf("answer count %d, %v", len(answer), err)
			}
			if tc.name == "mixed" {
				n := publicNetwork{lookup: func(context.Context, string) ([]net.IPAddr, error) { return answer, nil }, dial: func(context.Context, string, string) (net.Conn, error) {
					t.Error("mixed DoH dialled")
					return nil, errors.New("blocked")
				}}
				if _, err := n.dialContext(context.Background(), "tcp", "mixed.test:80"); err == nil {
					t.Fatal("mixed DoH bypass")
				}
			}
		})
	}
	invoked := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { invoked = true; return nil, r.Context().Err() })}
	if _, err := resolveDoH(context.Background(), "test.example", "http://doh.invalid", client); err == nil || invoked {
		t.Fatal("insecure DoH endpoint used")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolveDoH(ctx, "test.example", "https://doh.invalid", client); err == nil {
		t.Fatal("cancelled lookup succeeded")
	}
}
