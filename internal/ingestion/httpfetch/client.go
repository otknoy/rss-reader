// Package httpfetch retrieves and parses feeds and articles from the public network.
package httpfetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"rss-reader/internal/feeds"
	"time"
)

// Client retrieves feeds and article content over HTTP.
type Client struct{ http *http.Client }

// New creates a client restricted to public-network destinations.
func New() *Client { return &Client{http: publicClient()} }

// publicClient checks every resolved address at connection time, including redirects.
func publicClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("接続先が見つかりません")
		}
		for _, ip := range ips {
			if !publicIP(ip) {
				return nil, errors.New("ローカル・プライベートネットワークのURLは取得できません")
			}
		}
		dialer := net.Dialer{Timeout: 10 * time.Second}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}
	return &http.Client{Timeout: 20 * time.Second, Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("リダイレクトが多すぎます")
		}
		return feeds.ValidateURL(req.URL.String())
	}}
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, raw := range []string{
		"0.0.0.0/8",
		"100.64.0.0/10",
		"192.0.0.0/24",
		"192.0.2.0/24",
		"198.18.0.0/15",
		"198.51.100.0/24",
		"203.0.113.0/24",
		"240.0.0.0/4",
		"2001:db8::/32",
		"2001::/32",
		"2002::/16",
		"64:ff9b::/96",
	} {
		if netip.MustParsePrefix(raw).Contains(ip) {
			return false
		}
	}
	return true
}
