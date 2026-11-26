package main

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/url"
	"rss-reader/internal/feeds"
	"strconv"
)

func (a *application) importOPML(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(feeds.MaxOPMLBytes + (64 << 10)); err != nil {
		var limitErr *http.MaxBytesError
		if errors.As(err, &limitErr) {
			http.Error(w, feeds.ErrOPMLTooLarge.Error(), http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "OPMLファイルを選択してください", http.StatusBadRequest)
		}
		return
	}
	defer func() {
		if err := r.MultipartForm.RemoveAll(); err != nil {
			log.Printf("cleanup OPML upload: %v", err)
		}
	}()
	file, header, err := r.FormFile("opml")
	if err != nil {
		http.Error(w, "OPMLファイルを選択してください", http.StatusBadRequest)
		return
	}
	defer closeResource(file)
	if header.Size > feeds.MaxOPMLBytes {
		http.Error(w, feeds.ErrOPMLTooLarge.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	result, err := feeds.ImportOPML(r.Context(), a.feeds, file)
	if errors.Is(err, feeds.ErrInvalidOPML) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if errors.Is(err, feeds.ErrOPMLTooLarge) {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	target, _ := url.Parse(returnTo(r))
	q := target.Query()
	q.Set("message", "opml-imported")
	q.Set("added", strconv.Itoa(result.Added))
	q.Set("existing", strconv.Itoa(result.Existing))
	q.Set("skipped", strconv.Itoa(result.Skipped))
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

func (a *application) exportOPML(w http.ResponseWriter, r *http.Request) {
	// Build before setting download headers, so DB/XML errors remain ordinary error responses.
	var output bytes.Buffer
	if err := feeds.ExportOPML(r.Context(), a.feeds, &output); err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/x-opml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="subscriptions.opml"`)
	if _, err := w.Write(output.Bytes()); err != nil {
		log.Printf("write OPML export: %v", err)
	}
}
