package notion

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amberpixels/f10-drivers/config"
	"github.com/amberpixels/f10-drivers/contract"
)

// The hosted server's tools the backend calls, and no others.
const (
	toolFetch    = "notion-fetch"
	toolQuery    = "notion-query-data-sources"
	toolComments = "notion-get-comments"
	toolUsers    = "notion-get-users"
	toolCreate   = "notion-create-pages"
	toolUpdate   = "notion-update-page"
	toolComment  = "notion-create-comment"
)

// errNoUniqueID is a Unique ID value with no number: no task.
var errNoUniqueID = errors.New("no Unique ID number")

// rowsLimit is the most rows one rows-mode query answers; it has no cursor.
const rowsLimit = 100

// mcpBackend answers the tracker's calls through Notion's hosted MCP server.
// Its tools answer text meant for an LLM: each call below names the tool and
// what it parses, and notiontest's MCP fake serves the same shapes.
type mcpBackend struct {
	c      *mcpClient
	prefix string
	idProp string
	warn   io.Writer

	schema  map[string]PropertySchema // the data source's, to read rows by
	created string                    // its created_time property, if any

	mu    sync.Mutex              // guards users and pages: prefetchAfter calls Page concurrently
	users map[string]string       // user id to name
	pages map[string]*fetchedPage // normalized page id to its fetch
}

type fetchedPage struct {
	page Page
	md   Markdown
}

func newMCPBackend(ctx context.Context, cfg *config.Config, server string, warn io.Writer) (*mcpBackend, error) {
	if server == "" {
		server = DefaultMCPURL
	}

	sess, err := LoadSession(ctx, server)
	if err != nil {
		return nil, err
	}

	return &mcpBackend{
		c:      newMCPClient(server, sess, clientVersion()),
		prefix: cfg.Prefix,
		idProp: cfg.Notion.Properties.ID,
		warn:   warn,
		users:  map[string]string{},
		pages:  map[string]*fetchedPage{},
	}, nil
}

func clientVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}

	return "dev"
}

// fetchResult is notion-fetch's answer: the object's url and a text body
// of tagged sections.
type fetchResult struct {
	Metadata struct {
		Type string `json:"type"`
	} `json:"metadata"`
	URL             string   `json:"url"`
	Text            string   `json:"text"`
	Truncated       bool     `json:"truncated"`
	UnknownBlockIDs []string `json:"unknown_block_ids"`
}

var pageDatabase = regexp.MustCompile(`<ancestor-\d+-database url="([^"]+)"`)

var dataSourceTag = regexp.MustCompile(`<data-source url="\{*collection://([0-9a-fA-F-]+)\}*"`)

// Database fetches a database: its url, and its data sources from the
// <data-source url="collection://..."> tags.
func (m *mcpBackend) Database(ctx context.Context, id string) (*Database, error) {
	res, err := m.fetch(ctx, id, "database")

	var inner *fetchResult
	if err != nil && m.c.call(ctx, toolFetch, map[string]any{"id": id}, false, &inner) == nil &&
		inner.Metadata.Type == "page" {
		if db := pageDatabase.FindStringSubmatch(inner.Text); db != nil {
			return nil, fmt.Errorf("notion.database: %s is a page in the database %s; set notion.database to that",
				id, bareURL(db[1]))
		}
	}

	if err != nil {
		return nil, err
	}

	db := &Database{URL: bareURL(res.URL)}
	if db.URL == "" {
		db.URL = "https://www.notion.so/" + normalize(id)
	}

	for _, sub := range dataSourceTag.FindAllStringSubmatch(res.Text, -1) {
		if !slices.ContainsFunc(db.DataSources, func(d DataSourceRef) bool { return d.ID == sub[1] }) {
			db.DataSources = append(db.DataSources, DataSourceRef{ID: sub[1]})
		}
	}

	return db, nil
}

