// Package notiontest is a fake Notion API for tests: one database of tasks
// from testdata/, served over httptest, evaluating the filters the tracker
// sends rather than replaying canned query answers.
package notiontest

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Token is the bearer token the fake accepts.
const Token = "secret_test"

// IDs of the fixture database and data source.
const (
	DatabaseID   = "1a2b3c4d00004000800000000000db01"
	DataSourceID = "1a2b3c4d00004000800000000000d501"
)

//go:embed testdata
var fixtures embed.FS

type page = map[string]any

// Server is a running fake.
type Server struct {
	*httptest.Server

	// PageLimit caps every list answer, as Notion caps them at 100, so a
	// test can make the client follow next_cursor with a few rows.
	PageLimit int

	// CreateStatus, when set, is the status POST /pages answers, creating
	// nothing.
	CreateStatus int

	// UpdateStatus and CommentStatus, when set, are the status the first
	// update or comment request answers, changing nothing; later ones are
	// served.
	UpdateStatus  int
	CommentStatus int

	// RowLimit caps every rows-mode answer of the MCP face, which has no
	// cursor, so a test can make it answer has_more.
	RowLimit int

	// Protocol, when set, is the MCP version initialize answers.
	Protocol string

	mu       sync.Mutex // guards pages, markdown and the counters, which writes change, and the MCP face's state
	creates  int
	updates  int
	posts    int
	oauth    oauthState
	calls    map[string]int
	session  string
	sessions int
	pages    []page
	comments map[string][]any
	people   []any
	markdown map[string]any
	database any
	source   any
}

// New starts a fake on the fixture database; it closes with the test.
func New(t *testing.T) *Server {
	t.Helper()

	s := &Server{
		oauth: oauthState{
			clients: map[string][]string{}, codes: map[string]grant{},
			access: map[string]bool{}, refresh: map[string]string{},
		},
		calls: map[string]int{},
	}
	load(t, "pages.json", &s.pages)
	load(t, "comments.json", &s.comments)
	load(t, "users.json", &s.people)
	load(t, "markdown.json", &s.markdown)
	load(t, "database.json", &s.database)
	load(t, "data_source.json", &s.source)

	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)

	return s
}

func load(t *testing.T, name string, into any) {
	t.Helper()

	data, err := fixtures.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// Creates counts the POST /pages requests received, answered or not.
func (s *Server) Creates() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.creates
}

// Updates counts the property updates received over either face, answered
// or not.
func (s *Server) Updates() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updates
}

// Comments counts the comments received over either face, answered or not.
func (s *Server) Comments() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.posts
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.serveMCP(w, r) {
		return
	}

	if r.Header.Get("Authorization") != "Bearer "+Token {
		apiError(w, http.StatusUnauthorized, "unauthorized", "API token is invalid.")

		return
	}

	if r.Header.Get("Notion-Version") == "" {
		apiError(w, http.StatusBadRequest, "missing_version", "Notion-Version header failed validation.")

		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	id := ""

	if len(parts) > 1 {
		id = norm(parts[1])
	}

	switch {
	case r.Method == http.MethodGet && parts[0] == "databases" && id == DatabaseID:
		reply(w, s.database)
	case r.Method == http.MethodGet && parts[0] == "data_sources" && len(parts) == 2 && id == DataSourceID:
		reply(w, s.source)
	case r.Method == http.MethodPost && parts[0] == "data_sources" && len(parts) == 3 && parts[2] == "query" && id == DataSourceID:
		s.query(w, r)
	case r.Method == http.MethodPost && parts[0] == "pages" && len(parts) == 1:
		s.create(w, r)
	case r.Method == http.MethodPatch && parts[0] == "pages" && len(parts) == 2:
		s.update(w, r, id)
	case r.Method == http.MethodPost && parts[0] == "comments" && len(parts) == 1:
		s.comment(w, r)
	case r.Method == http.MethodGet && parts[0] == "users" && len(parts) == 1:
		q := r.URL.Query()
		s.page(w, s.people, q.Get("start_cursor"), atoi(q.Get("page_size")))
	case r.Method == http.MethodGet && parts[0] == "pages" && len(parts) == 2:
		if p := s.pageByID(id); p != nil {
			reply(w, p)

			return
		}

		notFound(w, id)
	case r.Method == http.MethodGet && parts[0] == "pages" && len(parts) == 3 && parts[2] == "markdown":
		if md, ok := s.markdown[id]; ok {
			reply(w, md)

			return
		}

		notFound(w, id)
	case r.Method == http.MethodGet && parts[0] == "comments":
		q := r.URL.Query()
		s.page(w, orEmpty(s.comments[norm(q.Get("block_id"))]), q.Get("start_cursor"), atoi(q.Get("page_size")))
	default:
		notFound(w, id)
	}
}

