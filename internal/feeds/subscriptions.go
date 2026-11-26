package feeds

import "context"

type subscriptionWriter interface {
	AddToFolder(context.Context, string, *int64) (int64, error)
}

type initialRefresher interface {
	RefreshFeed(context.Context, int64) error
}

// SubscriptionService coordinates registration and the initial feed fetch.
type SubscriptionService struct {
	writer    subscriptionWriter
	refresher initialRefresher
}

func NewSubscriptionService(writer subscriptionWriter, refresher initialRefresher) *SubscriptionService {
	return &SubscriptionService{writer: writer, refresher: refresher}
}

type SubscribeResult struct {
	ID                int64
	InitialFetchError error
}

// Subscribe saves the subscription before attempting its initial fetch.
func (s *SubscriptionService) Subscribe(ctx context.Context, url string, folder *int64) (SubscribeResult, error) {
	id, err := s.writer.AddToFolder(ctx, url, folder)
	if err != nil {
		return SubscribeResult{}, err
	}
	return SubscribeResult{ID: id, InitialFetchError: s.refresher.RefreshFeed(ctx, id)}, nil
}
