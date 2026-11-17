package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"rss-reader/internal/feeds"
	"rss-reader/internal/ingestion"
	"strconv"
	"time"
)

func refreshInterval(raw string) (time.Duration, error) {
	if raw == "" {
		return ingestion.DefaultInterval, nil
	}
	interval, err := time.ParseDuration(raw)
	if err != nil || interval < time.Minute {
		return 0, fmt.Errorf("FETCH_INTERVAL must be a duration of at least 1m")
	}
	return interval, nil
}

func autoRefreshEnabled(raw string) (bool, error) {
	if raw == "" {
		return true, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("AUTO_REFRESH must be true or false")
	}
	return enabled, nil
}

// pollFeeds only schedules execution. The same one-pass operation is used by fetch --due.
func pollFeeds(ctx context.Context, refresh func(context.Context) error) {
	for ctx.Err() == nil {
		runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		err := refresh(runCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, feeds.ErrRefreshInProgress) {
			log.Printf("automatic feed refresh: %v", err)
		}
		timer := time.NewTimer(time.Minute)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
