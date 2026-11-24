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

type contentStoreStub struct {
	saved   []int64
	failed  []int64
	saveErr error
}

func (s *contentStoreStub) ClaimFullContent(context.Context, int64, bool) (string, error) {
	return "https://example.com/article", nil
}

func (s *contentStoreStub) SaveFullContent(_ context.Context, id int64, _ articles.FullContent) error {
	s.saved = append(s.saved, id)
	return s.saveErr
}

func (s *contentStoreStub) FailFullContent(_ context.Context, id int64, _ error) error {
	s.failed = append(s.failed, id)
	return nil
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
	store := &contentStoreStub{saveErr: errors.New("save failed")}
	if err := service.FetchContent(context.Background(), store, 4, true); err == nil {
		t.Fatal("expected save failure")
	}
	if len(store.failed) != 1 || store.failed[0] != 4 {
		t.Fatal("failed save did not release lease")
	}
}
