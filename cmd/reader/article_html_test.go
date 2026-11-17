package main

import (
	"net/http"
	"net/http/httptest"
	"rss-reader/internal/articles"
	"strings"
	"testing"
)

func TestRichArticleSanitizesAndResolvesURLs(t *testing.T) {
	raw := `<h2>見出し</h2><p>本文<strong>強調</strong></p><img src="placeholder.gif" data-src="/images/photo.jpg" onerror="alert(1)" style="position:fixed"><a href="../other">リンク</a><a href="javascript:alert(2)">不正なリンク</a><img src="data:image/svg+xml,evil"><script>alert(3)</script><iframe src="https://evil.example"></iframe><svg onload="alert(4)"></svg><form action="/articles/1/delete"><input type="submit"></form>`
	body := string(richArticle(articles.Article{URL: "https://example.com/posts/entry", BodyHTML: raw}))
	for _, unsafe := range []string{"<script", "<iframe", "<svg", "<form", "<input", "onerror=", "onload=", "style=", "javascript:", "data:image", "data-src="} {
		if strings.Contains(body, unsafe) {
			t.Errorf("unsafe content %q in %s", unsafe, body)
		}
	}
	for _, safe := range []string{"<h2>見出し</h2>", "<strong>強調</strong>", `src="https://example.com/images/photo.jpg"`, `href="https://example.com/other"`} {
		if !strings.Contains(body, safe) {
			t.Errorf("missing %q in %s", safe, body)
		}
	}
	if strings.Count(body, `src="https://example.com/images/photo.jpg"`) != 1 || strings.Contains(body, "placeholder.gif") {
		t.Fatal("lazy image URL was not normalized")
	}
}

func TestRichArticlePrefersFullContent(t *testing.T) {
	a := articles.Article{URL: "https://example.com/rss-item", BodyHTML: "<p>要約</p>", FullBodyHTML: `<p>取得した全文</p><img src="photo.jpg">`, FullSourceURL: "https://cdn.example.com/posts/page"}
	body := string(richArticle(a))
	if strings.Contains(body, "要約") || !strings.Contains(body, "取得した全文") || !strings.Contains(body, "https://cdn.example.com/posts/photo.jpg") {
		t.Fatalf("wrong source: %s", body)
	}
}

func TestDetailRendersRichContent(t *testing.T) {
	w := httptest.NewRecorder()
	render(w, view{Detail: &articles.Article{ID: 1, URL: "https://example.com/page", Title: "記事", BodyHTML: `<h2>本文見出し</h2><img src="/image.png" onerror="alert(1)">`}})
	body := w.Body.String()
	if !strings.Contains(body, "<h2>本文見出し</h2>") || !strings.Contains(body, `src="https://example.com/image.png"`) || strings.Contains(body, "/fetch-content") || strings.Contains(body, "onerror=") {
		t.Fatalf("unexpected detail HTML: %s", body)
	}
}

func TestImagePolicy(t *testing.T) {
	app := &application{}
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	policy := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "img-src https: http:") || !strings.Contains(policy, "default-src 'none'") {
		t.Fatalf("unexpected CSP: %s", policy)
	}
}