// mcpSchemaProp is one property in <data-source-state>'s schema. A status
// property groups its options; the others list them.
type mcpSchemaProp struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Options []Option        `json:"options"`
	Groups  json.RawMessage `json:"groups"`
}

// MCP's words for the property types the tracker checks by their REST names.
var mcpTypes = map[string]string{"auto_increment_id": "unique_id", "person": "people", "text": "rich_text"}

// DataSource fetches collection://<id> and reads the schema from the
// <data-source-state> JSON. The schema carries no Unique ID prefix: Page
// checks it on each page instead.
func (m *mcpBackend) DataSource(ctx context.Context, id string) (*DataSource, error) {
	res, err := m.fetch(ctx, "collection://"+dashed(id), "data_source")
	if err != nil {
		return nil, err
	}

	var state struct {
		Schema map[string]mcpSchemaProp `json:"schema"`
	}

	if err := json.Unmarshal([]byte(section(res.Text, "data-source-state")), &state); err != nil {
		return nil, fmt.Errorf("notion mcp: %s: reading <data-source-state>: %w", toolFetch, err)
	}

	ds := &DataSource{ID: dashed(id), Properties: map[string]PropertySchema{}}
	if dbID, ok := config.NotionID(res.URL); ok {
		ds.Parent.DatabaseID = dbID
	}

	var created []string

	for key, p := range state.Schema {
		// The schema keys an "id" or "url" property as userDefined:<name>;
		// its own name is the one the config uses.
		name := cmp.Or(p.Name, strings.TrimPrefix(key, "userDefined:"))

		s := PropertySchema{Type: p.Type}
		if t, ok := mcpTypes[p.Type]; ok {
			s.Type = t
		}

		opts := p.Options
		if len(p.Groups) > 0 {
			opts = append(opts, groupOptions(p.Groups)...)
		}

		switch s.Type {
		case "status":
			s.Status.Options = opts
		case "select":
			s.Select.Options = opts
		case "multi_select":
			s.MultiSelect.Options = opts
		case "created_time":
			created = append(created, name)
		}

		ds.Properties[name] = s
	}

	slices.Sort(created)

	m.schema = ds.Properties
	if len(created) > 0 {
		m.created = created[0]
	}

	return ds, nil
}

// groupOptions flattens a status property's groups, in the order Notion
// lists them.
func groupOptions(raw json.RawMessage) []Option {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}

	var opts []Option

	for dec.More() {
		if _, err := dec.Token(); err != nil {
			return opts
		}

		var group []Option
		if err := dec.Decode(&group); err != nil {
			return opts
		}

		opts = append(opts, group...)
	}

	return opts
}

// Query runs notion-query-data-sources in rows mode: flat rows keyed by
// property name, and has_more with no cursor, so all is answered only when
// one page holds every match.
func (m *mcpBackend) Query(ctx context.Context, dataSource string, q Query, all bool) ([]Page, error) {
	data := map[string]any{
		"mode":            "rows",
		"data_source_url": "collection://" + dashed(dataSource),
		"limit":           min(cmp.Or(q.PageSize, rowsLimit), rowsLimit),
	}

	if len(q.Filters) > 0 {
		filters := make([]any, len(q.Filters))
		for i, f := range q.Filters {
			mf, err := mcpFilter(f)
			if err != nil {
				return nil, err
			}

			filters[i] = mf
		}

		data["filter"] = map[string]any{"type": "group", "operator": "and", "filters": filters}
	}

	var res struct {
		Results []map[string]any `json:"results"`
		HasMore bool             `json:"has_more"`
	}

	if err := m.c.call(ctx, toolQuery, map[string]any{"data": data}, false, &res); err != nil {
		return nil, err
	}

	if all && res.HasMore {
		_, _ = fmt.Fprintf(m.warn, "warn: more than %d tasks match, and Notion's MCP server cannot page past them; "+
			"transport = \"api\" answers this verb\n", rowsLimit)

		return nil, fmt.Errorf("%w: more than %d matches over MCP", contract.ErrUnsupported, rowsLimit)
	}

	pages := make([]Page, 0, len(res.Results))

	for _, row := range res.Results {
		url, _ := row["url"].(string)

		p, err := m.page(ctx, url, row)
		if err != nil {
			return nil, err
		}

		// Notion drops a filter on a property it does not know and answers
		// every row, so a row the filters reject is a wrong answer, not noise.
		for _, f := range q.Filters {
			if !matches(p, f) {
				return nil, fmt.Errorf("notion mcp: %s ignored the filter on %q and answered %s; "+
					"check notion.properties against the database", toolQuery, f.Property, p.URL)
			}
		}

		pages = append(pages, *p)
	}

	return pages, nil
}

