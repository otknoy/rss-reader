package main

import (
	"errors"
	"net/http"
	"rss-reader/internal/feeds"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
)

func (a *application) changeFolder(w http.ResponseWriter, r *http.Request) {
	var err error
	switch r.PathValue("operation") {
	case "create":
		err = a.feeds.SaveFolder(r.Context(), 0, r.FormValue("name"))
	case "rename", "delete", "move":
		n, parseErr := id(r)
		if parseErr != nil {
			fail(w, parseErr)
			return
		}
		switch r.PathValue("operation") {
		case "rename":
			err = a.feeds.SaveFolder(r.Context(), n, r.FormValue("name"))
		case "delete":
			err = a.feeds.DeleteFolder(r.Context(), n)
			if err == nil {
				http.Redirect(w, r, "/?message=saved", http.StatusSeeOther)
				return
			}
		case "move":
			folder, parseErr := strconv.ParseInt(r.FormValue("folder"), 10, 64)
			if parseErr != nil || folder < 0 {
				http.Error(w, "フォルダーの指定が不正です", http.StatusBadRequest)
				return
			}
			err = a.feeds.Move(r.Context(), n, folder)
		}
	default:
		http.NotFound(w, r)
		return
	}
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, feeds.ErrFolderName):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		http.Error(w, "同名のフォルダーがあります", http.StatusConflict)
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		http.Error(w, "移動先のフォルダーがありません", http.StatusBadRequest)
	case err != nil:
		fail(w, err)
	default:
		redirect(w, r, "saved")
	}
}

func (a *application) setFolderOpen(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	state := r.FormValue("open")
	if err != nil || id < 0 || (state != "true" && state != "false") {
		http.Error(w, "開閉状態の指定が不正です", http.StatusBadRequest)
		return
	}
	if err := a.feeds.SetFolderOpen(r.Context(), id, state == "true"); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			http.NotFound(w, r)
			return
		}
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *application) setFolderOrder(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "並び順の指定が不正です", http.StatusBadRequest)
		return
	}
	values := r.PostForm["folder"]
	ids := make([]int64, 0, len(values))
	seen := make(map[int64]bool)
	for _, value := range values {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 || seen[id] {
			http.Error(w, "並び順の指定が不正です", http.StatusBadRequest)
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		http.Error(w, "並び順の指定が不正です", http.StatusBadRequest)
		return
	}
	if err := a.feeds.SetFolderOrder(r.Context(), ids); err != nil {
		fail(w, err)
		return
	}
	redirect(w, r, "")
}
