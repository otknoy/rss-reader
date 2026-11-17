package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"rss-reader/internal/articles"
	"strings"
	"testing"
)

func TestReturnTo(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/?q=Go&view=unread", "/?q=Go&view=unread"},
		{"https://evil.example/", "/"},
		{"//evil.example/", "/"},
		{"/\\evil.example/", "/"},
		{"", "/"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(url.Values{"return_to": {tc.in}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if got := returnTo(r); got != tc.want {
			t.Errorf("returnTo(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCrossOriginMutationRejected(t *testing.T) {
	app := &application{}
	r := httptest.NewRequest(http.MethodPost, "http://reader.local/articles", strings.NewReader("url=https://example.com"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestTemplateEscapesArticleContent(t *testing.T) {
	w := httptest.NewRecorder()
	render(w, view{Result: articles.Result{Items: []articles.Article{{ID: 1, Title: `<script>alert(1)</script>`, BodyText: `<img src=x onerror=alert(2)>`, URL: "https://example.com"}}, Page: 1, Pages: 1}})
	body := w.Body.String()
	if strings.Contains(body, "<script>") || strings.Contains(body, "<img src=x") {
		t.Fatal("article content was rendered as HTML")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("escaped article title missing")
	}
}
