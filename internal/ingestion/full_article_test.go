package ingestion

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestFetchFullArticle(t *testing.T) {
	paragraph := strings.Repeat("これは本文のテストです。全文専用検索語と画像を含む記事を保存します。", 25)
	fixture := `<!doctype html><html lang="ja"><head><title>全文のテスト</title></head><body><nav>メニュー</nav><main><article><h1>全文のテスト</h1><p>` + paragraph + `</p><h2>写真と説明</h2><figure><img src="/images/photo.jpg"><figcaption>写真の説明</figcaption></figure><p>` + paragraph + `</p></article></main><script>alert(1)</script></body></html>`
	fixture += "<!--" + strings.Repeat("x", (16<<20)-len(fixture)-len("<!---->")) + "-->"
	service := New(nil, nil, DefaultInterval)
	service.client = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Accept") == "" {
			t.Fatal("missing HTML Accept header")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(fixture)), Request: req}, nil
	})}
	result, err := service.FetchArticle(context.Background(), "https://example.com/posts/entry")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "全文専用検索語") || !strings.Contains(result.HTML, "<img") || strings.Contains(result.HTML, "<script") || result.URL != "https://example.com/posts/entry" {
		t.Fatalf("unexpected extracted content: %+v", result)
	}
}

func TestFetchArticleRejectsFailures(t *testing.T) {
	for _, tc := range []struct {
		name              string
		status            int
		contentType, body string
	}{
		{"HTTP failure", 403, "text/html", "denied"},
		{"image instead of article", 200, "image/png", "not html"},
		{"empty page", 200, "text/html", "<html><body></body></html>"},
		{"oversized response", 200, "text/html", strings.Repeat("x", (16<<20)+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := New(nil, nil, DefaultInterval)
			service.client = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: req}, nil
			})}
			_, err := service.FetchArticle(context.Background(), "https://example.com/article")
			if err == nil {
				t.Fatal("expected extraction failure")
			}
			if tc.name == "oversized response" && err.Error() != "記事が16 MiBを超えています" {
				t.Fatalf("expected size-limit error, got %v", err)
			}
		})
	}
	service := New(nil, nil, DefaultInterval)
	if _, err := service.FetchArticle(context.Background(), "file:///etc/passwd"); err == nil {
		t.Fatal("non-HTTP URL accepted")
	}
}
