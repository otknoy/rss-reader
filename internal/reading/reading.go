// Package reading coordinates article display and full-content retrieval.
package reading

import (
	"context"
	"errors"
	"log"
	"rss-reader/internal/articles"
	"time"
)

type articleStore interface {
	Open(context.Context, int64) (articles.Article, bool, error)
	RecordDisplay(context.Context, int64, string) error
	FullContentStatus(context.Context, int64) (articles.ContentStatus, error)
	ClaimFullContent(context.Context, int64, bool) (string, error)
	SaveFullContent(context.Context, int64, articles.FullContent) error
	FailFullContent(context.Context, int64, error) error
}

type contentFetcher interface {
	FetchArticle(context.Context, string) (articles.FullContent, error)
}

// Service coordinates article display and full-content retrieval.
type Service struct {
	articles articleStore
	fetcher  contentFetcher
}

// New connects article storage to the content fetcher.
func New(articles articleStore, fetcher contentFetcher) *Service {
	return &Service{articles: articles, fetcher: fetcher}
}

// Open marks the article as read and returns the article and whether its state changed.
func (s *Service) Open(ctx context.Context, id int64) (articles.Article, bool, error) {
	return s.articles.Open(ctx, id)
}

// Display records an article view and attempts to retrieve full content.
func (s *Service) Display(ctx context.Context, id int64, viewID string) error {
	if err := s.articles.RecordDisplay(ctx, id, viewID); err != nil {
		return err
	}
	s.tryFetchContent(ctx, id, false)
	return nil
}

// RetryContent attempts to retrieve full content without recording another view.
func (s *Service) RetryContent(ctx context.Context, id int64) {
	s.tryFetchContent(ctx, id, true)
}

// ContentStatus returns the stored state of article extraction.
func (s *Service) ContentStatus(ctx context.Context, id int64) (articles.ContentStatus, error) {
	return s.articles.FullContentStatus(ctx, id)
}

func (s *Service) tryFetchContent(ctx context.Context, id int64, retry bool) {
	if err := s.fetchContent(ctx, id, retry); err != nil {
		log.Printf("fetch opened article %d: %v", id, err)
	}
}

func (s *Service) fetchContent(ctx context.Context, id int64, retry bool) error {
	url, err := s.articles.ClaimFullContent(ctx, id, retry)
	if err != nil || url == "" {
		return err
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	content, err := s.fetcher.FetchArticle(fetchCtx, url)
	if err == nil {
		err = s.articles.SaveFullContent(fetchCtx, id, content)
	}
	if err == nil {
		return nil
	}

	cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	return errors.Join(err, s.articles.FailFullContent(cleanup, id, err))
}