// matches checks a page against a filter the way Notion should have.
func matches(p *Page, f Filter) bool {
	prop := p.Properties[f.Property]
	want := fmt.Sprint(f.Value)

	switch {
	case f.Type == "unique_id":
		return prop.UniqueID != nil && prop.UniqueID.Number != nil && strconv.Itoa(*prop.UniqueID.Number) == want
	case f.Op == OpEquals:
		return optionName(prop) == want
	case f.Op == OpNotEquals:
		return optionName(prop) != want
	case f.Type == "title":
		return strings.Contains(strings.ToLower(plain(prop.Title)), strings.ToLower(want))
	case f.Type == "relation":
		return slices.ContainsFunc(prop.Relation, func(r Ref) bool { return normalize(r.ID) == normalize(want) })
	default:
		return true
	}
}

// mcpFilter renders a filter as rows mode takes it.
func mcpFilter(f Filter) (map[string]any, error) {
	exact := map[string]any{"type": "exact", "value": f.Value}
	out := map[string]any{"type": "property", "property": valueKey(f.Property), "propertyType": f.Type, "value": exact}

	switch {
	case f.Type == "unique_id" && f.Op == OpEquals:
		out["propertyType"], out["operator"] = "auto_increment_id", "number_equals"
	case f.Type == "status" && (f.Op == OpEquals || f.Op == OpNotEquals):
		out["operator"] = map[Op]string{OpEquals: "status_is", OpNotEquals: "status_is_not"}[f.Op]
		out["value"] = map[string]any{"type": "is_option", "value": f.Value}
	case f.Type == "select" && (f.Op == OpEquals || f.Op == OpNotEquals):
		out["operator"] = map[Op]string{OpEquals: "enum_is", OpNotEquals: "enum_is_not"}[f.Op]
	case f.Type == "title" && f.Op == OpContains:
		out["operator"] = "string_contains"
	case f.Type == "relation" && f.Op == OpContains:
		id, _ := f.Value.(string)
		out["operator"], exact["value"] = "relation_contains", dashed(id)
	default:
		return nil, fmt.Errorf("notion mcp: no rows-mode filter for %s %s on %q", f.Type, f.Op, f.Property)
	}

	return out, nil
}

// Page fetches a page; PageMarkdown reads the same fetch.
func (m *mcpBackend) Page(ctx context.Context, id string) (*Page, error) {
	f, err := m.fetchPage(ctx, id)
	if err != nil {
		return nil, err
	}

	return &f.page, nil
}

// PageMarkdown is the page's <content>, in Notion-flavored markdown.
func (m *mcpBackend) PageMarkdown(ctx context.Context, id string) (*Markdown, error) {
	f, err := m.fetchPage(ctx, id)
	if err != nil {
		return nil, err
	}

	return &f.md, nil
}

// userID takes the id out of user://<id> or user://<id>/<email>.
func userID(uri string) string {
	id, _, _ := strings.Cut(strings.TrimPrefix(uri, "user://"), "/")

	return id
}

