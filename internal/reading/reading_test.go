package reading

import (
	"context"
	"errors"
	"rss-reader/internal/articles"
	"testing"
)

type contentStoreStub struct {
	saved   []int64
	failed  []int64
	saveErr error
	viewIDs []string
	retries []bool
}

func (s *contentStoreStub) Open(context.Context, int64) (articles.Article, bool, error) {
	return articles.Article{}, false, nil
}

func (s *contentStoreStub) FullContentStatus(context.Context, int64) (articles.ContentStatus, error) {
	return articles.ContentStatus{}, nil
}

func (s *contentStoreStub) RecordDisplay(_ context.Context, _ int64, viewID string) error {
	s.viewIDs = append(s.viewIDs, viewID)
	return nil
}

func (s *contentStoreStub) ClaimFullContent(_ context.Context, _ int64, retry bool) (string, error) {
	s.retries = append(s.retries, retry)
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

type contentFetcherStub struct{}

func (contentFetcherStub) FetchArticle(_ context.Context, url string) (articles.FullContent, error) {
	return articles.FullContent{Text: "Article text", URL: url}, nil
}

func TestContentLeaseReleasedWhenSaveFails(t *testing.T) {
	store := &contentStoreStub{saveErr: errors.New("save failed")}
	service := New(store, contentFetcherStub{})
	if err := service.fetchContent(t.Context(), 4, true); !errors.Is(err, store.saveErr) {
		t.Fatalf("fetchContent() = %v, want %v", err, store.saveErr)
	}
	if len(store.failed) != 1 || store.failed[0] != 4 {
		t.Fatal("failed save did not release lease")
	}
}

func TestRetryContentDoesNotRecordDisplay(t *testing.T) {
	store := &contentStoreStub{}
	service := New(store, contentFetcherStub{})

	if err := service.Display(t.Context(), 4, "view-id"); err != nil {
		t.Fatal(err)
	}
	service.RetryContent(t.Context(), 4)

	if len(store.viewIDs) != 1 || store.viewIDs[0] != "view-id" {
		t.Fatalf("unexpected view IDs: %v", store.viewIDs)
	}
	if len(store.retries) != 2 || store.retries[0] || !store.retries[1] {
		t.Fatalf("unexpected retry flags: %v", store.retries)
	}
}
