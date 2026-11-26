// Package main runs the RSS reader web server and maintenance commands.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	articlepostgres "rss-reader/internal/articles/postgres"
	"rss-reader/internal/config"
	"rss-reader/internal/feeds"
	feedpostgres "rss-reader/internal/feeds/postgres"
	"rss-reader/internal/ingestion"
	"rss-reader/internal/ingestion/httpfetch"
	"rss-reader/internal/reading"
	"rss-reader/internal/search"
	"rss-reader/internal/workspace"
	"rss-reader/migrations"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func openDB(ctx context.Context, key, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		closeResource(db)
		return nil, fmt.Errorf("database connection failed (%s)", key)
	}
	return db, nil
}

func run() error {
	command := "web"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if command != "web" && command != "fetch" && command != "migrate" && command != "healthcheck" {
		return errors.New("usage: reader [web|fetch [--due]|migrate|healthcheck]")
	}
	if command != "healthcheck" && len(os.Args) > 2 &&
		(command != "fetch" || len(os.Args) != 3 || os.Args[2] != "--due") {
		return errors.New("only fetch accepts --due")
	}

	c, err := config.LoadConfig(command)
	if err != nil {
		return err
	}
	if command == "healthcheck" {
		client := http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get("http://127.0.0.1:" + c.HealthcheckPort + "/healthz")
		if err != nil {
			return err
		}
		defer closeResource(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return errors.New("unhealthy")
		}
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if command == "migrate" {
		db, err := openDB(startup, "DATABASE_URL", c.DatabaseURL)
		if err != nil {
			return err
		}
		defer closeResource(db)
		return migrations.Run(startup, db)
	}

	fdb, err := openDB(startup, "FEEDS_DATABASE_URL", c.FeedsDatabaseURL)
	if err != nil {
		return err
	}
	defer closeResource(fdb)
	adb, err := openDB(startup, "ARTICLES_DATABASE_URL", c.ArticlesDatabaseURL)
	if err != nil {
		return err
	}
	defer closeResource(adb)

	fs, as := feedpostgres.New(fdb), articlepostgres.New(adb)
	client := httpfetch.New()
	fetcher := ingestion.New(fs, as, client, c.FetchInterval)

	if command == "fetch" {
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		var err error
		if len(os.Args) == 3 {
			err = fetcher.RefreshDue(fetchCtx)
		} else {
			err = fetcher.Refresh(fetchCtx)
		}
		if errors.Is(err, feeds.ErrRefreshInProgress) {
			log.Print("feed refresh skipped: already running")
			return nil
		}
		return err
	}

	app := newApplication(fs, as, fetcher, client)
	server := &http.Server{
		Addr:              c.ListenAddr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      150 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	done := make(chan error, 1)
	go func() {
		log.Printf("RSS Reader listening on %s", c.ListenAddr)
		done <- server.ListenAndServe()
	}()

	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func newApplication(fs *feedpostgres.Store, as *articlepostgres.Store, fetcher *ingestion.Service, client *httpfetch.Client) *application {
	return &application{
		feeds:         fs,
		subscriptions: feeds.NewSubscriptionService(fs, fetcher),
		articles:      as,
		workspace:     workspace.New(fs, search.New(as), as),
		reading:       reading.New(as, client),
		fetcher:       fetcher,
	}
}

func closeResource(resource io.Closer) {
	if err := resource.Close(); err != nil {
		log.Printf("close resource: %v", err)
	}
}