type discussionsXML struct {
	Discussions []struct {
		Context  string `xml:"context,attr"`
		Comments []struct {
			UserURL  string `xml:"user-url,attr"`
			Datetime string `xml:"datetime,attr"`
			Inner    string `xml:",innerxml"`
		} `xml:"comment"`
	} `xml:"discussion"`
}

// emptyBlock is an empty paragraph as Notion-flavored markdown writes it,
// on a line of its own; plain markdown has no such block.
var emptyBlock = regexp.MustCompile(`(?m)^<p ?/>\n?`)

var tag = regexp.MustCompile(`<[^>]+>`)

// Comments reads notion-get-comments' <discussions> XML: the page-level
// discussions, each comment's time, text and author by name, else email.
func (m *mcpBackend) Comments(ctx context.Context, pageID string) ([]Comment, error) {
	var res struct {
		Text string `json:"text"`
	}

	if err := m.c.call(ctx, toolComments, map[string]any{"page_id": pageID}, false, &res); err != nil {
		return nil, err
	}

	var doc discussionsXML

	dec := xml.NewDecoder(strings.NewReader(res.Text))
	dec.Strict = false

	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("notion mcp: %s: reading <discussions>: %w", toolComments, err)
	}

	var comments []Comment

	for _, d := range doc.Discussions {
		if d.Context != "" && d.Context != "page" {
			continue
		}

		for _, c := range d.Comments {
			name, err := m.userName(ctx, userID(c.UserURL))
			if err != nil {
				return nil, err
			}

			if _, email, ok := strings.Cut(strings.TrimPrefix(c.UserURL, "user://"), "/"); name == "" && ok {
				name = email
			}

			comment := Comment{CreatedTime: c.Datetime}
			comment.RichText = []RichText{{PlainText: html.UnescapeString(tag.ReplaceAllString(c.Inner, ""))}}

			if name != "" {
				comment.DisplayName.ResolvedName = &name
			}

			comments = append(comments, comment)
		}
	}

	return comments, nil
}

// CreatePage runs notion-create-pages with the properties in their SQLite
// value form, then fetches the new page for its Unique ID.
func (m *mcpBackend) CreatePage(ctx context.Context, np Draft) (*Page, error) {
	props := sqliteValues(np.Properties)

	pg := map[string]any{"properties": props}
	if np.Markdown != "" {
		pg["content"] = np.Markdown
	}

	args := map[string]any{
		"parent":      map[string]any{"type": "data_source_id", "data_source_id": dashed(np.DataSource)},
		"pages":       []any{pg},
		"allow_async": false,
	}

	var res json.RawMessage
	if err := m.c.call(ctx, toolCreate, args, true, &res); err != nil {
		return nil, err
	}

	id, ok := createdID(res)
	if !ok {
		return nil, fmt.Errorf("notion mcp: %s succeeded, but its answer names no page: %s", toolCreate, res)
	}

	p, err := m.Page(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("created https://www.notion.so/%s, but reading it back failed: %w", normalize(id), err)
	}

	return p, nil
}

// UpdatePage runs notion-update-page's update_properties with the values in
// their SQLite form. The values are absolute, so it is retried like a read.
func (m *mcpBackend) UpdatePage(ctx context.Context, pageID string, props map[string]Value) error {
	values := sqliteValues(props)

	args := map[string]any{
		"page_id":     dashed(pageID),
		"command":     "update_properties",
		"properties":  values,
		"allow_async": false,
	}

	var res json.RawMessage
	if err := m.c.call(ctx, toolUpdate, args, false, &res); err != nil {
		return err
	}

	m.mu.Lock()
	delete(m.pages, normalize(pageID))
	m.mu.Unlock()

	return nil
}

