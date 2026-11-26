package ingestion

import (
	"context"
	"errors"
	"fmt"
	"rss-reader/internal/articles"
	"rss-reader/internal/feeds"
	"time"
)

// DefaultInterval is the fallback interval between feed fetches.
const DefaultInterval = 30 * time.Minute

type subscriptions interface {
	List(context.Context) ([]feeds.Subscription, error)
	ListDue(context.Context) ([]feeds.Subscription, error)
	ListDueFeed(context.Context, int64) ([]feeds.Subscription, error)
	Record(context.Context, int64, feeds.FetchResult, time.Duration) error
	WithRefreshLock(context.Context, func() error) error
}
type articleWriter interface {
	Save(context.Context, articles.Article) error
}

// FetchedFeed contains the parsed articles and the result of retrieving their feed.
type FetchedFeed struct {
	feeds.FetchResult
	Articles []articles.Article
}

type feedFetcher interface {
	FetchFeed(context.Context, feeds.Subscription) FetchedFeed
}

// Service fetches subscriptions and saves their articles.
type Service struct {
	feeds    subscriptions
	articles articleWriter
	fetcher  feedFetcher
	interval time.Duration
}

// New creates a service for fetching and saving subscribed feeds.
func New(f subscriptions, a articleWriter, fetcher feedFetcher, interval time.Duration) *Service {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Service{feeds: f, articles: a, fetcher: fetcher, interval: interval}
}

// Refresh is an explicit manual refresh; it bypasses the due time but uses conditional HTTP.
func (s *Service) Refresh(ctx context.Context) error { return s.refresh(ctx, s.feeds.List, true) }

// RefreshDue performs one scheduled pass for subscriptions that are due.
// Individual download and parse failures are recorded on the subscription.
func (s *Service) RefreshDue(ctx context.Context) error {
	return s.refresh(ctx, s.feeds.ListDue, false)
}

// RefreshFeed attempts the initial fetch of a single due subscription.
func (s *Service) RefreshFeed(ctx context.Context, id int64) error {
	return s.refresh(ctx, func(ctx context.Context) ([]feeds.Subscription, error) {
		return s.feeds.ListDueFeed(ctx, id)
	}, true)
}

type articleSaveError struct{ error }

func (e articleSaveError) Unwrap() error { return e.error }

func (s *Service) refresh(ctx context.Context, load func(context.Context) ([]feeds.Subscription, error), reportFeedErrors bool) error {
	return s.feeds.WithRefreshLock(ctx, func() error {
		list, err := load(ctx)
		if err != nil {
			return err
		}

		var failures []error
		for _, f := range list {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failures, err)...)
			}

			result := s.fetch(ctx, f)
			if err := s.feeds.Record(ctx, f.ID, result, s.interval); err != nil {
				failures = append(failures, err)
			}
			var saveErr articleSaveError
			if result.Err != nil && (reportFeedErrors || errors.As(result.Err, &saveErr)) {
				failures = append(failures, fmt.Errorf("%s: %w", f.Title, result.Err))
			}
		}
		return errors.Join(append(failures, ctx.Err())...)
	})
}

func (s *Service) fetch(ctx context.Context, f feeds.Subscription) feeds.FetchResult {
	fetched := s.fetcher.FetchFeed(ctx, f)
	for _, item := range fetched.Articles {
		if err := s.articles.Save(ctx, item); err != nil {
			return feeds.FetchResult{
				Title:        f.Title,
				TTLMinutes:   f.TTLMinutes,
				ETag:         f.ETag,
				LastModified: f.LastModified,
				Err:          errors.Join(articleSaveError{err}, fetched.Err),
			}
		}
	}
	return fetched.FetchResult
}
