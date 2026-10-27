// Package notion is the Notion tracker: a small client over the REST API and
// a contract.Tracker on top of it, configured by driver.toml's [notion] table.
package notion

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIVersion is the Notion-Version this client speaks: databases hold data
// sources (since 2025-09-03), queries go to the data source, and a trashed
// object reads in_trash, not archived.
const APIVersion = "2026-03-11"

// DefaultBaseURL is Notion's API root.
const DefaultBaseURL = "https://api.notion.com/v1"

const (
	maxRetryWait = 10 * time.Second
	maxRetries   = 3
	pageSize     = 100

	readTimeout = 30 * time.Second
	// writeTimeout is longer than a read's: a write that times out leaves its
	// outcome unknown, and a re-run can make the task twice.
	writeTimeout = 60 * time.Second
)

// Client calls the endpoints the tracker needs and no others.
type Client struct {
	base  string
	token string
	read  *http.Client
	write *http.Client
}

// NewClient returns a client for base (DefaultBaseURL when empty).
func NewClient(base, token string) *Client {
	if base == "" {
		base = DefaultBaseURL
	}

	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		read:  &http.Client{Timeout: readTimeout},
		write: &http.Client{Timeout: writeTimeout},
	}
}

// APIError is an error response, printed as Notion words it.
type APIError struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("notion: HTTP %d", e.Status)
	}

	return fmt.Sprintf("notion: %s: %s", e.Code, e.Message)
}

// UnknownOutcomeError is a write that may have reached Notion with no answer
// saying whether it acted: the connection failed, or Notion answered 5xx.
type UnknownOutcomeError struct {
	Err error
}

func (e *UnknownOutcomeError) Error() string { return e.Err.Error() }

func (e *UnknownOutcomeError) Unwrap() error { return e.Err }

// Database is a database object, trimmed to what the tracker reads.
type Database struct {
	URL         string          `json:"url"`
	DataSources []DataSourceRef `json:"data_sources"`
}

// DataSourceRef names one of a database's data sources.
type DataSourceRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DataSource is a data source object: its parent database and its schema.
type DataSource struct {
	ID     string `json:"id"`
	Parent struct {
		DatabaseID string `json:"database_id"`
	} `json:"parent"`
	Properties map[string]PropertySchema `json:"properties"`
}

// PropertySchema is one property's definition in a data source.
type PropertySchema struct {
	Type     string `json:"type"`
	UniqueID struct {
		Prefix *string `json:"prefix"`
	} `json:"unique_id"`
	Status struct {
		Options []Option `json:"options"`
	} `json:"status"`
	Select struct {
		Options []Option `json:"options"`
	} `json:"select"`
	MultiSelect struct {
		Options []Option `json:"options"`
	} `json:"multi_select"`
}

// Page is a page object with its property values.
type Page struct {
	ID          string              `json:"id"`
	URL         string              `json:"url"`
	CreatedTime string              `json:"created_time"`
	Properties  map[string]Property `json:"properties"`
}

// Property is one property value; only the field named by Type is set.
type Property struct {
	Type        string     `json:"type"`
	Title       []RichText `json:"title"`
	UniqueID    *UniqueID  `json:"unique_id"`
	Status      *Option    `json:"status"`
	Select      *Option    `json:"select"`
	MultiSelect []Option   `json:"multi_select"`
	People      []Person   `json:"people"`
	Relation    []Ref      `json:"relation"`
	URL         *string    `json:"url"`
	RichText    []RichText `json:"rich_text"`
	Number      *float64   `json:"number"`
	Checkbox    *bool      `json:"checkbox"`
	Date        *Date      `json:"date"`
	Email       *string    `json:"email"`
	PhoneNumber *string    `json:"phone_number"`
}

// Date is a date property's value.
type Date struct {
	Start string  `json:"start"`
	End   *string `json:"end"`
}

// Person is a user in a people property.
type Person struct {
	Name string `json:"name"`
}

