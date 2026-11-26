package httpfetch

import (
	"io"
	"net/http"
	"net/url"
	"rss-reader/internal/feeds"
	"strings"
	"testing"
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
			client := New()
			client.http = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
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
			result := client.FetchFeed(t.Context(), previous)
			switch status {
			case http.StatusOK:
				if result.Err != nil || result.TTLMinutes != 90 || result.ETag != `"new"` || len(result.Articles) != 1 {
					t.Fatalf("result=%+v saved=%v", result, result.Articles)
				}
				if result.LastModified != "" {
					t.Fatal("200 response should clear old validators when absent")
				}
			case http.StatusNotModified:
				if result.Err != nil ||
					result.TTLMinutes != 60 ||
					result.ETag != `"new"` ||
					result.LastModified != previous.LastModified ||
					len(result.Articles) != 0 {
					t.Fatalf("result=%+v saved=%v", result, result.Articles)
				}
			default:
				if result.Err == nil ||
					result.TTLMinutes != 60 ||
					result.ETag != previous.ETag ||
					len(result.Articles) != 0 {
					t.Fatalf("result=%+v saved=%v", result, result.Articles)
				}
			}
		})
	}
}
