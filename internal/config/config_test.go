package config

import (
	"testing"
	"time"
)

func TestLoadConfigReadsOnlyCommandSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "admin")
	t.Setenv("FEEDS_DATABASE_URL", "feeds")
	t.Setenv("ARTICLES_DATABASE_URL", "articles")
	t.Setenv("LISTEN_ADDR", ":9000")
	t.Setenv("FETCH_INTERVAL", "45m")

	for _, tc := range []struct {
		command string
		want    Config
	}{
		{"migrate", Config{DatabaseURL: "admin"}},
		{"healthcheck", Config{HealthcheckPort: "9000"}},
		{"fetch", Config{FeedsDatabaseURL: "feeds", ArticlesDatabaseURL: "articles", FetchInterval: 45 * time.Minute}},
		{"web", Config{FeedsDatabaseURL: "feeds", ArticlesDatabaseURL: "articles", ListenAddr: ":9000", FetchInterval: 45 * time.Minute}},
	} {
		got, err := LoadConfig(tc.command)
		if got != tc.want || err != nil {
			t.Errorf("%s: got %+v, %v; want %+v", tc.command, got, err, tc.want)
		}
	}
}

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