// User is a workspace member, as --assignee resolves one.
type User struct {
	ID    string
	Name  string
	Email string
}

// Ref is a related page in a relation property.
type Ref struct {
	ID string `json:"id"`
}

// UniqueID is a Unique ID property value.
type UniqueID struct {
	Prefix *string `json:"prefix"`
	Number *int    `json:"number"`
}

// Option is a status, select or multi-select option.
type Option struct {
	Name string `json:"name"`
}

// RichText is one rich text run.
type RichText struct {
	PlainText string `json:"plain_text"`
}

// Markdown is a page body as Notion renders it.
type Markdown struct {
	Markdown        string   `json:"markdown"`
	Truncated       bool     `json:"truncated"`
	UnknownBlockIDs []string `json:"unknown_block_ids"`
}

// Comment is one comment on a page.
type Comment struct {
	CreatedTime string     `json:"created_time"`
	RichText    []RichText `json:"rich_text"`
	DisplayName struct {
		ResolvedName *string `json:"resolved_name"`
	} `json:"display_name"`
}

// Query asks a data source for the pages matching every filter.
type Query struct {
	Filters  []Filter
	PageSize int
}

// Filter matches one property. Type is the property's schema type, which
// decides the operators a backend may send.
type Filter struct {
	Property string
	Type     string
	Op       Op
	Value    any
}

// Op is a filter operator, named as the REST API names it.
type Op string

// The operators the tracker filters with.
const (
	OpEquals    Op = "equals"
	OpNotEquals Op = "does_not_equal"
	OpContains  Op = "contains" // a title's text, or a relation's page id
)

// Draft is a page to create in a data source: its property values by
// name, and its body as markdown.
type Draft struct {
	DataSource string
	Properties map[string]Value
	Markdown   string
}

// Value is a property value to set: Text for a title, a url, text, an
// email, a phone number or a date's start; Names for status, select and
// multi-select options; Pages for a relation's page ids; People for user
// ids. Clear empties the property instead.
type Value struct {
	Type     string
	Text     string
	Names    []string
	Pages    []string
	People   []string
	Number   *float64
	Checkbox *bool
	Clear    bool
}

type queryBody struct {
	Filter      any    `json:"filter,omitempty"`
	PageSize    int    `json:"page_size,omitempty"`
	StartCursor string `json:"start_cursor,omitempty"`
}

type list[T any] struct {
	Results    []T     `json:"results"`
	HasMore    bool    `json:"has_more"`
	NextCursor *string `json:"next_cursor"`
}

// Database retrieves a database.
func (c *Client) Database(ctx context.Context, id string) (*Database, error) {
	var db Database

	return &db, c.do(ctx, http.MethodGet, "/databases/"+id, nil, nil, &db)
}

// DataSource retrieves a data source with its schema.
func (c *Client) DataSource(ctx context.Context, id string) (*DataSource, error) {
	var ds DataSource

	return &ds, c.do(ctx, http.MethodGet, "/data_sources/"+id, nil, nil, &ds)
}

// Query runs q against a data source. With all set it follows every page;
// otherwise it returns the first.
func (c *Client) Query(ctx context.Context, dataSource string, q Query, all bool) ([]Page, error) {
	body := queryBody{Filter: restFilter(q.Filters), PageSize: cmp.Or(q.PageSize, pageSize)}

	var pages []Page

	for {
		var res list[Page]
		if err := c.do(ctx, http.MethodPost, "/data_sources/"+dataSource+"/query", nil, body, &res); err != nil {
			return nil, err
		}

		pages = append(pages, res.Results...)
		if !all || !res.HasMore || res.NextCursor == nil {
			return pages, nil
		}

		body.StartCursor = *res.NextCursor
	}
}

