package ingestion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	"golang.org/x/net/html"
	"rss-reader/internal/articles"
	"rss-reader/internal/feeds"
)

type Service struct {
	feeds    *feeds.Store
	articles *articles.Store
	client   *http.Client
}

func New(f *feeds.Store, a *articles.Store) *Service {
	return &Service{feeds: f, articles: a, client: PublicClient()}
}

// PublicClient checks every resolved address at connection time, including redirects.
func PublicClient() *http.Client {
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
	for _, raw := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "2001::/32", "2002::/16", "64:ff9b::/96"} {
		if netip.MustParsePrefix(raw).Contains(ip) {
			return false
		}
	}
	return true
}
func (s *Service) Refresh(ctx context.Context) error {
	list, err := s.feeds.List(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, f := range list {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		title, fetchErr := s.fetch(ctx, f)
		if err := s.feeds.Record(ctx, f.ID, title, fetchErr); err != nil {
			failures = append(failures, err)
		}
		if fetchErr != nil {
			failures = append(failures, fmt.Errorf("%s: %w", f.Title, fetchErr))
		}
	}
	return errors.Join(failures...)
}
func (s *Service) fetch(ctx context.Context, f feeds.Feed) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "PersonalRSSReader/0.1")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const max = 4 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return "", err
	}
	if len(raw) > max {
		return "", errors.New("フィードが4MBを超えています")
	}
	title, items, err := parseFeed(raw, resp.Request.URL, f)
	if err != nil {
		return "", err
	}
	for _, item := range items {
		if err := s.articles.Save(ctx, item); err != nil {
			return title, err
		}
	}
	return title, nil
}

func parseFeed(raw []byte, base *url.URL, f feeds.Feed) (string, []articles.Article, error) {
	parsed, err := gofeed.NewParser().ParseString(string(raw))
	if err != nil {
		return "", nil, errors.New("RSS / Atomを解析できませんでした")
	}
	title := PlainText(parsed.Title)
	if title == "" {
		title = f.Title
	}
	var items []articles.Article
	for _, item := range parsed.Items {
		link := strings.TrimSpace(item.Link)
		if link == "" {
			continue
		}
		relative, err := base.Parse(link)
		if err != nil {
			continue
		}
		link = relative.String()
		if feeds.ValidateURL(link) != nil {
			continue
		}
		key := item.GUID
		if key == "" {
			key = link
		}
		body := item.Content
		if body == "" {
			body = item.Description
		}
		published := time.Time{}
		if item.PublishedParsed != nil {
			published = *item.PublishedParsed
		} else if item.UpdatedParsed != nil {
			published = *item.UpdatedParsed
		}
		items = append(items, articles.Article{SourceID: f.ID, SourceName: title, ExternalID: key, URL: link, Title: PlainText(item.Title), BodyHTML: body, BodyText: PlainText(body), PublishedAt: published})
	}
	return title, items, nil
}

// PlainText extracts readable text without executing or rendering feed HTML.
func PlainText(raw string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(raw))
	var out strings.Builder
	skip := 0
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return strings.Join(strings.Fields(out.String()), " ")
		case html.StartTagToken:
			name, _ := tokenizer.TagName()
			tag := string(name)
			if tag == "script" || tag == "style" {
				skip++
			}
			if tag == "p" || tag == "div" || tag == "br" || tag == "li" {
				out.WriteByte(' ')
			}
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			tag := string(name)
			if (tag == "script" || tag == "style") && skip > 0 {
				skip--
			}
			out.WriteByte(' ')
		case html.TextToken:
			if skip == 0 {
				out.Write(tokenizer.Text())
			}
		}
	}
}
