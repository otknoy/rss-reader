package main

import (
	"testing"
	"time"
)

func TestRefreshInterval(t *testing.T) {
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
}
