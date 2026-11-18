package ingestion

import (
	"context"
	"errors"
	"fmt"
	"rss-reader/internal/articles"
	"time"
)

type contentStore interface {
	PendingFullContent(context.Context) ([]articles.Article, error)
	ClaimFullContent(context.Context, int64) (bool, error)
	SaveFullContent(context.Context, int64, articles.FullContent) error
	FailFullContent(context.Context, int64, error) error
}

// RefreshFullContent processes up to ten persisted extraction jobs in the current process.
// Failed jobs are retried later, including when RSS itself responds with 304.
func (s *Service) RefreshFullContent(ctx context.Context, store contentStore) error {
	pending, err := store.PendingFullContent(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, item := range pending {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if _, err := s.FetchQueuedContent(ctx, store, item); err != nil {
			failures = append(failures, fmt.Errorf("article %d: %w", item.ID, err))
		}
	}
	return errors.Join(failures...)
}

// FetchQueuedContent fetches an article when its scheduled attempt is due.
func (s *Service) FetchQueuedContent(ctx context.Context, store contentStore, item articles.Article) (bool, error) {
	claimed, err := store.ClaimFullContent(ctx, item.ID)
	if err != nil || !claimed {
		return false, err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	content, err := s.FetchArticle(fetchCtx, item.URL)
	if err == nil {
		err = store.SaveFullContent(fetchCtx, item.ID, content)
	}
	if err == nil {
		return true, nil
	}
	cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	return false, errors.Join(err, store.FailFullContent(cleanup, item.ID, err))
}