// create stores a page the way Notion answers it: response-shaped
// properties, the next Unique ID, and its markdown, so read finds it after.
// Option values are checked against the schema, as Notion rejects them.
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	s.creates++

	if s.CreateStatus != 0 {
		apiError(w, s.CreateStatus, "service_unavailable", "Notion is unavailable.")

		return
	}

	var body struct {
		Parent struct {
			DataSourceID string `json:"data_source_id"`
		} `json:"parent"`
		Properties map[string]map[string]any `json:"properties"`
		Markdown   string                    `json:"markdown"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_json", err.Error())

		return
	}

	if norm(body.Parent.DataSourceID) != DataSourceID {
		notFound(w, norm(body.Parent.DataSourceID))

		return
	}

	schema := s.schema()
	types := schemaTypes(schema)
	set := map[string]any{}

	for name, value := range body.Properties {
		typ, ok := types[name]
		if !ok {
			apiError(w, http.StatusBadRequest, "validation_error", name+" is not a property that exists.")

			return
		}

		v, err := s.respond(typ, value[typ], options(schema, name, typ))
		if err != nil {
			apiError(w, http.StatusBadRequest, "validation_error", name+": "+err.Error())

			return
		}

		set[name] = v
	}

	reply(w, s.insert(set, body.Markdown))
}

// update sets a page's properties from request-shaped values, checked as
// create checks them.
func (s *Server) update(w http.ResponseWriter, r *http.Request, id string) {
	s.updates++

	if s.UpdateStatus != 0 && s.updates == 1 {
		apiError(w, s.UpdateStatus, "service_unavailable", "Notion is unavailable.")

		return
	}

	var body struct {
		Properties map[string]map[string]any `json:"properties"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_json", err.Error())

		return
	}

	p := s.pageByID(id)
	if p == nil {
		notFound(w, id)

		return
	}

	set := map[string]any{}

	for name, value := range body.Properties {
		typ, ok := schemaTypes(s.schema())[name]
		if !ok {
			apiError(w, http.StatusBadRequest, "validation_error", name+" is not a property that exists.")

			return
		}

		v, err := s.respond(typ, value[typ], options(s.schema(), name, typ))
		if err != nil {
			apiError(w, http.StatusBadRequest, "validation_error", name+": "+err.Error())

			return
		}

		set[name] = v
	}

	s.apply(p, set)
	reply(w, p)
}

// apply stores response-shaped values on a page.
func (s *Server) apply(p page, set map[string]any) {
	types := schemaTypes(s.schema())
	props, _ := p["properties"].(map[string]any)

	for name, v := range set {
		props[name] = map[string]any{"type": types[name], types[name]: v}
	}
}

// comment stores a page-level comment from its rich text, authored by the
// integration.
func (s *Server) comment(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Parent struct {
			PageID string `json:"page_id"`
		} `json:"parent"`
		RichText []struct {
			Text struct {
				Content string `json:"content"`
			} `json:"text"`
		} `json:"rich_text"`
	}

	s.posts++

	if s.CommentStatus != 0 && s.posts == 1 {
		apiError(w, s.CommentStatus, "service_unavailable", "Notion is unavailable.")

		return
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_json", err.Error())

		return
	}

	var runs []any

	for _, rt := range body.RichText {
		if len(rt.Text.Content) > 2000 {
			apiError(w, http.StatusBadRequest, "validation_error", "rich_text content is longer than 2000.")

			return
		}

		runs = append(runs, map[string]any{"type": "text", "plain_text": rt.Text.Content})
	}

	if len(runs) == 0 || len(runs) > 100 {
		apiError(w, http.StatusBadRequest, "validation_error", "rich_text should have 1 to 100 items.")

		return
	}

	reply(w, s.addComment(norm(body.Parent.PageID), runs))
}

// addComment stores a comment by the integration and returns it.
func (s *Server) addComment(pageID string, runs []any) map[string]any {
	c := map[string]any{
		"object":        "comment",
		"id":            fmt.Sprintf("c0000000-0000-4000-8000-%012d", s.posts),
		"discussion_id": "d1",
		"created_time":  time.Now().UTC().Format(time.RFC3339),
		"created_by":    map[string]any{"object": "user", "id": "b1"},
		"rich_text":     runs,
		"display_name":  map[string]any{"type": "integration", "resolved_name": "f10"},
	}

	s.comments[pageID] = append(s.comments[pageID], c)

	return c
}

