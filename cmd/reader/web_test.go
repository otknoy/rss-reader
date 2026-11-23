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
	render(w, view{Result: articles.Result{
		Items: []articles.Article{{
			ID:       1,
			Title:    `<script>alert(1)</script>`,
			BodyText: `<img src=x onerror=alert(2)>`,
			URL:      "https://example.com",
		}},
		Page:  1,
		Pages: 1,
	}})
	body := w.Body.String()
	if strings.Contains(body, "<script>") || strings.Contains(body, "<img src=x") {
		t.Fatal("article content was rendered as HTML")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("escaped article title missing")
	}
}

func TestArticleURLPreservesListContext(t *testing.T) {
	selected := articleURL("/?q=Go+言語&feed=3&view=unread&page=2&message=saved", 42)
	parsed, err := url.Parse(selected)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if parsed.Path != "/" ||
		q.Get("article") != "42" ||
		q.Get("q") != "Go 言語" ||
		q.Get("feed") != "3" ||
		q.Get("view") != "unread" ||
		q.Get("page") != "2" ||
		q.Has("message") {
		t.Fatalf("bad selection URL: %s", selected)
	}
	closed, _ := url.Parse(articleURL(selected, 0))
	if closed.Query().Has("article") || closed.Query().Get("q") != "Go 言語" {
		t.Fatalf("bad close URL: %s", closed)
	}
	if got := articleURL("/articles/42", 0); got != "/" {
		t.Fatalf("legacy detail close: %s", got)
	}
}

func TestWorkspaceIncludesListAndReader(t *testing.T) {
	item := articles.Article{
		ID:       42,
		URL:      "https://example.com/entry",
		Title:    "選択中の記事",
		BodyText: "本文",
	}
	w := httptest.NewRecorder()
	render(w, view{
		Detail:   &item,
		ListURL:  "/?view=unread",
		ReturnTo: "/?view=unread&article=42",
		Result:   articles.Result{Items: []articles.Article{item}},
	})
	body := w.Body.String()
	for _, part := range []string{
		`id="list-pane"`,
		`id="reader-pane"`,
		`data-reading-id="42"`,
		`data-article-id="42"`,
		`aria-current="true"`,
		`data-workspace-link`,
		`src="/assets/reader.js"`,
	} {
		if !strings.Contains(body, part) {
			t.Errorf("missing %s", part)
		}
	}
	if strings.Count(body, "選択中の記事") != 2 {
		t.Fatal("list and selected article were not both rendered")
	}
}

func TestWorkspaceScript(t *testing.T) {
	app := &application{}
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/reader.js", nil))
	if w.Code != http.StatusOK ||
		!strings.Contains(w.Header().Get("Content-Type"), "javascript") ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("script response: %d %v", w.Code, w.Header())
	}
}

func TestManualContentFetchRouteRemoved(t *testing.T) {
	app := &application{}
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/articles/1/fetch-content", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("manual extraction route still exists: %d", w.Code)
	}
}