// restFilter renders filters in the API's shape, and-ed when several.
func restFilter(filters []Filter) any {
	all := make([]any, len(filters))
	for i, f := range filters {
		all[i] = map[string]any{"property": f.Property, f.Type: map[string]any{string(f.Op): f.Value}}
	}

	switch len(all) {
	case 0:
		return nil
	case 1:
		return all[0]
	default:
		return map[string]any{"and": all}
	}
}

// Page retrieves a page.
func (c *Client) Page(ctx context.Context, id string) (*Page, error) {
	var p Page

	return &p, c.do(ctx, http.MethodGet, "/pages/"+id, nil, nil, &p)
}

// CreatePage creates a page. It is the client's one write: only a 429 is
// retried, and a failure that leaves the outcome open is an
// *UnknownOutcomeError.
func (c *Client) CreatePage(ctx context.Context, p Draft) (*Page, error) {
	props := make(map[string]any, len(p.Properties))
	for name, v := range p.Properties {
		props[name] = map[string]any{v.Type: restValue(v)}
	}

	body := map[string]any{
		"parent":     map[string]any{"type": "data_source_id", "data_source_id": p.DataSource},
		"properties": props,
	}
	if p.Markdown != "" {
		body["markdown"] = p.Markdown
	}

	var created Page

	return &created, c.send(ctx, true, http.MethodPost, "/pages", nil, body, &created)
}

// UpdatePage sets property values on a page. The values are absolute, so
// it is retried like a read.
func (c *Client) UpdatePage(ctx context.Context, pageID string, props map[string]Value) error {
	body := make(map[string]any, len(props))
	for name, v := range props {
		body[name] = map[string]any{v.Type: restValue(v)}
	}

	var updated Page

	return c.do(ctx, http.MethodPatch, "/pages/"+pageID, nil, map[string]any{"properties": body}, &updated)
}

// CreateComment posts markdown, converted to rich text, as a page-level
// comment. Like CreatePage, only a 429 is retried.
func (c *Client) CreateComment(ctx context.Context, pageID, markdown string) (string, error) {
	rich, err := commentRichText(markdown)
	if err != nil {
		return "", err
	}

	body := map[string]any{"parent": map[string]any{"page_id": pageID}, "rich_text": rich}

	var created struct {
		ID string `json:"id"`
	}

	if err := c.send(ctx, true, http.MethodPost, "/comments", nil, body, &created); err != nil {
		return "", err
	}

	return created.ID, nil
}

// Users lists the workspace's people whose name or email holds query,
// every page of them; bots are left out.
func (c *Client) Users(ctx context.Context, query string) ([]User, error) {
	q := url.Values{"page_size": {strconv.Itoa(pageSize)}}

	var users []User

	for {
		var res list[struct {
			ID     string `json:"id"`
			Type   string `json:"type"`
			Name   string `json:"name"`
			Person struct {
				Email string `json:"email"`
			} `json:"person"`
		}]

		if err := c.do(ctx, http.MethodGet, "/users", q, nil, &res); err != nil {
			return nil, err
		}

		for _, u := range res.Results {
			if u.Type == "person" && matchesUser(u.Name, u.Person.Email, query) {
				users = append(users, User{ID: u.ID, Name: u.Name, Email: u.Person.Email})
			}
		}

		if !res.HasMore || res.NextCursor == nil {
			return users, nil
		}

		q.Set("start_cursor", *res.NextCursor)
	}
}

func matchesUser(name, email, query string) bool {
	q := strings.ToLower(query)

	return strings.Contains(strings.ToLower(name), q) || strings.Contains(strings.ToLower(email), q)
}

