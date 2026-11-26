package ingestion

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"rss-reader/internal/articles"
	"rss-reader/internal/feeds"
	"strings"
	"testing"
	"time"
)

func TestFeedTTL(t *testing.T) {
	base, _ := url.Parse("https://example.com/feed")
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"", 0},
		{"0", 0},
		{"-1", 0},
		{"abc", 0},
		{"1.5", 0},
		{" 60 ", 60},
		{"10", 10},
		{"9223372036854775807", 0},
		{"99999999999999999999999", 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			raw := `<rss version="2.0"><channel><title>TTL test</title><link>https://example.com</link><description>test</description><ttl>` + tc.value + `</ttl></channel></rss>`
			parsed, err := parseFeed([]byte(raw), base, feeds.Subscription{})
			if err != nil {
				t.Fatal(err)
			}
			if parsed.TTLMinutes != tc.want {
				t.Fatalf("ttl=%d, want %d", parsed.TTLMinutes, tc.want)
			}
		})
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type articleStub struct {
	saved []articles.Article
	err   error
}

func (s *articleStub) Save(_ context.Context, a articles.Article) error {
	s.saved = append(s.saved, a)
	return s.err
}

func TestConditionalFetch(t *testing.T) {
	fixture := `<rss version="2.0"><channel><title>TTL test</title><link>https://example.com</link><description>test</description><ttl>90</ttl><item><guid>1</guid><title>記事</title><link>https://example.com/one</link></item></channel></rss>`
	previous := feeds.Subscription{
		ID:           1,
		Title:        "old",
		URL:          "https://example.com/feed",
		TTLMinutes:   60,
		ETag:         `"old"`,
		LastModified: "Mon, 16 Nov 2026 00:00:00 GMT",
	}
	for _, status := range []int{http.StatusOK, http.StatusNotModified, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			writer := &articleStub{}
			service := New(nil, writer, DefaultInterval)
			service.client = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("If-None-Match") != previous.ETag ||
					req.Header.Get("If-Modified-Since") != previous.LastModified {
					t.Fatal("conditional headers missing")
				}
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Etag": {`"new"`}},
					Body:       io.NopCloser(strings.NewReader(fixture)),
					Request:    req,
				}, nil
			})}
			result := service.fetch(context.Background(), previous)
			switch status {
			case http.StatusOK:
				if result.Err != nil || result.TTLMinutes != 90 || result.ETag != `"new"` || len(writer.saved) != 1 {
					t.Fatalf("result=%+v saved=%v", result, writer.saved)
				}
				if result.LastModified != "" {
					t.Fatal("200 response should clear old validators when absent")
				}
			case http.StatusNotModified:
				if result.Err != nil ||
					result.TTLMinutes != 60 ||
					result.ETag != `"new"` ||
					result.LastModified != previous.LastModified ||
					len(writer.saved) != 0 {
					t.Fatalf("result=%+v saved=%v", result, writer.saved)
				}
			default:
				if result.Err == nil ||
					result.TTLMinutes != 60 ||
					result.ETag != previous.ETag ||
					len(writer.saved) != 0 {
					t.Fatalf("result=%+v saved=%v", result, writer.saved)
				}
			}
		})
	}
	t.Run("failed save must not commit new validators", func(t *testing.T) {
		writer := &articleStub{err: errors.New("database unavailable")}
		source := &subscriptionsStub{rows: []feeds.Subscription{previous}}
		service := New(source, writer, DefaultInterval)
		service.client = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Etag": {`"new"`}},
				Body:       io.NopCloser(strings.NewReader(fixture)),
				Request:    req,
			}, nil
		})}
		result := service.fetch(context.Background(), previous)
		if result.Err == nil || result.ETag != previous.ETag || result.TTLMinutes != previous.TTLMinutes {
			t.Fatalf("unsafe validators: %+v", result)
		}
		if err := service.RefreshDue(t.Context()); !errors.Is(err, writer.err) {
			t.Fatalf("scheduled refresh must report save failure, got %v", err)
		}
	})
}

