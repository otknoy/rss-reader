package ingestion

import (
	"context"
	"errors"
	"rss-reader/internal/articles"
	"rss-reader/internal/feeds"
	"testing"
	"time"
)

type feedFetcherFunc func(context.Context, feeds.Subscription) FetchedFeed

func (f feedFetcherFunc) FetchFeed(ctx context.Context, subscription feeds.Subscription) FetchedFeed {
	return f(ctx, subscription)
}

type articleStub struct {
	saved []articles.Article
	err   error
}

func (s *articleStub) Save(_ context.Context, a articles.Article) error {
	s.saved = append(s.saved, a)
	return s.err
}

func TestRefreshDueKeepsValidatorsWhenArticleSaveFails(t *testing.T) {
	previous := feeds.Subscription{ID: 1, TTLMinutes: 60, ETag: "old", LastModified: "old-date"}
	writer := &articleStub{err: errors.New("database unavailable")}
	source := &subscriptionsStub{rows: []feeds.Subscription{previous}}
	fetcher := feedFetcherFunc(func(context.Context, feeds.Subscription) FetchedFeed {
		return FetchedFeed{
			TTLMinutes: 90, ETag: "new", LastModified: "new-date",
			Articles: []articles.Article{{Title: "new article"}},
		}
	})
	service := New(source, writer, fetcher, DefaultInterval)

	if err := service.RefreshDue(t.Context()); !errors.Is(err, writer.err) {
		t.Fatalf("scheduled refresh must report save failure, got %v", err)
	}
	if len(source.results) != 1 {
		t.Fatalf("expected one saved result, got %d", len(source.results))
	}
	result := source.results[0]
	if !errors.Is(result.Err, writer.err) || result.ETag != previous.ETag ||
		result.LastModified != previous.LastModified || result.TTLMinutes != previous.TTLMinutes {
		t.Fatalf("unsafe validators: %+v", result)
	}
}

type subscriptionsStub struct {
	rows                []feeds.Subscription
	results             []feeds.FetchResult
	interval            time.Duration
	listCalls, dueCalls int
	dueFeedIDs          []int64
	busy                bool
	recordErr           error
}

func (s *subscriptionsStub) List(context.Context) ([]feeds.Subscription, error) {
	s.listCalls++
	return s.rows, nil
}

func (s *subscriptionsStub) ListDue(context.Context) ([]feeds.Subscription, error) {
	s.dueCalls++
	return s.rows, nil
}

func (s *subscriptionsStub) ListDueFeed(_ context.Context, id int64) ([]feeds.Subscription, error) {
	s.dueFeedIDs = append(s.dueFeedIDs, id)
	for _, feed := range s.rows {
		if feed.ID == id {
			return []feeds.Subscription{feed}, nil
		}
	}
	return nil, nil
}

func (s *subscriptionsStub) Record(_ context.Context, _ int64, r feeds.FetchResult, interval time.Duration) error {
	s.results = append(s.results, r)
	s.interval = interval
	return s.recordErr
}

func (s *subscriptionsStub) WithRefreshLock(_ context.Context, run func() error) error {
	if s.busy {
		return feeds.ErrRefreshInProgress
	}
	return run()
}

func TestRefreshContinuesAfterFeedFailure(t *testing.T) {
	source := &subscriptionsStub{rows: []feeds.Subscription{
		{ID: 1, URL: "https://example.com/bad"},
		{ID: 2, URL: "https://example.com/good"},
	}}
	fetcher := feedFetcherFunc(func(_ context.Context, subscription feeds.Subscription) FetchedFeed {
		if subscription.ID == 1 {
			return FetchedFeed{Err: errors.New("HTTP 503")}
		}
		return FetchedFeed{}
	})
	service := New(source, &articleStub{}, fetcher, 45*time.Minute)
	if err := service.RefreshDue(t.Context()); err != nil {
		t.Fatalf("scheduled refresh failed after recording a feed error: %v", err)
	}
	if source.dueCalls != 1 ||
		source.listCalls != 0 ||
		len(source.results) != 2 ||
		source.results[0].Err == nil ||
		source.results[1].Err != nil ||
		source.interval != 45*time.Minute {
		t.Fatalf("unexpected processing: %+v", source)
	}
	source.busy = true
	if err := service.Refresh(context.Background()); !errors.Is(err, feeds.ErrRefreshInProgress) {
		t.Fatalf("got %v", err)
	}
	if source.listCalls != 0 {
		t.Fatal("manual refresh ignored lock")
	}
	source.busy = false
	if err := service.Refresh(t.Context()); err == nil {
		t.Fatal("manual refresh must report feed failure")
	}
	if source.listCalls != 1 {
		t.Fatal("manual refresh should list all feeds")
	}
	source.recordErr = errors.New("could not record fetch result")
	if err := service.RefreshDue(t.Context()); !errors.Is(err, source.recordErr) {
		t.Fatalf("scheduled refresh must report record failure, got %v", err)
	}
}

func TestRefreshFeedLoadsOnlyRequestedSubscription(t *testing.T) {
	source := &subscriptionsStub{rows: []feeds.Subscription{
		{ID: 1, URL: "https://example.com/other"},
		{ID: 2, URL: "https://example.com/target"},
	}}
	fetcher := feedFetcherFunc(func(_ context.Context, subscription feeds.Subscription) FetchedFeed {
		if subscription.ID != 2 {
			t.Fatalf("fetched unexpected subscription: %d", subscription.ID)
		}
		return FetchedFeed{}
	})
	service := New(source, &articleStub{}, fetcher, DefaultInterval)
	if err := service.RefreshFeed(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	if source.listCalls != 0 || source.dueCalls != 0 ||
		len(source.dueFeedIDs) != 1 || source.dueFeedIDs[0] != 2 || len(source.results) != 1 {
		t.Fatalf("unexpected refresh: %+v", source)
	}
}
