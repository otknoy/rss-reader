package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestRefreshConfiguration(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		want  time.Duration
		valid bool
	}{{
		"",
		30 * time.Minute,
		true,
	}, {
		"1h",
		time.Hour,
		true,
	}, {
		"1m",
		time.Minute,
		true,
	}, {
		"0",
		0,
		false,
	}, {
		"-1m",
		0,
		false,
	}, {
		"30s",
		0,
		false,
	}, {
		"invalid",
		0,
		false,
	}} {
		got, err := refreshInterval(tc.raw)
		if (err == nil) != tc.valid || got != tc.want {
			t.Errorf("%q: got %s %v", tc.raw, got, err)
		}
	}
	for _, tc := range []struct {
		raw         string
		want, valid bool
	}{{"", true, true}, {"true", true, true}, {"false", false, true}, {"invalid", false, false}} {
		got, err := autoRefreshEnabled(tc.raw)
		if (err == nil) != tc.valid || got != tc.want {
			t.Errorf("%q: got %v %v", tc.raw, got, err)
		}
	}
}

func TestPollingRetriesAndStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls atomic.Int32
		done := make(chan struct{})
		go func() {
			defer close(done)
			pollFeeds(ctx, func(context.Context) error {
				count := calls.Add(1)
				if count == 1 {
					return errors.New("temporary failure")
				}
				return nil
			})
		}()
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("initial calls=%d", calls.Load())
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("calls after retry=%d", calls.Load())
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("poller did not stop")
		}
		time.Sleep(2 * time.Minute)
		if calls.Load() != 2 {
			t.Fatal("poller ran after cancellation")
		}
	})
}

func TestPollingCancelsInFlightFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			pollFeeds(ctx, func(runCtx context.Context) error { <-runCtx.Done(); return runCtx.Err() })
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("in-flight fetch was not canceled")
		}
	})
}
