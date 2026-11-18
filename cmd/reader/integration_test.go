package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"rss-reader/internal/articles"
	"rss-reader/internal/feeds"
	"rss-reader/internal/ingestion"
	"rss-reader/internal/search"
	"rss-reader/migrations"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Every run gets its own database. No development tables are cleared.
func TestPrototypeIntegration(t *testing.T) {
	adminDSN := os.Getenv("INTEGRATION_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("run via docker compose --profile test run --build --rm test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close admin database: %v", err)
		}
	}()
	name := fmt.Sprintf("reader_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	var connections []*sql.DB
	defer func() {
		for _, db := range connections {
			if err := db.Close(); err != nil {
				t.Errorf("close database: %v", err)
			}
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(cleanup, `DROP DATABASE `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	connect := func(dsn string) *sql.DB {
		t.Helper()
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		db, err := sql.Open("pgx", u.String())
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, db)
		return db
	}
	owner := connect(adminDSN)
	if err := migrations.Run(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, owner); err != nil {
		t.Fatalf("migration rerun: %v", err)
	}
	fdb := connect(os.Getenv("FEEDS_DATABASE_URL"))
	adb := connect(os.Getenv("ARTICLES_DATABASE_URL"))
	for _, tc := range []struct {
		db    *sql.DB
		table string
	}{{fdb, "articles.entries"}, {adb, "feeds.subscriptions"}} {
		_, err := tc.db.ExecContext(ctx, `SELECT * FROM `+tc.table)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "42501" {
			t.Fatalf("expected permission denial for %s, got %v", tc.table, err)
		}
	}
	fs, as := feeds.New(fdb), articles.New(adb)
	if err := fs.Add(ctx, "https://example.com/feed.xml"); err != nil {
		t.Fatal(err)
	}
	list, err := fs.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("feeds=%v, err=%v", list, err)
	}
	t.Run("durable TTL schedule", func(t *testing.T) {
		feedID := list[0].ID
		for _, tc := range []struct {
			ttl   int64
			delay time.Duration
		}{{60, time.Hour}, {10, 30 * time.Minute}, {0, 30 * time.Minute}} {
			metadata := feeds.FetchResult{Title: "scheduled", TTLMinutes: tc.ttl, ETag: `"v1"`, LastModified: "Mon, 16 Nov 2026 00:00:00 GMT"}
			if err := fs.Record(ctx, feedID, metadata, 30*time.Minute); err != nil {
				t.Fatal(err)
			}
			// A fresh store / connection sees the persisted due time, as after a restart.
			restarted := feeds.New(connect(os.Getenv("FEEDS_DATABASE_URL")))
			recorded, err := restarted.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			f := recorded[0]
			if f.CheckedAt == nil || f.NextFetchAt == nil || f.LastSuccessAt == nil || f.TTLMinutes != tc.ttl || f.NextFetchAt.Sub(*f.CheckedAt) != tc.delay {
				t.Fatalf("bad schedule: %+v", f)
			}
			if due, err := restarted.ListDue(ctx); err != nil || len(due) != 0 {
				t.Fatalf("fetched too early: %v %v", due, err)
			}
			if err := fs.Add(ctx, f.URL); err != nil {
				t.Fatal(err)
			}
			if due, err := fs.ListDue(ctx); err != nil || len(due) != 0 {
				t.Fatalf("duplicate subscription reset due time: %v %v", due, err)
			}
			metadata.Err = errors.New("temporary fetch failure")
			if err := fs.Record(ctx, feedID, metadata, 30*time.Minute); err != nil {
				t.Fatal(err)
			}
			failed, err := fs.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if failed[0].LastError == "" || !failed[0].LastSuccessAt.Equal(*f.LastSuccessAt) || failed[0].NextFetchAt.Sub(*failed[0].CheckedAt) != tc.delay {
				t.Fatalf("failure lost schedule or success state: %+v", failed[0])
			}
		}
		if _, err := owner.ExecContext(ctx, `UPDATE feeds.subscriptions SET next_fetch_at=now()-interval '1 second' WHERE id=$1`, feedID); err != nil {
			t.Fatal(err)
		}
		if due, err := fs.ListDue(ctx); err != nil || len(due) != 1 {
			t.Fatalf("due feed missing: %v %v", due, err)
		}
		if err := fs.Remove(ctx, feedID); err != nil {
			t.Fatal(err)
		}
		if due, err := fs.ListDue(ctx); err != nil || len(due) != 0 {
			t.Fatalf("inactive feed scheduled: %v %v", due, err)
		}
		if err := fs.Add(ctx, list[0].URL); err != nil {
			t.Fatal(err)
		}
		if due, err := fs.ListDue(ctx); err != nil || len(due) != 1 {
			t.Fatalf("resubscription not scheduled: %v %v", due, err)
		}
	})
	t.Run("cross-process refresh lock", func(t *testing.T) {
		other := feeds.New(connect(os.Getenv("FEEDS_DATABASE_URL")))
		canceled, cancelLock := context.WithCancel(ctx)
		defer cancelLock()
		callbackErr := errors.New("fetch failed")
		err := fs.WithRefreshLock(canceled, func() error {
			called := false
			if err := other.WithRefreshLock(ctx, func() error { called = true; return nil }); !errors.Is(err, feeds.ErrRefreshInProgress) || called {
				t.Fatalf("overlapping refresh allowed: %v", err)
			}
			cancelLock()
			return callbackErr
		})
		if !errors.Is(err, callbackErr) {
			t.Fatalf("callback error lost: %v", err)
		}
		if err := other.WithRefreshLock(ctx, func() error { return nil }); err != nil {
			t.Fatalf("lock leaked after canceled/error run: %v", err)
		}
	})

	article := articles.Article{SourceID: list[0].ID, ExternalID: "entry-1", SourceName: "テスト購読", URL: "https://example.com/one", Title: "Goの記事", BodyText: "日本語の全文検索 100% foo_bar"}
	if err := as.Save(ctx, article); err != nil {
		t.Fatal(err)
	}
	result, err := as.Search(ctx, articles.Query{})
	if err != nil || result.Total != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	articleID := result.Items[0].ID
	for _, kind := range []string{"read", "favorite"} {
		if err := as.SetState(ctx, articleID, kind, true); err != nil {
			t.Fatal(err)
		}
	}
	article.Title = "更新された記事"
	if err := as.Save(ctx, article); err != nil {
		t.Fatal(err)
	}
	saved, err := as.Get(ctx, articleID)
	if err != nil || !saved.IsRead || !saved.Favorite {
		t.Fatalf("state lost: %+v, %v", saved, err)
	}
	full := articles.FullContent{Title: "取得した記事", HTML: `<h2>全文</h2><p>全文だけのキーワード</p><img src="/photo.jpg">`, Text: "全文だけのキーワード", URL: "https://example.com/one"}
	if err := as.SaveFullContent(ctx, articleID, full); err != nil {
		t.Fatal(err)
	}
	// RSS refresh must not erase an explicitly fetched full article.
	if err := as.Save(ctx, article); err != nil {
		t.Fatal(err)
	}
	complete, err := as.Get(ctx, articleID)
	if err != nil || complete.FullBodyHTML != full.HTML || complete.FullFetchedAt == nil || complete.BodyText != article.BodyText || !complete.IsRead || !complete.Favorite {
		t.Fatalf("full content or state lost: %+v %v", complete, err)
	}
	if found, err := as.Search(ctx, articles.Query{Text: full.Text}); err != nil || found.Total != 1 {
		t.Fatalf("full article is not searchable: %+v %v", found, err)
	}
	if err := fs.Remove(ctx, list[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"日本語", "100%", "foo_bar"} {
		r, err := as.Search(ctx, articles.Query{Text: q})
		if err != nil || r.Total != 1 {
			t.Fatalf("query %q: %+v %v", q, r, err)
		}
	}
	r, err := as.Search(ctx, articles.Query{Text: "not_found"})
	if err != nil || r.Total != 0 {
		t.Fatalf("unexpected match: %+v %v", r, err)
	}
	app := &application{feeds: fs, articles: as, search: search.New(as), fetcher: ingestion.New(fs, as, ingestion.DefaultInterval)}
	request := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, req)
		return w
	}
	if w := request("GET", "/?q=日本語", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "更新された記事") {
		t.Fatalf("search HTTP: %d %s", w.Code, w.Body.String())
	}
	if w := request("GET", fmt.Sprintf("/articles/%d", articleID), nil); w.Code != 200 || !strings.Contains(w.Body.String(), full.Text) {
		t.Fatalf("detail HTTP: %d", w.Code)
	}
	selection := fmt.Sprintf("/?article=%d&q=日本語&feed=%d", articleID, article.SourceID)
	if w := request("GET", selection, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `data-reading-id="`+fmt.Sprint(articleID)+`"`) || !strings.Contains(w.Body.String(), `id="list-pane"`) {
		t.Fatalf("workspace selection: %d %s", w.Code, w.Body.String())
	}
	if w := request("GET", "/?article=invalid", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid selection status: %d", w.Code)
	}
	if w := request("POST", "/articles", url.Values{"url": {"https://example.com/manual"}, "title": {"手動登録記事"}, "body": {"保存した本文"}}); w.Code != 303 {
		t.Fatalf("manual add HTTP: %d %s", w.Code, w.Body.String())
	}
	if w := request("POST", fmt.Sprintf("/articles/%d/state", articleID), url.Values{"kind": {"read"}, "value": {"false"}}); w.Code != 303 {
		t.Fatalf("state HTTP: %d", w.Code)
	}
	if w := request("GET", "/?view=unread", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "更新された記事") {
		t.Fatalf("unread HTTP: %d", w.Code)
	}
	if w := request("POST", fmt.Sprintf("/articles/%d/delete", articleID), url.Values{"return_to": {selection}}); w.Code != http.StatusNotFound {
		t.Fatalf("article deletion is still exposed over HTTP: %d", w.Code)
	}
	if w := request("GET", selection, nil); w.Code != http.StatusOK {
		t.Fatalf("rejected deletion changed article: %d", w.Code)
	}
	// Preserve support for historical deleted rows without exposing deletion in the Web UI.
	if err := as.Delete(ctx, articleID); err != nil {
		t.Fatal(err)
	}

	if err := as.Save(ctx, article); err != nil {
		t.Fatal(err)
	}
	if found, err := as.Search(ctx, articles.Query{Text: "更新された記事"}); err != nil || found.Total != 0 {
		t.Fatalf("deleted article returned after refetch: %+v %v", found, err)
	}
	if w := request("GET", fmt.Sprintf("/articles/%d", articleID), nil); w.Code != http.StatusNotFound {
		t.Fatalf("deleted article status: %d", w.Code)
	}
	t.Run("durable automatic full content", func(t *testing.T) {
		manual, err := as.Search(ctx, articles.Query{Text: "手動登録記事"})
		if err != nil || manual.Total != 1 {
			t.Fatalf("manual article missing: %+v %v", manual, err)
		}
		manualItem := manual.Items[0]
		if status, err := as.FullContentStatus(ctx, manualItem.ID); err != nil || !status.Pending {
			t.Fatalf("manual registration was not queued: %+v %v", status, err)
		}
		if err := as.SaveFullContent(ctx, manualItem.ID, articles.FullContent{HTML: "<p>手動登録記事の本文</p>", Text: "手動登録記事の本文", URL: manualItem.URL}); err != nil {
			t.Fatal(err)
		}
		queued := articles.Article{SourceID: 999, ExternalID: "queue-one", URL: "https://example.com/queue-one", Title: "取得待ちのテスト", BodyText: "RSSの要約"}
		if err := as.Save(ctx, queued); err != nil {
			t.Fatal(err)
		}
		matching, err := as.Search(ctx, articles.Query{Text: queued.Title})
		if err != nil || matching.Total != 1 {
			t.Fatalf("queued article: %v %v", matching, err)
		}
		queuedID := matching.Items[0].ID
		freshStore := articles.New(connect(os.Getenv("ARTICLES_DATABASE_URL")))
		status, err := freshStore.FullContentStatus(ctx, queuedID)
		if err != nil || !status.Pending {
			t.Fatalf("pending state was not persisted: %+v %v", status, err)
		}
		if err := as.Save(ctx, queued); err != nil {
			t.Fatal(err)
		}
		if jobs, err := freshStore.PendingFullContent(ctx); err != nil || len(jobs) != 1 {
			t.Fatalf("duplicate or missing job: %v %v", jobs, err)
		}
		claimed, err := as.ClaimFullContent(ctx, queuedID)
		if err != nil || !claimed {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		if claimed, err := freshStore.ClaimFullContent(ctx, queuedID); err != nil || claimed {
			t.Fatalf("another worker overlapped automatic fetch: %v %v", claimed, err)
		}
		if err := as.FailFullContent(ctx, queuedID, errors.New("temporary failure")); err != nil {
			t.Fatal(err)
		}
		if jobs, err := as.PendingFullContent(ctx); err != nil || len(jobs) != 0 {
			t.Fatalf("failure did not back off: %v %v", jobs, err)
		}
		retained, err := as.Get(ctx, queuedID)
		if err != nil || retained.BodyText != queued.BodyText || !retained.FullContentPending || retained.FullFetchError == "" {
			t.Fatalf("failure lost article or retry state: %+v %v", retained, err)
		}
		if _, err := owner.ExecContext(ctx, `UPDATE articles.entries SET full_next_fetch_at=now()-interval '1 second',full_fetch_until=now()-interval '1 second' WHERE id=$1`, queuedID); err != nil {
			t.Fatal(err)
		}
		if claimed, err := freshStore.ClaimFullContent(ctx, queuedID); err != nil || !claimed {
			t.Fatalf("expired lease not reclaimed: %v %v", claimed, err)
		}
		if err := as.FailFullContent(ctx, queuedID, context.Canceled); err != nil {
			t.Fatal(err)
		}
		if jobs, err := freshStore.PendingFullContent(ctx); err != nil || len(jobs) != 1 {
			t.Fatalf("canceled work was not resumable: %v %v", jobs, err)
		}
		if err := as.SaveFullContent(ctx, queuedID, articles.FullContent{HTML: "<p>取得済み本文</p>", Text: "取得済み本文", URL: queued.URL}); err != nil {
			t.Fatal(err)
		}
		if err := as.Save(ctx, queued); err != nil {
			t.Fatal(err)
		}
		status, err = as.FullContentStatus(ctx, queuedID)
		if err != nil || status.Pending || status.Error != "" {
			t.Fatalf("completed article was requeued: %+v %v", status, err)
		}
		if w := request("GET", fmt.Sprintf("/articles/%d/content-status", queuedID), nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"pending":false`) {
			t.Fatalf("content status endpoint: %d %s", w.Code, w.Body.String())
		}
		for i := 0; i < 12; i++ {
			a := articles.Article{SourceID: 999, ExternalID: fmt.Sprintf("queue-batch-%d", i), URL: fmt.Sprintf("https://example.com/batch/%d", i), Title: "batch"}
			if err := as.Save(ctx, a); err != nil {
				t.Fatal(err)
			}
		}
		jobs, err := as.PendingFullContent(ctx)
		if err != nil || len(jobs) != 10 {
			t.Fatalf("unbounded or missing batch: %d %v", len(jobs), err)
		}
		for _, job := range jobs {
			if err := as.Delete(ctx, job.ID); err != nil {
				t.Fatal(err)
			}
		}
		jobs, err = as.PendingFullContent(ctx)
		if err != nil || len(jobs) != 2 {
			t.Fatalf("deleted jobs were processed or remaining jobs lost: %d %v", len(jobs), err)
		}
		// Existing articles are queued by the migration, not by every repeated RSS update.
		if _, err := owner.ExecContext(ctx, `UPDATE articles.entries SET full_next_fetch_at=NULL WHERE id=$1`, jobs[0].ID); err != nil {
			t.Fatal(err)
		}
		if err := as.Save(ctx, jobs[0]); err != nil {
			t.Fatal(err)
		}
		if status, err := as.FullContentStatus(ctx, jobs[0].ID); err != nil || status.Pending {
			t.Fatalf("old article was unexpectedly backfilled: %+v %v", status, err)
		}
		oldManual := articles.Article{URL: "https://example.com/old-manual", Title: "移行前の手動登録"}
		if err := as.Save(ctx, oldManual); err != nil {
			t.Fatal(err)
		}
		oldManualResult, err := as.Search(ctx, articles.Query{Text: oldManual.Title})
		if err != nil || oldManualResult.Total != 1 {
			t.Fatalf("legacy manual fixture: %v %v", oldManualResult, err)
		}
		oldManualID := oldManualResult.Items[0].ID
		if _, err := owner.ExecContext(ctx, `UPDATE articles.entries SET full_next_fetch_at=NULL WHERE id=$1`, oldManualID); err != nil {
			t.Fatal(err)
		}
		if claimed, err := as.ClaimFullContent(ctx, jobs[1].ID); err != nil || !claimed {
			t.Fatalf("retry fixture claim: %v %v", claimed, err)
		}
		if err := as.FailFullContent(ctx, jobs[1].ID, errors.New("retry later")); err != nil {
			t.Fatal(err)
		}
		var retryBefore, retryAfter time.Time
		if err := owner.QueryRowContext(ctx, `SELECT full_next_fetch_at FROM articles.entries WHERE id=$1`, jobs[1].ID).Scan(&retryBefore); err != nil {
			t.Fatal(err)
		}
		var deletedID int64
		if err := owner.QueryRowContext(ctx, `SELECT id FROM articles.entries WHERE deleted_at IS NOT NULL AND full_fetched_at IS NULL LIMIT 1`).Scan(&deletedID); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.ExecContext(ctx, `UPDATE articles.entries SET full_next_fetch_at=NULL WHERE id=$1`, deletedID); err != nil {
			t.Fatal(err)
		}
		// Replay only the backfill migration against representative pre-migration rows.
		if _, err := owner.ExecContext(ctx, `DELETE FROM public.schema_migrations WHERE name='006_backfill_article_content.sql'`); err != nil {
			t.Fatal(err)
		}
		if err := migrations.Run(ctx, owner); err != nil {
			t.Fatal(err)
		}
		for _, id := range []int64{jobs[0].ID, oldManualID} {
			if status, err := as.FullContentStatus(ctx, id); err != nil || !status.Pending {
				t.Fatalf("legacy article %d not queued: %+v %v", id, status, err)
			}
		}
		if status, err := as.FullContentStatus(ctx, queuedID); err != nil || status.Pending {
			t.Fatalf("completed full content was requeued: %+v %v", status, err)
		}
		if err := owner.QueryRowContext(ctx, `SELECT full_next_fetch_at FROM articles.entries WHERE id=$1`, jobs[1].ID).Scan(&retryAfter); err != nil {
			t.Fatal(err)
		}
		if !retryBefore.Equal(retryAfter) {
			t.Fatal("backfill changed an existing retry schedule")
		}
		var deletedStillExcluded bool
		if err := owner.QueryRowContext(ctx, `SELECT full_next_fetch_at IS NULL FROM articles.entries WHERE id=$1`, deletedID).Scan(&deletedStillExcluded); err != nil {
			t.Fatal(err)
		}
		if !deletedStillExcluded {
			t.Fatal("deleted article was queued by backfill")
		}
	})
	t.Run("OPML import export and atomic merge", func(t *testing.T) {
		existingURL := "https://opml.example.org/existing?a=1&b=2"
		archivedURL := "https://opml.example.org/archived"
		newURL := "https://opml.example.org/new"
		for _, u := range []string{existingURL, archivedURL} {
			if err := fs.Add(ctx, u); err != nil {
				t.Fatal(err)
			}
		}
		find := func(list []feeds.Feed, url string) feeds.Feed {
			t.Helper()
			for _, f := range list {
				if f.URL == url {
					return f
				}
			}
			t.Fatalf("feed not found: %s", url)
			return feeds.Feed{}
		}
		initial, err := fs.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		existing, archived := find(initial, existingURL), find(initial, archivedURL)
		if err := fs.Record(ctx, existing.ID, feeds.FetchResult{Title: "既存の名前", TTLMinutes: 60, ETag: `"stable"`}, 30*time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := fs.Remove(ctx, archived.ID); err != nil {
			t.Fatal(err)
		}
		before, err := fs.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		saved := find(before, existingURL)
		input := `<opml version="2.0"><body><outline text="フォルダー"><outline title="新しい名前には変更しない" xmlUrl="https://opml.example.org/existing?a=1&amp;b=2"/><outline title="日本語 &amp; 新着" xmlUrl="https://opml.example.org/new"/><outline xmlUrl="https://opml.example.org/archived"/><outline xmlUrl="https://opml.example.org/existing?a=1&amp;b=2"/><outline xmlUrl="javascript:alert(1)"/></outline></body></opml>`
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, opmlUpload(t, []byte(input), "/?q=Go&view=unread"))
		location, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		q := location.Query()
		if w.Code != http.StatusSeeOther || q.Get("added") != "2" || q.Get("existing") != "2" || q.Get("skipped") != "1" || q.Get("q") != "Go" || q.Get("view") != "unread" {
			t.Fatalf("import response: %d %s %s", w.Code, location, w.Body.String())
		}
		after, err := fs.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before)+2 {
			t.Fatalf("subscription count: %d -> %d", len(before), len(after))
		}
		unchanged := find(after, existingURL)
		if unchanged.Title != saved.Title || unchanged.ETag != saved.ETag || unchanged.NextFetchAt == nil || !unchanged.NextFetchAt.Equal(*saved.NextFetchAt) {
			t.Fatalf("existing subscription changed: %+v", unchanged)
		}
		restored := find(after, archivedURL)
		if restored.ID != archived.ID || restored.NextFetchAt != nil {
			t.Fatalf("reactivation lost identity or fetch schedule: %+v", restored)
		}
		if find(after, newURL).Title != "日本語 & 新着" {
			t.Fatal("OPML title was lost")
		}
		// A syntactically broken file must not import an otherwise valid first entry.
		if _, err := fs.ImportOPML(ctx, strings.NewReader(`<opml><body><outline xmlUrl="https://opml.example.org/partial"/></body>`)); !errors.Is(err, feeds.ErrInvalidOPML) {
			t.Fatalf("malformed XML accepted: %v", err)
		}
		current, err := fs.List(ctx)
		if err != nil || len(current) != len(after) {
			t.Fatalf("malformed import had partial effects: %v %v", current, err)
		}
		var output, again bytes.Buffer
		if err := fs.ExportOPML(ctx, &output); err != nil {
			t.Fatal(err)
		}
		var document struct {
			Version string `xml:"version,attr"`
			Body    struct {
				Outlines []struct {
					URL string `xml:"xmlUrl,attr"`
				} `xml:"outline"`
			} `xml:"body"`
		}
		if err := xml.Unmarshal(output.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
		if document.Version != "2.0" || len(document.Body.Outlines) != len(after) || !strings.Contains(output.String(), "日本語 &amp; 新着") {
			t.Fatalf("invalid export: %s", output.String())
		}
		result, err := fs.ImportOPML(ctx, bytes.NewReader(output.Bytes()))
		if err != nil || result.Added != 0 || result.Existing != len(after) || result.Skipped != 0 {
			t.Fatalf("round trip: %+v %v", result, err)
		}
		if err := fs.ExportOPML(ctx, &again); err != nil {
			t.Fatal(err)
		}
		if output.String() != again.String() {
			t.Fatal("export is not deterministic")
		}
		exported := request("GET", "/feeds/export.opml", nil)
		if exported.Code != http.StatusOK || !strings.Contains(exported.Header().Get("Content-Disposition"), "subscriptions.opml") || exported.Body.String() != output.String() {
			t.Fatalf("download response: %d %v", exported.Code, exported.Header())
		}
		// Simulate a DB failure after the first insert to verify transaction rollback.
		if _, err := owner.ExecContext(ctx, `ALTER TABLE feeds.subscriptions ADD CONSTRAINT opml_test_reject CHECK (url <> 'https://opml.example.org/reject')`); err != nil {
			t.Fatal(err)
		}
		failed := `<opml><body><outline xmlUrl="https://opml.example.org/rollback"/><outline xmlUrl="https://opml.example.org/reject"/></body></opml>`
		if _, err := fs.ImportOPML(ctx, strings.NewReader(failed)); err == nil {
			t.Fatal("expected database import failure")
		}
		current, err = fs.List(ctx)
		if err != nil || len(current) != len(after) {
			t.Fatalf("failed transaction had partial effects: %v %v", current, err)
		}
	})
}