// CreateComment runs notion-create-comment with the markdown converted to
// rich text, as the REST transport sends it, so both post the same comment.
func (m *mcpBackend) CreateComment(ctx context.Context, pageID, markdown string) (string, error) {
	rich, err := commentRichText(markdown)
	if err != nil {
		return "", err
	}

	var res json.RawMessage

	args := map[string]any{"page_id": dashed(pageID), "rich_text": rich}
	if err := m.c.call(ctx, toolComment, args, true, &res); err != nil {
		return "", err
	}

	id, ok := commentID(res, pageID)
	if !ok {
		return "", fmt.Errorf("notion mcp: %s succeeded, but its answer names no comment: %s", toolComment, res)
	}

	return id, nil
}

// commentID finds the new comment's id: the answer's id field, else the
// first Notion id in it other than the page's own.
func commentID(raw json.RawMessage, pageID string) (string, bool) {
	var named struct {
		ID string `json:"id"`
	}

	if json.Unmarshal(raw, &named) == nil && named.ID != "" {
		return named.ID, true
	}

	for _, id := range notionIDs.FindAllString(string(raw), -1) {
		if n := strings.ToLower(normalize(id)); n != strings.ToLower(normalize(pageID)) {
			return dashed(n), true
		}
	}

	return "", false
}

var notionIDs = regexp.MustCompile(`[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}`)

