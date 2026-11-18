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
	"rss-reader/internal/articles"
	"rss-reader/internal/feeds"
	"strconv"
	"strings"
	"time"

	"github.com/mmcdole/gofeed/rss"

	"github.com/mmcdole/gofeed"
	"golang.org/x/net/html"
)

// DefaultInterval is the fallback interval between feed fetches.
const DefaultInterval = 30 * time.Minute

type subscriptions interface {
	List(context.Context) ([]feeds.Feed, error)
	ListDue(context.Context) ([]feeds.Feed, error)
	Record(context.Context, int64, feeds.FetchResult, time.Duration) error
	WithRefreshLock(context.Context, func() error) error
}
type articleWriter interface {
	Save(context.Context, articles.Article) error
}

// Service fetches subscriptions and saves their articles.
type Service struct {
	feeds    subscriptions
	articles articleWriter
	client   *http.Client
	interval time.Duration
}

// New creates a feed ingestion service with a public-network HTTP client.
func New(f subscriptions, a articleWriter, interval time.Duration) *Service {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Service{feeds: f, articles: a, client: PublicClient(), interval: interval}
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

// Refresh is an explicit manual refresh; it bypasses the due time but uses conditional HTTP.
func (s *Service) Refresh(ctx context.Context) error { return s.refresh(ctx, false, 0) }

// RefreshDue performs one scheduled pass, shared by web polling and future CronJobs.
func (s *Service) RefreshDue(ctx context.Context) error { return s.refresh(ctx, true, 0) }

// RefreshFeed attempts the initial fetch of a single due subscription.
func (s *Service) RefreshFeed(ctx context.Context, id int64) error {
	return s.refresh(ctx, true, id)
}

func (s *Service) refresh(ctx context.Context, dueOnly bool, onlyID int64) error {
	return s.feeds.WithRefreshLock(ctx, func() error {
		var list []feeds.Feed
		var err error
		if dueOnly {
			list, err = s.feeds.ListDue(ctx)
		} else {
			list, err = s.feeds.List(ctx)
		}
		if err != nil {
			return err
		}
		var failures []error
		for _, f := range list {
			if onlyID != 0 && f.ID != onlyID {
				continue
			}
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failures, err)...)
			}
			result := s.fetch(ctx, f)
			if err := s.feeds.Record(ctx, f.ID, result, s.interval); err != nil {
				failures = append(failures, err)
			}
			if result.Err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", f.Title, result.Err))
			}
		}
		return errors.Join(failures...)
	})
}

func (s *Service) fetch(ctx context.Context, f feeds.Feed) (result feeds.FetchResult) {
	result = feeds.FetchResult{Title: f.Title, TTLMinutes: f.TTLMinutes, ETag: f.ETag, LastModified: f.LastModified}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		result.Err = err
		return
	}
	req.Header.Set("User-Agent", "PersonalRSSReader/0.1")
	if f.ETag != "" {
		req.Header.Set("If-None-Match", f.ETag)
	}
	if f.LastModified != "" {
		req.Header.Set("If-Modified-Since", f.LastModified)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		result.Err = err
		return
	}
	defer func() { result.Err = errors.Join(result.Err, resp.Body.Close()) }()
	if resp.StatusCode == http.StatusNotModified {
		if value := resp.Header.Get("ETag"); value != "" {
			result.ETag = value
		}
		if value := resp.Header.Get("Last-Modified"); value != "" {
			result.LastModified = value
		}
		return
	}
	if resp.StatusCode != http.StatusOK {
		result.Err = fmt.Errorf("HTTP %d", resp.StatusCode)
		return
	}
	const maxFeedBytes = 4 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		result.Err = err
		return
	}
	if len(raw) > maxFeedBytes {
		result.Err = errors.New("フィードが4MBを超えています")
		return
	}
	parsed, err := parseFeed(raw, resp.Request.URL, f)
	if err != nil {
		result.Err = err
		return
	}
	for _, item := range parsed.Items {
		if err := s.articles.Save(ctx, item); err != nil {
			result.Err = err
			return
		}
	}
	return feeds.FetchResult{Title: parsed.Title, TTLMinutes: parsed.TTLMinutes, ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
}

type parsedFeed struct {
	Title      string
	Items      []articles.Article
	TTLMinutes int64
}

// Invalid or unrepresentable TTLs fall back to the configured interval.
func parseTTL(raw string) int64 {
	minutes, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || minutes <= 0 || minutes > int64((1<<63-1)/time.Minute) {
		return 0
	}
	return minutes
}

func parseFeed(raw []byte, base *url.URL, f feeds.Feed) (parsedFeed, error) {
	parser := gofeed.NewParser()
	parser.KeepOriginalFeed = true
	parsed, err := parser.ParseString(string(raw))
	if err != nil {
		return parsedFeed{}, errors.New("RSS / Atomを解析できませんでした")
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
	ttl := int64(0)
	if original, ok := parsed.OriginalFeed().(*rss.Feed); ok {
		ttl = parseTTL(original.TTL)
	}
	return parsedFeed{Title: title, Items: items, TTLMinutes: ttl}, nil
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
