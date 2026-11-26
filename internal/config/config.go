// Package config loads runtime settings for reader commands.
package config

import (
	"fmt"
	"net"
	"os"
	"rss-reader/internal/ingestion"
	"time"
)

type Config struct {
	DatabaseURL         string
	FeedsDatabaseURL    string
	ArticlesDatabaseURL string
	ListenAddr          string
	HealthcheckPort     string
	FetchInterval       time.Duration
}

// LoadConfig reads the environment settings used by command.
func LoadConfig(command string) (c Config, err error) {
	switch command {
	case "migrate":
		c.DatabaseURL, err = requiredEnv("DATABASE_URL")
	case "web", "fetch":
		c.FeedsDatabaseURL, err = requiredEnv("FEEDS_DATABASE_URL")
		if err != nil {
			return Config{}, err
		}
		c.ArticlesDatabaseURL, err = requiredEnv("ARTICLES_DATABASE_URL")
		if err != nil {
			return Config{}, err
		}
		c.FetchInterval, err = refreshInterval(os.Getenv("FETCH_INTERVAL"))
		if command == "web" {
			c.ListenAddr = listenAddr()
		}
	case "healthcheck":
		_, c.HealthcheckPort, err = net.SplitHostPort(listenAddr())
	default:
		return Config{}, fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		return Config{}, err
	}
	return c, nil
}

func requiredEnv(key string) (string, error) {
	value := os.Getenv(key)
	if value == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return value, nil
}

func listenAddr() string {
	if addr := os.Getenv("LISTEN_ADDR"); addr != "" {
		return addr
	}
	return ":8080"
}

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