// insert stores a new page from response-shaped values by property name:
// the status Notion defaults to, the next Unique ID, and its markdown.
func (s *Server) insert(set map[string]any, markdown string) page {
	schema := s.schema()
	types := schemaTypes(schema)
	props := map[string]any{}

	for name, typ := range types {
		props[name] = map[string]any{"type": typ}

		if opts := options(schema, name, typ); typ == "status" && len(opts) > 0 {
			props[name] = map[string]any{"type": typ, typ: map[string]any{"name": opts[0]}} // Notion's default
		}
	}

	for name, v := range set {
		props[name] = map[string]any{"type": types[name], types[name]: v}
	}

	next := 0

	for _, p := range s.pages {
		props, _ := p["properties"].(map[string]any)

		for _, prop := range props {
			pm, _ := prop.(map[string]any)
			uid, _ := pm["unique_id"].(map[string]any)
			n, _ := uid["number"].(float64)
			next = max(next, int(n))
		}
	}

	next++

	for name, typ := range types {
		if typ == "unique_id" {
			d, _ := schema[name].(map[string]any)
			cfg, _ := d[typ].(map[string]any)
			uid := map[string]any{"prefix": cfg["prefix"], "number": float64(next)}
			props[name] = map[string]any{"type": typ, typ: uid}
		}
	}

	id := fmt.Sprintf("2b000000-0000-4000-8000-%012d", next)
	p := page{
		"object":       "page",
		"id":           id,
		"created_time": time.Now().UTC().Format(time.RFC3339),
		"url":          "https://www.notion.so/" + norm(id),
		"properties":   props,
	}

	s.pages = append(s.pages, p)
	s.markdown[norm(id)] = map[string]any{
		"object":            "page_markdown",
		"id":                id,
		"markdown":          markdown,
		"truncated":         false,
		"unknown_block_ids": []any{},
	}

	return p
}

func (s *Server) schema() map[string]any {
	src, _ := s.source.(map[string]any)
	props, _ := src["properties"].(map[string]any)

	return props
}

func schemaTypes(schema map[string]any) map[string]string {
	types := map[string]string{}

	for name, def := range schema {
		d, _ := def.(map[string]any)
		types[name], _ = d["type"].(string)
	}

	return types
}

func options(schema map[string]any, name, typ string) []string {
	d, _ := schema[name].(map[string]any)
	cfg, _ := d[typ].(map[string]any)
	opts, _ := cfg["options"].([]any)

	names := make([]string, 0, len(opts))

	for _, o := range opts {
		om, _ := o.(map[string]any)
		n, _ := om["name"].(string)
		names = append(names, n)
	}

	return names
}

// respond turns a request value into its response shape; null clears.
func (s *Server) respond(typ string, v any, opts []string) (any, error) {
	if v == nil && typ != "title" {
		return nil, nil
	}

	known := func(o any) (any, error) {
		om, _ := o.(map[string]any)
		name, _ := om["name"].(string)

		if !slices.Contains(opts, name) {
			return nil, fmt.Errorf("%q is not an option", name)
		}

		return map[string]any{"name": name}, nil
	}

	switch typ {
	case "title":
		runs, _ := v.([]any)
		out := make([]any, 0, len(runs))

		for _, run := range runs {
			rm, _ := run.(map[string]any)
			txt, _ := rm["text"].(map[string]any)
			out = append(out, map[string]any{"type": "text", "plain_text": txt["content"]})
		}

		return out, nil
	case "status", "select":
		return known(v)
	case "url", "email", "phone_number", "number", "checkbox":
		return v, nil
	case "rich_text":
		runs, _ := v.([]any)
		out := make([]any, 0, len(runs))

		for _, run := range runs {
			rm, _ := run.(map[string]any)
			txt, _ := rm["text"].(map[string]any)
			out = append(out, map[string]any{"type": "text", "plain_text": txt["content"]})
		}

		return out, nil
	case "date":
		d, _ := v.(map[string]any)
		if _, ok := d["start"].(string); !ok {
			return nil, errors.New("a date needs a start")
		}

		return map[string]any{"start": d["start"], "end": d["end"], "time_zone": nil}, nil
	case "people":
		items, _ := v.([]any)
		out := make([]any, 0, len(items))

		for _, item := range items {
			im, _ := item.(map[string]any)
			id, _ := im["id"].(string)

			name, ok := s.users()[id]
			if !ok {
				return nil, fmt.Errorf("%s is not a user", id)
			}

			out = append(out, map[string]any{"object": "user", "id": id, "name": name})
		}

		return out, nil
	case "multi_select", "relation":
		items, _ := v.([]any)
		out := make([]any, 0, len(items))

		for _, item := range items {
			if typ == "relation" {
				out = append(out, item)

				continue
			}

			o, err := known(item)
			if err != nil {
				return nil, err
			}

			out = append(out, o)
		}

		return out, nil
	}

	return nil, fmt.Errorf("the fake cannot set a %s property", typ)
}