// Users runs notion-get-users with query, every page of it; bots are left
// out.
func (m *mcpBackend) Users(ctx context.Context, query string) ([]User, error) {
	args := map[string]any{"query": query}

	var users []User

	for {
		var res struct {
			Results []struct {
				ID    string `json:"id"`
				Type  string `json:"type"`
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"results"`
			HasMore    bool   `json:"has_more"`
			NextCursor string `json:"next_cursor"`
		}

		if err := m.c.call(ctx, toolUsers, args, false, &res); err != nil {
			return nil, err
		}

		for _, u := range res.Results {
			if u.Type == "person" {
				users = append(users, User{ID: u.ID, Name: strings.TrimSpace(u.Name), Email: u.Email})
			}
		}

		if !res.HasMore || res.NextCursor == "" {
			return users, nil
		}

		args["start_cursor"] = res.NextCursor
	}
}

func (m *mcpBackend) fetch(ctx context.Context, id, want string) (*fetchResult, error) {
	var res fetchResult
	if err := m.c.call(ctx, toolFetch, map[string]any{"id": id}, false, &res); err != nil {
		return nil, err
	}

	if res.Metadata.Type != want {
		return nil, fmt.Errorf("notion mcp: %s %s is a %q, want a %s", toolFetch, id, res.Metadata.Type, want)
	}

	return &res, nil
}

// fetchPage runs notion-fetch on a page once per run and reads its
// <properties> JSON and its <content>.
func (m *mcpBackend) fetchPage(ctx context.Context, id string) (*fetchedPage, error) {
	m.mu.Lock()
	f, ok := m.pages[normalize(id)]
	m.mu.Unlock()

	if ok {
		return f, nil
	}

	res, err := m.fetch(ctx, id, "page")
	if err != nil {
		return nil, err
	}

	var values map[string]any
	if err := json.Unmarshal([]byte(section(res.Text, "properties")), &values); err != nil {
		return nil, fmt.Errorf("notion mcp: %s: reading <properties>: %w", toolFetch, err)
	}

	p, err := m.page(ctx, res.URL, values)
	if err != nil {
		return nil, err
	}

	f = &fetchedPage{
		page: *p,
		md: Markdown{
			Markdown:        emptyBlock.ReplaceAllString(section(res.Text, "content"), ""),
			Truncated:       res.Truncated,
			UnknownBlockIDs: res.UnknownBlockIDs,
		},
	}
	m.mu.Lock()
	m.pages[normalize(p.ID)] = f
	m.mu.Unlock()

	return f, nil
}

// page builds a Page from a row or a fetch's properties, typed by the
// schema: options by name, multi-selects, people and relations as arrays or
// JSON-array strings, the Unique ID as a number with or without its prefix.
func (m *mcpBackend) page(ctx context.Context, url string, values map[string]any) (*Page, error) {
	id, ok := config.NotionID(url)
	if !ok {
		return nil, fmt.Errorf("notion mcp: a page without a Notion url: %q", url)
	}

	p := &Page{ID: dashed(id), URL: bareURL(url), Properties: map[string]Property{}}

	if s, ok := values[valueKey(m.created)].(string); ok && m.created != "" {
		p.CreatedTime = s
	}

	for name, s := range m.schema {
		if s.Type == "date" {
			if start := str(values["date:"+name+":start"]); start != "" {
				d := &Date{Start: start}
				if end := str(values["date:"+name+":end"]); end != "" {
					d.End = &end
				}

				p.Properties[name] = Property{Type: s.Type, Date: d}
			}

			continue
		}

		v, ok := values[valueKey(name)]
		if !ok {
			continue
		}

		prop := Property{Type: s.Type}

		switch s.Type {
		case "title":
			prop.Title = []RichText{{PlainText: str(v)}}
		case "unique_id":
			u, err := m.uniqueID(name, v)
			if err != nil && !errors.Is(err, errNoUniqueID) {
				return nil, err
			}

			prop.UniqueID = u
		case "status", "select":
			if n := str(v); n != "" {
				if s.Type == "status" {
					prop.Status = &Option{Name: n}
				} else {
					prop.Select = &Option{Name: n}
				}
			}
		case "multi_select":
			for _, n := range strs(v) {
				prop.MultiSelect = append(prop.MultiSelect, Option{Name: n})
			}
		case "people":
			for _, u := range strs(v) {
				name, err := m.userName(ctx, userID(u))
				if err != nil {
					return nil, err
				}

				if name != "" {
					prop.People = append(prop.People, Person{Name: name})
				}
			}
		case "relation":
			for _, r := range strs(v) {
				if rid, ok := config.NotionID(r); ok {
					prop.Relation = append(prop.Relation, Ref{ID: dashed(rid)})
				}
			}
		case "url":
			prop.URL = nonEmpty(str(v))
		case "email":
			prop.Email = nonEmpty(str(v))
		case "phone_number":
			prop.PhoneNumber = nonEmpty(str(v))
		case "rich_text":
			if t := str(v); t != "" {
				prop.RichText = []RichText{{PlainText: t}}
			}
		case "number":
			if n, ok := v.(float64); ok {
				prop.Number = &n
			}
		case "checkbox":
			if t := str(v); t != "" {
				b := t == "__YES__"
				prop.Checkbox = &b
			}
		}

		p.Properties[name] = prop
	}

	return p, nil
}

// uniqueID reads "MKT-12" (a fetch) or "12" (a row). A prefix other than
// the configured one fails as the REST path fails on the schema's prefix.
func (m *mcpBackend) uniqueID(name string, v any) (*UniqueID, error) {
	s := str(v)
	prefix, num, found := strings.Cut(s, "-")

	if !found {
		prefix, num = "", s
	}

	n, err := strconv.Atoi(num)
	if err != nil {
		return nil, errNoUniqueID
	}

	if prefix != "" && name == m.idProp && prefix != m.prefix {
		return nil, fmt.Errorf("prefix is %q, but Notion's %q property is prefixed %q", m.prefix, name, prefix)
	}

	u := &UniqueID{Number: &n}
	if prefix != "" {
		u.Prefix = &prefix
	}

	return u, nil
}

// userName resolves a user id through notion-get-users, once per user per
// run; a user Notion does not return has no name.
func (m *mcpBackend) userName(ctx context.Context, id string) (string, error) {
	m.mu.Lock()
	name, ok := m.users[id]
	m.mu.Unlock()

	if ok || id == "" {
		return name, nil
	}

	var res struct {
		Results []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"results"`
	}

	err := m.c.call(ctx, toolUsers, map[string]any{"user_id": id}, false, &res)
	if _, ok := errors.AsType[*ToolError](err); ok || isNotFound(err) {
		err = nil
	}

	if err != nil {
		return "", err
	}

	for _, u := range res.Results {
		if normalize(u.ID) == normalize(id) {
			name = strings.TrimSpace(u.Name)
		}
	}

	m.mu.Lock()
	m.users[id] = name
	m.mu.Unlock()

	return name, nil
}

// createdID finds the new page's id: pages[0].id or .url, else the first
// Notion id anywhere in the answer.
func createdID(raw json.RawMessage) (string, bool) {
	var res struct {
		Pages []struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"pages"`
	}

	if json.Unmarshal(raw, &res) == nil && len(res.Pages) > 0 {
		if id, ok := config.NotionID(res.Pages[0].ID + " " + res.Pages[0].URL); ok {
			return id, true
		}
	}

	return config.NotionID(string(raw))
}

// valueKey is a property's key in the tools' values: one named "id" or
// "url", in any case, would clash with the page's own and is prefixed.
func valueKey(name string) string {
	if l := strings.ToLower(name); l == "id" || l == "url" {
		return "userDefined:" + name
	}

	return name
}

// section returns the text between <name ...> and </name>, trimmed.
func section(text, name string) string {
	start := strings.Index(text, "<"+name)
	if start < 0 {
		return ""
	}

	open := strings.IndexByte(text[start:], '>')
	end := strings.LastIndex(text, "</"+name+">")

	if open < 0 || end < start+open {
		return ""
	}

	return strings.TrimSpace(text[start+open+1 : end])
}

// str reads a scalar value as text.
func str(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

// strs reads an array, or a string holding a JSON array, as strings.
func strs(v any) []string {
	switch v := v.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s := str(e); s != "" {
				out = append(out, s)
			}
		}

		return out
	case string:
		if v == "" {
			return nil
		}

		var arr []any
		if strings.HasPrefix(v, "[") && json.Unmarshal([]byte(v), &arr) == nil {
			return strs(arr)
		}

		return []string{v}
	default:
		return nil
	}
}

// dashed formats a Notion id as a UUID.
func dashed(id string) string {
	n := normalize(id)
	if len(n) != 32 {
		return id
	}

	return n[:8] + "-" + n[8:12] + "-" + n[12:16] + "-" + n[16:20] + "-" + n[20:]
}

// bareURL drops a url's query, such as the ?pvs= Notion adds.
func bareURL(u string) string {
	u, _, _ = strings.Cut(u, "?")

	return u
}

// sqliteValues renders property values in the SQLite form the create and
// update tools take.
func sqliteValues(props map[string]Value) map[string]any {
	values := map[string]any{}

	for name, v := range props {
		key := valueKey(name)

		switch v.Type {
		case "date":
			// The end goes too, so a range collapses to the new date as over REST.
			start, end := "date:"+name+":start", "date:"+name+":end"
			values[end] = nil

			if v.Clear {
				values[start] = nil

				continue
			}

			values[start] = v.Text
			values["date:"+name+":is_datetime"] = map[bool]int{true: 1, false: 0}[len(v.Text) > len(time.DateOnly)]
		case "checkbox":
			values[key] = map[bool]string{true: "__YES__", false: "__NO__"}[v.Checkbox != nil && *v.Checkbox]
		case "people":
			values[key] = v.People
		case "multi_select":
			values[key] = v.Names
		case "relation":
			values[key] = v.Pages
		case "status", "select":
			if v.Clear || len(v.Names) == 0 {
				values[key] = nil
			} else {
				values[key] = v.Names[0]
			}
		case "number":
			if v.Clear {
				values[key] = nil
			} else {
				values[key] = v.Number
			}
		default: // url, rich_text, email, phone_number, title
			if v.Clear {
				values[key] = nil
			} else {
				values[key] = v.Text
			}
		}
	}

	return values
}