type subscriptionsStub struct {
	rows                []feeds.Subscription
	results             []feeds.FetchResult
	interval            time.Duration
	listCalls, dueCalls int
	dueFeedIDs          []int64
	busy                bool
	recordErr           error
}

func (s *subscriptionsStub) List(context.Context) ([]feeds.Subscription, error) {
	s.listCalls++
	return s.rows, nil
}

func (s *subscriptionsStub) ListDue(context.Context) ([]feeds.Subscription, error) {
	s.dueCalls++
	return s.rows, nil
}

func (s *subscriptionsStub) ListDueFeed(_ context.Context, id int64) ([]feeds.Subscription, error) {
	s.dueFeedIDs = append(s.dueFeedIDs, id)
	for _, feed := range s.rows {
		if feed.ID == id {
			return []feeds.Subscription{feed}, nil
		}
	}
	return nil, nil
}

func (s *subscriptionsStub) Record(_ context.Context, _ int64, r feeds.FetchResult, interval time.Duration) error {
	s.results = append(s.results, r)
	s.interval = interval
	return s.recordErr
}

func (s *subscriptionsStub) WithRefreshLock(_ context.Context, run func() error) error {
	if s.busy {
		return feeds.ErrRefreshInProgress
	}
	return run()
}

func TestRefreshContinuesAfterFeedFailure(t *testing.T) {
	source := &subscriptionsStub{rows: []feeds.Subscription{
		{ID: 1, URL: "https://example.com/bad"},
		{ID: 2, URL: "https://example.com/good"},
	}}
	service := New(source, &articleStub{}, 45*time.Minute)
	service.client = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		status := 304
		if req.URL.Path == "/bad" {
			status = 503
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})}
	if err := service.RefreshDue(t.Context()); err != nil {
		t.Fatalf("scheduled refresh failed after recording a feed error: %v", err)
	}
	if source.dueCalls != 1 ||
		source.listCalls != 0 ||
		len(source.results) != 2 ||
		source.results[0].Err == nil ||
		source.results[1].Err != nil ||
		source.interval != 45*time.Minute {
		t.Fatalf("unexpected processing: %+v", source)
	}
	source.busy = true
	if err := service.Refresh(context.Background()); !errors.Is(err, feeds.ErrRefreshInProgress) {
		t.Fatalf("got %v", err)
	}
	if source.listCalls != 0 {
		t.Fatal("manual refresh ignored lock")
	}
	source.busy = false
	if err := service.Refresh(t.Context()); err == nil {
		t.Fatal("manual refresh must report feed failure")
	}
	if source.listCalls != 1 {
		t.Fatal("manual refresh should list all feeds")
	}
	source.recordErr = errors.New("could not record fetch result")
	if err := service.RefreshDue(t.Context()); !errors.Is(err, source.recordErr) {
		t.Fatalf("scheduled refresh must report record failure, got %v", err)
	}
}

func TestRefreshFeedLoadsOnlyRequestedSubscription(t *testing.T) {
	source := &subscriptionsStub{rows: []feeds.Subscription{
		{ID: 1, URL: "https://example.com/other"},
		{ID: 2, URL: "https://example.com/target"},
	}}
	service := New(source, &articleStub{}, DefaultInterval)
	service.client = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/target" {
			t.Fatalf("fetched unexpected feed: %s", req.URL)
		}
		return &http.Response{StatusCode: 304, Body: http.NoBody, Request: req}, nil
	})}
	if err := service.RefreshFeed(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	if source.listCalls != 0 || source.dueCalls != 0 ||
		len(source.dueFeedIDs) != 1 || source.dueFeedIDs[0] != 2 || len(source.results) != 1 {
		t.Fatalf("unexpected refresh: %+v", source)
	}
}
