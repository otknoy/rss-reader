package main

import (
	"errors"
	"net/http"
	"rss-reader/internal/feeds"
	"strconv"
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
	switch {
	case errors.Is(err, feeds.ErrFolderName):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, feeds.ErrFolderNameConflict):
		http.Error(w, "同名のフォルダーがあります", http.StatusConflict)
	case errors.Is(err, feeds.ErrFolderNotFound):
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
		if errors.Is(err, feeds.ErrFolderNotFound) {
			http.NotFound(w, r)
			return
		}
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseFolderOrder(values []string) ([]int64, bool) {
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, true
}

func (a *application) setFolderOrder(w http.ResponseWriter, r *http.Request) {
	previous, previousOK := parseFolderOrder(r.PostForm["previous_folder"])
	ids, idsOK := parseFolderOrder(r.PostForm["folder"])
	if !previousOK || !idsOK {
		http.Error(w, "並び順の指定が不正です", http.StatusBadRequest)
		return
	}
	err := a.feeds.SetFolderOrder(r.Context(), previous, ids)
	if errors.Is(err, feeds.ErrInvalidFolderOrder) {
		http.Error(w, "並び順の指定が不正です", http.StatusBadRequest)
		return
	}
	if errors.Is(err, feeds.ErrFolderOrderChanged) {
		http.Error(w, "フォルダーが別の操作で変更されたため、並び順を保存できませんでした。", http.StatusConflict)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	redirect(w, r, "")
}
