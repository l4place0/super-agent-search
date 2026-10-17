package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type httpClient struct {
	upstream *http.Client
	public   *http.Client
}

func publicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "64:ff9b::/96", "2002::/16"} {
		_, n, _ := net.ParseCIDR(cidr)
		if n.Contains(ip) {
			return false
		}
	}
	return true
}
func validURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return nil, errors.New("expected an HTTP(S) URL without credentials")
	}
	return u, nil
}
func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := resolvePublic(ctx, host)
	if err != nil {
		return nil, errors.New("DNS lookup failed")
	}
	if len(ips) == 0 {
		return nil, errors.New("no address found")
	}
	for _, ip := range ips {
		if !publicIP(ip.IP) {
			return nil, errors.New("private/reserved destinations are blocked")
		}
	}
	d := net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
	for _, ip := range ips {
		conn, e := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if e == nil {
			return conn, nil
		}
		err = e
	}
	return nil, err
}

// Optional DoH bypasses broken VM DNS. publicDial still validates every address
// and pins the connection to the validated IP, including on redirects.
func resolvePublic(ctx context.Context, host string) ([]net.IPAddr, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IPAddr{{IP: ip}}, nil
	}
	endpoint := os.Getenv("SAS_DOH_URL")
	if endpoint == "" {
		return net.DefaultResolver.LookupIPAddr(ctx, host)
	}
	u, e := validURL(endpoint)
	if e != nil || u.Scheme != "https" {
		return nil, errors.New("invalid DoH configuration")
	}
	q := u.Query()
	q.Set("name", host)
	q.Set("type", "A")
	u.RawQuery = q.Encode()
	r, e := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if e != nil {
		return nil, e
	}
	r.Header.Set("Accept", "application/dns-json")
	client := &http.Client{Timeout: 8 * time.Second}
	res, e := client.Do(r)
	if e != nil {
		return nil, errors.New("DoH request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, errors.New("DoH response failed")
	}
	body, e := readBody(res.Body, 64<<10)
	if e != nil {
		return nil, e
	}
	var data struct {
		Status int
		Answer []struct {
			Type int
			Data string
		}
	}
	if json.Unmarshal(body, &data) != nil || data.Status != 0 {
		return nil, errors.New("DoH lookup failed")
	}
	ips := []net.IPAddr{}
	for _, a := range data.Answer {
		if a.Type == 1 {
			if ip := net.ParseIP(a.Data); ip != nil {
				ips = append(ips, net.IPAddr{IP: ip})
			}
		}
	}
	return ips, nil
}
func newHTTPClient() *httpClient {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxIdleConnsPerHost = 8
	base.ResponseHeaderTimeout = 75 * time.Second
	up := &http.Client{Transport: base, Timeout: 80 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	pub := base.Clone()
	pub.Proxy = nil
	pub.DialContext = publicDial
	reader := &http.Client{Transport: pub, Timeout: 20 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		_, err := validURL(req.URL.String())
		return err
	}}
	return &httpClient{upstream: up, public: reader}
}
func readBody(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("response exceeds size limit")
	}
	return b, nil
}
func cleanText(s string) string { return strings.Join(strings.Fields(s), " ") }
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
