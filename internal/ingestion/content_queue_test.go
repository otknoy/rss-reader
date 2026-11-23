package ingestion

import (
	"context"
	"errors"
	"io"
	"net/http"
	"rss-reader/internal/articles"
	"strings"
	"testing"
)

type queueStub struct {
	pending []articles.Article
	busy    map[int64]bool
	saved   []int64
	failed  []int64
	saveErr error
}

func (s *queueStub) PendingFullContent(context.Context) ([]articles.Article, error) {
	return s.pending, nil
}

func (s *queueStub) ClaimFullContent(_ context.Context, id int64) (bool, error) {
	return !s.busy[id], nil
}

func (s *queueStub) SaveFullContent(_ context.Context, id int64, _ articles.FullContent) error {
	s.saved = append(s.saved, id)
	return s.saveErr
}

func (s *queueStub) FailFullContent(_ context.Context, id int64, _ error) error {
	s.failed = append(s.failed, id)
	return nil
}

func TestAutomaticFullContentContinuesAfterFailure(t *testing.T) {
	text := strings.Repeat("This is a readable article paragraph with enough meaningful content for extraction. ", 30)
	fixture := `<html><head><title>Article</title></head><body><article><p>` + text + `</p><p>` + text + `</p></article></body></html>`
	service := New(nil, nil, DefaultInterval)
	calls := 0
	service.client = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		status := 200
		if req.URL.Path == "/bad" {
			status = 403
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": {"text/html; charset=utf-8"}},
			Body:       io.NopCloser(strings.NewReader(fixture)),
			Request:    req,
		}, nil
	})}
	store := &queueStub{
		pending: []articles.Article{
			{ID: 1, URL: "https://example.com/bad"},
			{ID: 2, URL: "https://example.com/good"},
			{ID: 3, URL: "https://example.com/busy"},
		},
		busy: map[int64]bool{3: true},
	}
	if err := service.RefreshFullContent(context.Background(), store); err == nil {
		t.Fatal("expected partial failure")
	}
	if calls != 2 ||
		len(store.saved) != 1 ||
		store.saved[0] != 2 ||
		len(store.failed) != 1 ||
		store.failed[0] != 1 {
		t.Fatalf("calls=%d saved=%v failed=%v", calls, store.saved, store.failed)
	}
	// Processing saved jobs is independent from RSS fetching, so a 304 cannot starve retries.
	store.pending = nil
	if err := service.RefreshFullContent(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("empty queue triggered a network request")
	}
}

func TestContentLeaseReleasedWhenSaveFails(t *testing.T) {
	text := strings.Repeat("This is a meaningful article paragraph for extraction. ", 40)
	service := New(nil, nil, DefaultInterval)
	service.client = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": {"text/html"}},
			Body:       io.NopCloser(strings.NewReader("<html><body><article><p>" + text + "</p></article></body></html>")),
			Request:    req,
		}, nil
	})}
	store := &queueStub{saveErr: errors.New("save failed")}
	if _, err := service.FetchQueuedContent(context.Background(), store, articles.Article{ID: 4, URL: "https://example.com/article"}); err == nil {
		t.Fatal("expected save failure")
	}
	if len(store.failed) != 1 || store.failed[0] != 4 {
		t.Fatal("failed save did not release lease")
	}
}
