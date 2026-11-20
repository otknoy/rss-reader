package ingestion

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"rss-reader/internal/articles"
	"rss-reader/internal/feeds"
	"strings"

	readability "codeberg.org/readeck/go-readability/v2"
	"golang.org/x/net/html/charset"
)

// FetchArticle extracts a readable page without executing JavaScript or following page resources.
// The caller saves the result only on success; feed content is always retained.
func (s *Service) FetchArticle(ctx context.Context, rawURL string) (content articles.FullContent, err error) {
	if err := feeds.ValidateURL(rawURL); err != nil {
		return content, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return content, err
	}
	req.Header.Set("User-Agent", "PersonalRSSReader/0.1")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := s.client.Do(req)
	if err != nil {
		return content, err
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode != http.StatusOK {
		return content, fmt.Errorf("article HTTP %d", resp.StatusCode)
	}
	const maxArticleMiB = 16
	const maxArticleBytes = maxArticleMiB << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxArticleBytes+1))
	if err != nil {
		return content, err
	}
	if len(raw) > maxArticleBytes {
		return content, fmt.Errorf("記事が%d MiBを超えています", maxArticleMiB)
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(raw)
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || (mediaType != "text/html" && mediaType != "application/xhtml+xml") {
		return content, errors.New("HTMLの記事を取得できませんでした")
	}
	decoded, err := charset.NewReader(bytes.NewReader(raw), contentType)
	if err != nil {
		return content, err
	}
	parser := readability.NewParser()
	parser.MaxElemsToParse = 50000
	page, err := parser.Parse(decoded, resp.Request.URL)
	if err != nil {
		return content, fmt.Errorf("extract article: %w", err)
	}
	if page.Node == nil {
		return content, errors.New("記事の本文を抽出できませんでした")
	}
	var body, text strings.Builder
	if err := page.RenderHTML(&body); err != nil {
		return content, err
	}
	if err := page.RenderText(&text); err != nil {
		return content, err
	}
	if strings.TrimSpace(text.String()) == "" {
		return content, errors.New("記事の本文を抽出できませんでした")
	}
	// Some publishers provide only an Open Graph cover image in the document head.
	if cover := page.ImageURL(); cover != "" && !strings.Contains(body.String(), "<img") {
		// Escape publisher-controlled attributes before appending a cover image.
		prefix := `<figure><img src="` + html.EscapeString(cover) + `" alt=""></figure>`
		content.HTML = prefix + body.String()
	} else {
		content.HTML = body.String()
	}
	content.Title = page.Title()
	content.Text = text.String()
	content.URL = resp.Request.URL.String()
	return content, nil
}
