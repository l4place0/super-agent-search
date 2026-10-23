package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedUpstreamDoesNotUsePublicBoundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer server.Close()
	client := newHTTPClientWithNetwork(publicNetwork{lookup: func(context.Context, string) ([]net.IPAddr, error) {
		t.Error("trusted upstream used public resolver")
		return nil, nil
	}})
	defer client.upstream.CloseIdleConnections()
	// Disable ambient proxies only for this local test; production upstream may
	// use an administrator's proxy, unlike public fetch.
	client.upstream.Transport.(*http.Transport).Proxy = nil
	res, err := client.upstream.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
}
func TestPublicIPv6IsDialledAsPinnedLiteral(t *testing.T) {
	network := publicNetwork{lookup: func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("2606:4700:4700::1111")}}, nil
	}, dial: func(_ context.Context, network, address string) (net.Conn, error) {
		if address != "[2606:4700:4700::1111]:8443" {
			t.Errorf("IPv6 was not pinned: %s", address)
		}
		left, right := net.Pipe()
		right.Close()
		return left, nil
	}}
	conn, err := network.dialContext(context.Background(), "tcp", "ipv6.test:8443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}
func TestPublicRedirectBudget(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Redirect(w, r, "http://public.test/again", 302)
	}))
	defer upstream.Close()
	client := newHTTPClientWithNetwork(publicNetwork{lookup: func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}, dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}})
	defer client.public.CloseIdleConnections()
	if res, err := client.public.Get("http://public.test/start"); err == nil {
		res.Body.Close()
		t.Fatal("redirect loop accepted")
	}
	if requests != 5 {
		t.Fatalf("redirect budget changed: %d", requests)
	}
}
