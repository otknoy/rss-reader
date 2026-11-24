package ingestion

import (
	"context"
	"errors"
	"rss-reader/internal/articles"
	"time"
)

type contentStore interface {
	ClaimFullContent(context.Context, int64, bool) (string, error)
	SaveFullContent(context.Context, int64, articles.FullContent) error
	FailFullContent(context.Context, int64, error) error
}

// FetchContent attempts extraction for a display or an explicit retry.
func (s *Service) FetchContent(ctx context.Context, store contentStore, id int64, retry bool) error {
	url, err := store.ClaimFullContent(ctx, id, retry)
	if err != nil || url == "" {
		return err
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	content, err := s.FetchArticle(fetchCtx, url)
	if err == nil {
		err = store.SaveFullContent(fetchCtx, id, content)
	}
	if err == nil {
		return nil
	}

	cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	return errors.Join(err, store.FailFullContent(cleanup, id, err))
}
