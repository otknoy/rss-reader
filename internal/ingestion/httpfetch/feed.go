package httpfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"rss-reader/internal/articles"
	"rss-reader/internal/feeds"
	"rss-reader/internal/ingestion"
	"strconv"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/mmcdole/gofeed/rss"
	"golang.org/x/net/html"
)

// FetchFeed downloads a feed and parses its articles and HTTP validators.
func (c *Client) FetchFeed(ctx context.Context, f feeds.Subscription) (result ingestion.FetchedFeed) {
	result.FetchResult = feeds.FetchResult{
		Title:        f.Title,
		TTLMinutes:   f.TTLMinutes,
		ETag:         f.ETag,
		LastModified: f.LastModified,
	}

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

	resp, err := c.http.Do(req)
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
	return ingestion.FetchedFeed{
		FetchResult: feeds.FetchResult{
			Title:        parsed.Title,
			TTLMinutes:   parsed.TTLMinutes,
			ETag:         resp.Header.Get("ETag"),
			LastModified: resp.Header.Get("Last-Modified"),
		},
		Articles: parsed.Items,
	}
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

func parseFeed(raw []byte, base *url.URL, f feeds.Subscription) (parsedFeed, error) {
	parser := gofeed.NewParser()
	parser.KeepOriginalFeed = true
	parsed, err := parser.ParseString(string(raw))
	if err != nil {
		return parsedFeed{}, errors.New("RSS / Atomを解析できませんでした")
	}
	title := plainText(parsed.Title)
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
		items = append(items, articles.Article{
			SourceID:    f.ID,
			SourceName:  title,
			ExternalID:  key,
			URL:         link,
			Title:       plainText(item.Title),
			BodyHTML:    body,
			BodyText:    plainText(body),
			PublishedAt: published,
		})
	}
	ttl := int64(0)
	if original, ok := parsed.OriginalFeed().(*rss.Feed); ok {
		ttl = parseTTL(original.TTL)
	}
	return parsedFeed{Title: title, Items: items, TTLMinutes: ttl}, nil
}

// plainText extracts readable text without executing or rendering feed HTML.
func plainText(raw string) string {
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