// restValue renders a property value in the API's request shape.
func restValue(v Value) any {
	named := func(n string) any { return map[string]any{"name": n} }

	if v.Clear {
		if v.Type == "rich_text" || v.Type == "multi_select" || v.Type == "people" || v.Type == "relation" {
			return []any{}
		}

		return nil
	}

	switch v.Type {
	case "title":
		return []any{map[string]any{"text": map[string]any{"content": v.Text}}}
	case "relation":
		refs := make([]any, len(v.Pages))
		for i, id := range v.Pages {
			refs[i] = map[string]any{"id": id}
		}

		return refs
	case "multi_select":
		opts := make([]any, len(v.Names))
		for i, n := range v.Names {
			opts[i] = named(n)
		}

		return opts
	case "people":
		people := make([]any, len(v.People))
		for i, id := range v.People {
			people[i] = map[string]any{"id": id}
		}

		return people
	case "rich_text":
		return []any{map[string]any{"text": map[string]any{"content": v.Text}}}
	case "url", "email", "phone_number":
		return v.Text
	case "number":
		return v.Number
	case "checkbox":
		return v.Checkbox
	case "date":
		return map[string]any{"start": v.Text}
	default: // status, select
		if len(v.Names) == 0 {
			return nil
		}

		return named(v.Names[0])
	}
}

// PageMarkdown retrieves a page's body as markdown.
func (c *Client) PageMarkdown(ctx context.Context, id string) (*Markdown, error) {
	var md Markdown

	return &md, c.do(ctx, http.MethodGet, "/pages/"+id+"/markdown", nil, nil, &md)
}

// Comments lists a page's comments, every page of them.
func (c *Client) Comments(ctx context.Context, pageID string) ([]Comment, error) {
	q := url.Values{"block_id": {pageID}, "page_size": {strconv.Itoa(pageSize)}}

	var comments []Comment

	for {
		var res list[Comment]
		if err := c.do(ctx, http.MethodGet, "/comments", q, nil, &res); err != nil {
			return nil, err
		}

		comments = append(comments, res.Results...)
		if !res.HasMore || res.NextCursor == nil {
			return comments, nil
		}

		q.Set("start_cursor", *res.NextCursor)
	}
}

// do sends a read, retrying a 429 or a transient 5xx a few times after
// Retry-After: a read changes nothing, so a retry is safe.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	return c.send(ctx, false, method, path, query, body, out)
}

// send sends one request. A write retries only a 429, which Notion answers
// before acting; a lost connection or a 5xx on a write is an
// *UnknownOutcomeError, and a 2xx is a write that happened, readable or not.
func (c *Client) send(ctx context.Context, write bool, method, path string, query url.Values, body, out any) error {
	var payload []byte

	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	hc := c.read
	if write {
		hc = c.write
	}

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err // nothing was sent
		}

		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		if err != nil {
			return err
		}

		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Notion-Version", APIVersion)

		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := hc.Do(req)
		if err != nil {
			if write {
				return &UnknownOutcomeError{Err: fmt.Errorf("notion: %w", err)}
			}

			return fmt.Errorf("notion: %w", err)
		}

		data, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if retryable(write, resp.StatusCode) && attempt < maxRetries {
			if err := sleep(ctx, retryAfter(resp.Header.Get("Retry-After"))); err != nil {
				return err
			}

			continue
		}

		if resp.StatusCode/100 != 2 {
			apiErr := &APIError{Status: resp.StatusCode}
			_ = json.Unmarshal(data, apiErr)

			if write && resp.StatusCode/100 == 5 {
				return &UnknownOutcomeError{Err: apiErr}
			}

			return apiErr
		}

		if readErr == nil {
			if err := json.Unmarshal(data, out); err != nil {
				readErr = fmt.Errorf("decoding %s: %w", path, err)
			}
		}

		if readErr != nil && write {
			return fmt.Errorf("notion: %s %s succeeded, but its answer could not be read: %w", method, path, readErr)
		}

		if readErr != nil {
			return fmt.Errorf("notion: %w", readErr)
		}

		return nil
	}
}

func retryable(write bool, status int) bool {
	if write {
		return status == http.StatusTooManyRequests
	}

	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryAfter(h string) time.Duration {
	secs, err := strconv.Atoi(h)
	if err != nil || secs < 0 {
		return time.Second
	}

	return min(time.Duration(secs)*time.Second, maxRetryWait)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
