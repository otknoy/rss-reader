// Package main runs the RSS reader web server and maintenance commands.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"rss-reader/internal/articles"
	"rss-reader/internal/feeds"
	"rss-reader/internal/ingestion"
	"rss-reader/internal/search"
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

func openDB(ctx context.Context, key string) (*sql.DB, error) {
	dsn := os.Getenv(key)
	if dsn == "" {
		return nil, fmt.Errorf("%s is required", key)
	}
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
	if command == "healthcheck" {
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			addr = ":8080"
		}
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return err
		}
		client := http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			return err
		}
		defer closeResource(resp.Body)
		if resp.StatusCode != 200 {
			return errors.New("unhealthy")
		}
		return nil
	}
	if command != "web" && command != "fetch" && command != "migrate" {
		return errors.New("usage: reader [web|fetch [--due]|migrate|healthcheck]")
	}
	if len(os.Args) > 2 && (command != "fetch" || len(os.Args) != 3 || os.Args[2] != "--due") {
		return errors.New("only fetch accepts --due")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if command == "migrate" {
		db, err := openDB(startup, "DATABASE_URL")
		if err != nil {
			return err
		}
		defer closeResource(db)
		return migrations.Run(startup, db)
	}

	fdb, err := openDB(startup, "FEEDS_DATABASE_URL")
	if err != nil {
		return err
	}
	defer closeResource(fdb)
	adb, err := openDB(startup, "ARTICLES_DATABASE_URL")
	if err != nil {
		return err
	}
	defer closeResource(adb)

	fs, as := feeds.New(fdb), articles.New(adb)
	interval, err := refreshInterval(os.Getenv("FETCH_INTERVAL"))
	if err != nil {
		return err
	}
	fetcher := ingestion.New(fs, as, interval)

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

	enabled, err := autoRefreshEnabled(os.Getenv("AUTO_REFRESH"))
	if err != nil {
		return err
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	if enabled {
		log.Printf("automatic feed refresh enabled: base interval=%s, due checks every 1m", interval)
		go func() {
			defer close(workerDone)
			pollFeeds(workerCtx, fetcher.RefreshDue)
		}()
	} else {
		close(workerDone)
	}
	defer func() {
		stopWorker()
		<-workerDone
	}()

	app := &application{feeds: fs, articles: as, search: search.New(as), fetcher: fetcher}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      150 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	done := make(chan error, 1)
	go func() {
		log.Printf("RSS Reader listening on %s", addr)
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

func closeResource(resource io.Closer) {
	if err := resource.Close(); err != nil {
		log.Printf("close resource: %v", err)
	}
}
