package main

import (
	_ "embed"
	"encoding/json"
	"html/template"
	"log"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
)

//go:embed database.html
var databaseHTML string

var databasePage = template.Must(template.New("database").Parse(databaseHTML))

func (a *application) database(w http.ResponseWriter, r *http.Request) {
	const pageSize = 20
	q := r.URL.Query()
	table := q.Get("table")
	if table == "" {
		table = "subscriptions"
	}
	number := int64(1)
	if raw := q.Get("page"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n < 1 {
			http.Error(w, "ページの指定が不正です", http.StatusBadRequest)
			return
		}
		number = n
	}
	var id int64
	if raw := q.Get("id"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 || table != "entries" {
			http.Error(w, "記事IDの指定が不正です", http.StatusBadRequest)
			return
		}
		id = n
	}
	var data []byte
	var err error
	switch table {
	case "entries":
		data, err = a.articles.Inspect(r.Context(), id, pageSize+1, (number-1)*pageSize)
	case "article_views":
		data, err = a.articles.InspectViews(r.Context(), pageSize+1, (number-1)*pageSize)
	case "subscriptions", "folders", "closed_folders":
		data, err = a.feeds.Inspect(r.Context(), table, pageSize+1, (number-1)*pageSize)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(data, &records); err != nil {
		fail(w, err)
		return
	}
	model := struct {
		Table, QualifiedTable, Previous, Next, Back string
		Page                                        int64
		Columns                                     []string
		Rows                                        []struct {
			Values []string
			Detail string
		}
	}{Table: table, QualifiedTable: "feeds." + table, Page: number}
	if table == "entries" || table == "article_views" {
		model.QualifiedTable = "articles." + table
	}
	pageURL := func(n int64) string {
		return "/admin/database?" + (url.Values{"table": {table}, "page": {strconv.FormatInt(n, 10)}}).Encode()
	}
	if id > 0 {
		model.Back = pageURL(number)
	} else if number > 1 {
		model.Previous = pageURL(number - 1)
	}
	if len(records) > pageSize {
		records = records[:pageSize]
		model.Next = pageURL(number + 1)
	}
	if len(records) > 0 {
		model.Columns = slices.Sorted(maps.Keys(records[0]))
		if index := slices.Index(model.Columns, "id"); index > 0 {
			copy(model.Columns[1:index+1], model.Columns[:index])
			model.Columns[0] = "id"
		}
	}
	for _, record := range records {
		row := struct {
			Values []string
			Detail string
		}{}
		for _, column := range model.Columns {
			raw := record[column]
			value := string(raw)
			if len(raw) > 0 && raw[0] == '"' {
				if err := json.Unmarshal(raw, &value); err != nil {
					fail(w, err)
					return
				}
			}
			row.Values = append(row.Values, value)
		}
		if table == "entries" && id == 0 {
			row.Detail = pageURL(number) + "&id=" + url.QueryEscape(string(record["id"]))
		}
		model.Rows = append(model.Rows, row)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := databasePage.Execute(w, model); err != nil {
		log.Printf("render database administration: %v", err)
	}
}