func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Filter      map[string]any `json:"filter"`
		PageSize    int            `json:"page_size"`
		StartCursor string         `json:"start_cursor"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_json", err.Error())

		return
	}

	results := []any{}

	for _, p := range s.pages {
		ok, err := match(p, body.Filter)
		if err != nil {
			apiError(w, http.StatusBadRequest, "validation_error", err.Error())

			return
		}

		if ok {
			results = append(results, p)
		}
	}

	s.page(w, results, body.StartCursor, body.PageSize)
}

// page answers one page of results; the cursor is the next row's index.
func (s *Server) page(w http.ResponseWriter, results []any, cursor string, size int) {
	if s.PageLimit > 0 && (size == 0 || size > s.PageLimit) {
		size = s.PageLimit
	}

	start := min(atoi(cursor), len(results))

	end := len(results)
	if size > 0 {
		end = min(start+size, end)
	}

	var next any
	if end < len(results) {
		next = strconv.Itoa(end)
	}

	reply(
		w,
		map[string]any{"object": "list", "results": results[start:end], "has_more": next != nil, "next_cursor": next},
	)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)

	return n
}

// match evaluates the filter shapes the tracker sends.
func match(p page, f map[string]any) (bool, error) {
	if f == nil {
		return true, nil
	}

	if all, ok := f["and"].([]any); ok {
		for _, sub := range all {
			subMap, _ := sub.(map[string]any)

			ok, err := match(p, subMap)
			if err != nil || !ok {
				return false, err
			}
		}

		return true, nil
	}

	name, _ := f["property"].(string)
	props, _ := p["properties"].(map[string]any)
	prop, _ := props[name].(map[string]any)

	if prop == nil {
		return false, fmt.Errorf("could not find property with name or id: %s", name)
	}

	typ, _ := prop["type"].(string)

	cond, ok := f[typ].(map[string]any)
	if !ok {
		return false, fmt.Errorf("filter on %s must use its type %s", name, typ)
	}

	switch typ {
	case "unique_id":
		uid, _ := prop["unique_id"].(map[string]any)

		return uid["number"] == cond["equals"], nil
	case "status", "select":
		opt, _ := prop[typ].(map[string]any)
		val, _ := opt["name"].(string)

		if want, ok := cond["equals"].(string); ok {
			return val == want, nil
		}

		if not, ok := cond["does_not_equal"].(string); ok {
			return val != not, nil
		}
	case "title":
		want, _ := cond["contains"].(string)

		return strings.Contains(strings.ToLower(text(prop["title"])), strings.ToLower(want)), nil
	case "relation":
		want, _ := cond["contains"].(string)
		rel, _ := prop["relation"].([]any)

		for _, r := range rel {
			rm, _ := r.(map[string]any)
			if id, _ := rm["id"].(string); norm(id) == norm(want) {
				return true, nil
			}
		}

		return false, nil
	}

	return false, fmt.Errorf("unsupported filter %v", f)
}

func (s *Server) pageByID(id string) page {
	for _, p := range s.pages {
		if pid, _ := p["id"].(string); norm(pid) == id {
			return p
		}
	}

	return nil
}

func text(v any) string {
	runs, _ := v.([]any)

	var b strings.Builder

	for _, r := range runs {
		rm, _ := r.(map[string]any)
		s, _ := rm["plain_text"].(string)
		b.WriteString(s)
	}

	return b.String()
}

func norm(id string) string { return strings.ToLower(strings.ReplaceAll(id, "-", "")) }

func orEmpty(v []any) []any {
	if v == nil {
		return []any{}
	}

	return v
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter, id string) {
	apiError(w, http.StatusNotFound, "object_not_found", "Could not find object with ID: "+id+".")
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "error", "status": status, "code": code, "message": msg})
}
