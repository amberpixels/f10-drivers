package notion

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amberpixels/f10-drivers/config"
	"github.com/amberpixels/f10-drivers/contract"
)

// The property types each configured property must have.
var wantTypes = map[string][]string{
	"id":       {"unique_id"},
	"title":    {"title"},
	"status":   {"status", "select"},
	"priority": {"select", "status"},
	"assignee": {"people"},
	"labels":   {"multi_select", "select"},
	"parent":   {"relation"},
	"after":    {"relation"},
}

// settableTypes are the property types --set can write.
var settableTypes = []string{
	"url", "rich_text", "number", "checkbox", "date", "email", "phone_number", "select", "status", "multi_select",
}

// Tracker is the Notion contract.Tracker.
type Tracker struct {
	b    backend
	cfg  *config.Config
	n    *config.Notion
	warn io.Writer

	ds    *DataSource
	props config.Properties // the configured names, title filled in from the schema
	ids   map[string]string // page id to task id, for after relations
}

var (
	_ contract.Tracker   = (*Tracker)(nil)
	_ contract.Creator   = (*Tracker)(nil)
	_ contract.Commenter = (*Tracker)(nil)
	_ contract.Updater   = (*Tracker)(nil)
)

// backend is the way into Notion: the REST API with an integration token,
// or the hosted MCP server as the logged-in user.
type backend interface {
	Database(ctx context.Context, id string) (*Database, error)
	DataSource(ctx context.Context, id string) (*DataSource, error)
	// Query returns the first page of matches, or with all set every match;
	// a backend that cannot page returns an error wrapping
	// contract.ErrUnsupported when more remain.
	Query(ctx context.Context, dataSource string, q Query, all bool) ([]Page, error)
	Page(ctx context.Context, id string) (*Page, error)
	PageMarkdown(ctx context.Context, id string) (*Markdown, error)
	Comments(ctx context.Context, pageID string) ([]Comment, error)
	CreatePage(ctx context.Context, p Draft) (*Page, error)
	// UpdatePage sets property values; it is absolute, so safe to retry.
	UpdatePage(ctx context.Context, pageID string, props map[string]Value) error
	// CreateComment posts a page-level comment and returns its id.
	CreateComment(ctx context.Context, pageID, markdown string) (string, error)
	// Users returns the workspace's people whose name or email holds query.
	Users(ctx context.Context, query string) ([]User, error)
}

// Endpoints override where each way in reaches Notion (tests, proxies).
type Endpoints struct {
	API string // the REST API root
	MCP string // the MCP server url
}

// New returns the tracker for cfg over its configured transport; warn
// receives non-fatal notices such as a truncated body.
func New(ctx context.Context, cfg *config.Config, ep Endpoints, warn io.Writer) (*Tracker, error) {
	if cfg.Notion == nil {
		return nil, fmt.Errorf("%s has no [notion] table", cfg.Path)
	}

	var b backend

	switch cfg.Notion.Transport {
	case config.TransportMCP:
		m, err := newMCPBackend(ctx, cfg, ep.MCP, warn)
		if err != nil {
			return nil, err
		}

		b = m
	default:
		token, err := cfg.Notion.Token()
		if err != nil {
			return nil, err
		}

		b = NewClient(ep.API, token)
	}

	return &Tracker{
		b:    b,
		cfg:  cfg,
		n:    cfg.Notion,
		warn: warn,
		ids:  map[string]string{},
	}, nil
}

// Read prints the task: a title line, its properties, the body Notion
// renders as markdown, then its comments.
func (t *Tracker) Read(ctx context.Context, number string) (string, error) {
	p, err := t.find(ctx, number)
	if err != nil {
		return "", err
	}

	md, err := t.b.PageMarkdown(ctx, p.ID)
	if err != nil {
		return "", err
	}

	id := t.id(number)
	if md.Truncated {
		_, _ = fmt.Fprintf(t.warn, "warn: %s is too long for Notion's markdown export; the body is cut short\n", id)
	}

	if n := len(md.UnknownBlockIDs); n > 0 {
		_, _ = fmt.Fprintf(t.warn, "warn: %s has %d block(s) Notion could not render as markdown\n", id, n)
	}

	comments, err := t.b.Comments(ctx, p.ID)
	if err != nil {
		return "", err
	}

	row, err := t.row(ctx, p)
	if err != nil {
		return "", err
	}

	var b strings.Builder

	fmt.Fprintf(&b, "# %s: %s\n\n", row.ID, row.Title)

	for _, f := range [][2]string{
		{"Status", row.Status},
		{"Priority", row.Priority},
		{"Labels", strings.Join(t.labels(p), ", ")},
		{"Assignee", row.Assignee},
	} {
		if f[1] != "" {
			fmt.Fprintf(&b, "%s: %s\n", f[0], f[1])
		}
	}

	for _, key := range slices.Sorted(maps.Keys(t.props.Extra)) {
		name := t.props.Extra[key]
		if v := display(p.Properties[name]); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", name, v)
		}
	}

	body := strings.TrimSpace(md.Markdown)
	if body != "" {
		b.WriteString("\n" + body + "\n")
	}

	if md.Truncated || len(md.UnknownBlockIDs) > 0 {
		fmt.Fprintf(&b, "\n> Notion's markdown export left part of this page out; the full page: %s\n", p.URL)
	}

	// A task created from f10 keeps its After: line in the body; print it once.
	if row.After != "" && !endsWithLine(body, "After: "+row.After) {
		fmt.Fprintf(&b, "\nAfter: %s\n", row.After)
	}

	if len(comments) > 0 {
		b.WriteString("\n## Comments\n")

		for _, c := range comments {
			fmt.Fprintf(&b, "\n**%s**, %s\n\n%s\n", author(c), stamp(c.CreatedTime), plain(c.RichText))
		}
	}

	return b.String(), nil
}

// URL prints the task's page url.
func (t *Tracker) URL(ctx context.Context, number string) (string, error) {
	p, err := t.find(ctx, number)
	if err != nil {
		return "", err
	}

	return p.URL, nil
}

// Branch fills the configured branch template from the task's title.
func (t *Tracker) Branch(ctx context.Context, number string) (string, error) {
	p, err := t.find(ctx, number)
	if err != nil {
		return "", err
	}

	return t.cfg.BranchName(t.id(number), number, t.title(p)), nil
}

// Index prints the database's url, the task list.
func (t *Tracker) Index(ctx context.Context) (string, error) {
	db := t.n.Database
	if db == "" {
		ds, err := t.dataSource(ctx)
		if err != nil {
			return "", err
		}

		db = ds.Parent.DatabaseID
	}

	d, err := t.b.Database(ctx, db)
	if err != nil {
		return "", err
	}

	return d.URL, nil
}

// List returns the first page of tasks whose status is not done.
func (t *Tracker) List(ctx context.Context) ([]contract.Row, error) {
	if _, err := t.dataSource(ctx); err != nil {
		return nil, err
	}

	var q Query

	if t.props.Status != "" {
		for _, s := range t.n.Done {
			q.Filters = append(q.Filters, t.filter(t.props.Status, OpNotEquals, s))
		}
	}

	return t.rows(ctx, q)
}

// Search returns the tasks whose title contains the query, any status.
func (t *Tracker) Search(ctx context.Context, query string) ([]contract.Row, error) {
	if _, err := t.dataSource(ctx); err != nil {
		return nil, err
	}

	var q Query
	if query != "" {
		q.Filters = []Filter{t.filter(t.props.Title, OpContains, query)}
	}

	return t.rows(ctx, q)
}

// Children returns the tasks whose parent relation holds this task. Without
// a configured parent property the verb is declined.
func (t *Tracker) Children(ctx context.Context, number string) ([]contract.ChildRow, error) {
	if t.n.Properties.Parent == "" {
		return nil, contract.ErrUnsupported
	}

	p, err := t.find(ctx, number)
	if err != nil {
		return nil, err
	}

	pages, err := t.b.Query(ctx, t.ds.ID, Query{Filters: []Filter{t.filter(t.props.Parent, OpContains, p.ID)}}, true)
	if err != nil {
		return nil, err
	}

	t.remember(pages)

	if err := t.prefetchAfter(ctx, pages); err != nil {
		return nil, err
	}

	children := make([]contract.ChildRow, 0, len(pages))

	for i := range pages {
		row, err := t.row(ctx, &pages[i])
		if err != nil {
			return nil, err
		}

		state := "open"
		if t.n.IsDone(row.Status) {
			state = "closed"
		}

		children = append(children, contract.ChildRow{
			ID: row.ID, Title: row.Title, URL: row.URL, State: state, Labels: t.labels(&pages[i]), After: row.After,
		})
	}

	return children, nil
}

// Create makes a page for the task and returns its id. Every property value,
// --set included, is checked against the schema before the page is created:
// Notion rejects an unknown status, and would add an unknown label to the
// schema.
func (t *Tracker) Create(ctx context.Context, task contract.NewTask) (string, error) {
	ds, draft, err := t.draft(ctx, task)
	if err != nil {
		return "", contract.NotWritten(err)
	}

	page, err := t.b.CreatePage(ctx, draft)

	if _, ok := errors.AsType[*UnknownOutcomeError](err); ok {
		return "", fmt.Errorf(
			"%w; the task may have been created: check https://www.notion.so/%s before running create again",
			err, normalize(ds.Parent.DatabaseID))
	}

	if err != nil {
		return "", err
	}

	id := t.taskID(page)
	if id == "" {
		return "", fmt.Errorf("created %s, but Notion gave it no %q number", page.URL, t.props.ID)
	}

	t.remember([]Page{*page})

	return id, nil
}

// Update sets the given properties on a task and returns its id. Every
// value is checked against the schema first, and a flag with no property to
// go to fails: an update whose one change was dropped did nothing.
func (t *Tracker) Update(ctx context.Context, number string, u contract.TaskUpdate) (string, error) {
	p, err := t.find(ctx, number)
	if err != nil {
		return "", err
	}

	props := map[string]Value{}

	for _, f := range []struct{ flag, value, prop string }{
		{"status", u.Status, t.props.Status},
		{"priority", u.Priority, t.props.Priority},
	} {
		if f.value == "" {
			continue
		}

		if f.prop == "" {
			return "", unconfigured(f.flag)
		}

		name, err := t.optionFor(f.flag, f.value)
		if err != nil {
			return "", err
		}

		props[f.prop] = Value{Type: t.ds.Properties[f.prop].Type, Names: []string{name}}
	}

	if len(u.Labels) > 0 {
		if t.props.Labels == "" {
			return "", unconfigured("label")
		}

		names, err := t.labelNames(u.Labels)
		if err != nil {
			return "", err
		}

		props[t.props.Labels] = Value{Type: t.ds.Properties[t.props.Labels].Type, Names: names}
	}

	if u.Assignee != "" {
		if t.props.Assignee == "" {
			return "", unconfigured("assignee")
		}

		user, err := t.assignee(ctx, u.Assignee)
		if err != nil {
			return "", err
		}

		props[t.props.Assignee] = Value{Type: "people", People: []string{user.ID}}
	}

	for _, kv := range u.Set {
		name, v, err := t.setValue(kv.Key, kv.Value)
		if err != nil {
			return "", err
		}

		props[name] = v
	}

	if err := t.b.UpdatePage(ctx, p.ID, props); err != nil {
		return "", err
	}

	return t.id(number), nil
}

// Comment posts markdown as a page-level comment on the task and returns
// the comment's id.
func (t *Tracker) Comment(ctx context.Context, number, markdown string) (string, error) {
	p, err := t.find(ctx, number)
	if err != nil {
		return "", err
	}

	id, err := t.b.CreateComment(ctx, p.ID, markdown)
	if _, ok := errors.AsType[*UnknownOutcomeError](err); ok {
		return "", fmt.Errorf("%w; the comment may have been posted: check %s before running comment again", err, p.URL)
	}

	return id, err
}

// draft reads the schema and checks every property value against it, all
// before any write, so its errors leave nothing behind.
func (t *Tracker) draft(ctx context.Context, task contract.NewTask) (*DataSource, Draft, error) {
	ds, err := t.dataSource(ctx)
	if err != nil {
		return nil, Draft{}, err
	}

	for _, name := range task.Dropped {
		_, _ = fmt.Fprintf(t.warn, "warn: --%s dropped: create does not know it\n", name)
	}

	props := map[string]Value{t.props.Title: {Type: "title", Text: task.Title}}

	for _, f := range []struct{ flag, value, prop string }{
		{"status", task.Status, t.props.Status},
		{"priority", task.Priority, t.props.Priority},
	} {
		if f.value == "" || !t.configured(f.flag, f.prop) {
			continue
		}

		name, err := t.optionFor(f.flag, f.value)
		if err != nil {
			return nil, Draft{}, err
		}

		props[f.prop] = Value{Type: ds.Properties[f.prop].Type, Names: []string{name}}
	}

	if len(task.Labels) > 0 && t.configured("label", t.props.Labels) {
		names, err := t.labelNames(task.Labels)
		if err != nil {
			return nil, Draft{}, err
		}

		props[t.props.Labels] = Value{Type: ds.Properties[t.props.Labels].Type, Names: names}
	}

	for _, kv := range task.Set {
		if kv.Key == "assignee" {
			return nil, Draft{}, errors.New(
				"--set assignee: create takes no assignee; set it with update <n> --assignee",
			)
		}

		name, v, err := t.setValue(kv.Key, kv.Value)
		if err != nil {
			return nil, Draft{}, err
		}

		// A new page has nothing to clear.
		if !v.Clear {
			props[name] = v
		}
	}

	for _, f := range []struct{ flag, number, prop string }{
		{"parent", task.Parent, t.props.Parent},
		{"after", task.After, t.props.After},
	} {
		if f.number == "" || !t.configured(f.flag, f.prop) {
			continue
		}

		p, err := t.find(ctx, f.number)
		if err != nil {
			return nil, Draft{}, fmt.Errorf("--%s: %w", f.flag, err)
		}

		props[f.prop] = Value{Type: "relation", Pages: []string{p.ID}}
	}

	return ds, Draft{DataSource: ds.ID, Properties: props, Markdown: task.Body}, nil
}

func unconfigured(flag string) error {
	return fmt.Errorf("--%s: notion.properties sets no property for it", flag)
}

// assignee resolves an email, or else a name, to the one user it names.
func (t *Tracker) assignee(ctx context.Context, v string) (*User, error) {
	users, err := t.b.Users(ctx, v)
	if err != nil {
		return nil, err
	}

	byEmail := strings.Contains(v, "@")

	var (
		found     []User
		anyEmails bool
	)

	for _, u := range users {
		anyEmails = anyEmails || u.Email != ""

		if (byEmail && strings.EqualFold(u.Email, v)) || (!byEmail && strings.EqualFold(u.Name, v)) {
			found = append(found, u)
		}
	}

	switch {
	case len(found) == 1:
		return &found[0], nil
	case len(found) > 1:
		names := make([]string, len(found))
		for i, u := range found {
			names[i] = fmt.Sprintf("%s <%s>", u.Name, u.Email)
		}

		return nil, fmt.Errorf(
			"--assignee %q matches %d users (%s); give an email",
			v,
			len(found),
			strings.Join(names, ", "),
		)
	case byEmail && len(users) > 0 && !anyEmails:
		return nil, fmt.Errorf("--assignee %q: Notion shows no user's email; "+
			"give the integration the capability to read user information including email addresses", v)
	default:
		return nil, fmt.Errorf(
			"--assignee %q: no Notion user has that %s",
			v,
			map[bool]string{true: "email", false: "name"}[byEmail],
		)
	}
}

// setValue checks one --set against its property's schema type and returns
// the property's name and the value to send; an empty value clears it.
func (t *Tracker) setValue(key, raw string) (string, Value, error) {
	name, ok := t.props.Extra[key]
	if !ok {
		keys := slices.Sorted(maps.Keys(t.props.Extra))
		if flag, ok := map[string]string{"status": "status", "priority": "priority", "labels": "label", "assignee": "assignee"}[key]; ok {
			return "", Value{}, fmt.Errorf("--set %s: use --%s", key, flag)
		}

		return "", Value{}, fmt.Errorf("--set %s: not a key of [notion.properties] (%s)", key, strings.Join(keys, ", "))
	}

	typ := t.ds.Properties[name].Type
	v := Value{Type: typ}
	bad := func(want string) error {
		return fmt.Errorf("--set %s=%q: %q is a %s property, want %s", key, raw, name, typ, want)
	}

	if raw == "" && typ != "checkbox" {
		v.Clear = true

		return name, v, nil
	}

	switch typ {
	case "url":
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", v, bad("an absolute http(s) url")
		}

		v.Text = raw
	case "number":
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return "", v, bad("a number")
		}

		v.Number = &n
	case "checkbox":
		b, err := strconv.ParseBool(raw)
		if err != nil || (raw != "true" && raw != "false") {
			return "", v, bad("true or false")
		}

		v.Checkbox = &b
	case "date":
		if _, err := time.Parse(time.DateOnly, raw); err != nil {
			if _, err := time.Parse(time.RFC3339, raw); err != nil {
				return "", v, bad("a date, YYYY-MM-DD or RFC 3339")
			}
		}

		v.Text = raw
	case "select", "status":
		names := options(t.ds.Properties[name])

		j := indexFold(names, raw)
		if j < 0 {
			return "", v, fmt.Errorf(
				"--set %s=%q: no option of %q matches (%s)",
				key,
				raw,
				name,
				strings.Join(names, ", "),
			)
		}

		v.Names = []string{names[j]}
	case "multi_select":
		// Notion allows no comma in an option name, so the split is unambiguous.
		var picked []string

		for piece := range strings.SplitSeq(raw, ",") {
			piece = strings.TrimSpace(piece)
			if piece == "" {
				return "", v, bad("a comma-separated list of option names")
			}

			if indexFold(picked, piece) < 0 {
				picked = append(picked, piece)
			}
		}

		exact, err := optionNames(t.ds.Properties[name], picked, func(n string) string {
			return fmt.Sprintf("--set %s=%q: %q is not an option of %q", key, raw, n, name)
		})
		if err != nil {
			return "", v, err
		}

		v.Names = exact
	default: // rich_text, email, phone_number
		v.Text = raw
	}

	return name, v, nil
}

// configured reports whether a create flag has a property to go to, and
// warns that the flag is dropped when it has none.
func (t *Tracker) configured(flag, prop string) bool {
	if prop == "" {
		_, _ = fmt.Fprintf(t.warn, "warn: --%s dropped: notion.properties sets no property for it\n", flag)
	}

	return prop != ""
}

// optionFor maps a status or a contract priority level to an option of its
// property, by its exact name. A level maps to the first value
// notion.priority lists for it that is an option, else an option named
// like the level.
func (t *Tracker) optionFor(flag, value string) (string, error) {
	prop, candidates := t.props.Status, []string{value}
	if flag == "priority" {
		prop, candidates = t.props.Priority, append(slices.Clone(t.n.Priority[value]), value)
	}

	names := options(t.ds.Properties[prop])
	for _, c := range candidates {
		if j := indexFold(names, c); j >= 0 {
			return names[j], nil
		}
	}

	return "", fmt.Errorf("--%s %q: no option of %q matches (%s)", flag, value, prop, strings.Join(names, ", "))
}

// labelNames checks each label is an option of the labels property and
// returns the options' exact names; a select property holds one label.
func (t *Tracker) labelNames(labels []string) ([]string, error) {
	prop := t.props.Labels

	exact, err := optionNames(t.ds.Properties[prop], labels, func(l string) string {
		return fmt.Sprintf("--label %q is not an option of %q", l, prop)
	})
	if err != nil {
		return nil, err
	}

	if t.ds.Properties[prop].Type == "select" && len(exact) > 1 {
		return nil, fmt.Errorf("--label: %q is a select property and holds one label, got %d", prop, len(exact))
	}

	return exact, nil
}

// optionNames matches each value to an option of s, case-insensitively,
// and returns the options' exact names; miss words the error for a value
// that matches none, before the options are listed.
func optionNames(s PropertySchema, values []string, miss func(string) string) ([]string, error) {
	names := options(s)
	exact := make([]string, 0, len(values))

	for _, v := range values {
		j := indexFold(names, v)
		if j < 0 {
			return nil, fmt.Errorf("%s (%s)", miss(v), strings.Join(names, ", "))
		}

		exact = append(exact, names[j])
	}

	return exact, nil
}

// endsWithLine reports whether text's last line is line, ignoring
// surrounding whitespace.
func endsWithLine(text, line string) bool {
	text = strings.TrimRight(text, " \t\r\n")

	return strings.TrimSpace(text[strings.LastIndex(text, "\n")+1:]) == line
}

func (t *Tracker) rows(ctx context.Context, q Query) ([]contract.Row, error) {
	pages, err := t.b.Query(ctx, t.ds.ID, q, false)
	if err != nil {
		return nil, err
	}

	t.remember(pages)

	if err := t.prefetchAfter(ctx, pages); err != nil {
		return nil, err
	}

	rows := make([]contract.Row, 0, len(pages))

	for i := range pages {
		row, err := t.row(ctx, &pages[i])
		if err != nil {
			return nil, err
		}

		rows = append(rows, row)
	}

	return rows, nil
}

// row maps a page to a contract row through the configured properties.
func (t *Tracker) row(ctx context.Context, p *Page) (contract.Row, error) {
	row := contract.Row{
		ID:      t.taskID(p),
		Title:   t.title(p),
		Status:  optionName(p.Properties[t.props.Status]),
		URL:     p.URL,
		Created: p.CreatedTime,
	}

	if v := optionName(p.Properties[t.props.Priority]); v != "" {
		row.Priority = t.n.PriorityOf(v)
	}

	if prop, ok := p.Properties[t.props.Assignee]; ok {
		names := make([]string, 0, len(prop.People))
		for _, person := range prop.People {
			names = append(names, person.Name)
		}

		row.Assignee = strings.Join(names, ", ")
	}

	if rel := p.Properties[t.props.After].Relation; t.props.After != "" && len(rel) > 0 {
		after, err := t.idOf(ctx, rel[0].ID)
		if err != nil {
			return row, err
		}

		row.After = after
	}

	return row, nil
}

// find returns the page whose Unique ID is number.
func (t *Tracker) find(ctx context.Context, number string) (*Page, error) {
	ds, err := t.dataSource(ctx)
	if err != nil {
		return nil, err
	}

	n, err := strconv.Atoi(number)
	if err != nil {
		return nil, fmt.Errorf("task number %q: %w", number, err)
	}

	pages, err := t.b.Query(ctx, ds.ID, Query{Filters: []Filter{t.filter(t.props.ID, OpEquals, n)}, PageSize: 1}, false)
	if err != nil {
		return nil, err
	}

	if len(pages) == 0 {
		return nil, fmt.Errorf("%s not found", t.id(number))
	}

	t.remember(pages)

	return &pages[0], nil
}

// dataSource resolves and checks the data source once per run: the
// configured one, else the database's only one, its schema holding every
// configured property with a usable type.
func (t *Tracker) dataSource(ctx context.Context) (*DataSource, error) {
	if t.ds != nil {
		return t.ds, nil
	}

	id := t.n.DataSource
	if id == "" {
		db, err := t.b.Database(ctx, t.n.Database)
		if err != nil {
			return nil, err
		}

		switch len(db.DataSources) {
		case 0:
			return nil, fmt.Errorf("database %s has no data source", t.n.Database)
		case 1:
			id = db.DataSources[0].ID
		default:
			names := make([]string, len(db.DataSources))
			for i, d := range db.DataSources {
				names[i] = fmt.Sprintf("%s (%s)", d.ID, d.Name)
			}

			return nil, fmt.Errorf("database %s has %d data sources; set notion.data_source to one of: %s",
				t.n.Database, len(names), strings.Join(names, ", "))
		}
	}

	ds, err := t.b.DataSource(ctx, id)
	if err != nil {
		return nil, err
	}

	props := t.n.Properties
	if props.Title == "" {
		for name, s := range ds.Properties {
			if s.Type == "title" {
				props.Title = name
			}
		}
	}

	fixed := map[string]string{
		"id": props.ID, "title": props.Title, "status": props.Status, "priority": props.Priority,
		"assignee": props.Assignee, "labels": props.Labels, "parent": props.Parent, "after": props.After,
	}

	for key, name := range fixed {
		if name == "" {
			continue
		}

		s, ok := ds.Properties[name]
		if !ok {
			return nil, fmt.Errorf("notion.properties.%s: the data source has no property %q", key, name)
		}

		if !slices.Contains(wantTypes[key], s.Type) {
			return nil, fmt.Errorf("notion.properties.%s: %q is a %s property, want %s",
				key, name, s.Type, strings.Join(wantTypes[key], " or "))
		}
	}

	for key, name := range props.Extra {
		// Two keys on one property would let one update flag silently overwrite the other.
		for fkey, fname := range fixed {
			if fname == name {
				return nil, fmt.Errorf(
					"notion.properties.%s: %q is already mapped by notion.properties.%s",
					key,
					name,
					fkey,
				)
			}
		}

		s, ok := ds.Properties[name]
		if !ok {
			return nil, fmt.Errorf("notion.properties.%s: the data source has no property %q", key, name)
		}

		if !slices.Contains(settableTypes, s.Type) {
			return nil, fmt.Errorf("notion.properties.%s: %q is a %s property; --set writes %s",
				key, name, s.Type, strings.Join(settableTypes, ", "))
		}
	}

	if err := t.checkPrefix(ds.Properties[props.ID]); err != nil {
		return nil, err
	}

	if props.Status != "" {
		if err := t.exactDone(props.Status, ds.Properties[props.Status]); err != nil {
			return nil, err
		}
	}

	t.ds, t.props = ds, props

	return ds, nil
}

// checkPrefix fails when Notion prefixes the Unique ID property with
// something other than the configured prefix: ids would match nothing a
// person sees in Notion.
func (t *Tracker) checkPrefix(s PropertySchema) error {
	if p := s.UniqueID.Prefix; p != nil && *p != "" && *p != t.cfg.Prefix {
		return fmt.Errorf("prefix is %q, but Notion's %q property is prefixed %q", t.cfg.Prefix, t.n.Properties.ID, *p)
	}

	return nil
}

// exactDone rewrites each done status to the option's exact name, so the
// list filter, which Notion matches exactly, agrees with IsDone.
func (t *Tracker) exactDone(name string, s PropertySchema) error {
	names := options(s)

	for i, d := range t.n.Done {
		j := indexFold(names, d)
		if j < 0 {
			return fmt.Errorf("notion.done: %q is not an option of %q (%s)", d, name, strings.Join(names, ", "))
		}

		t.n.Done[i] = names[j]
	}

	return nil
}

// options lists the option names of a status, select or multi-select
// property.
func options(s PropertySchema) []string {
	var opts []Option

	switch s.Type {
	case "status":
		opts = s.Status.Options
	case "select":
		opts = s.Select.Options
	case "multi_select":
		opts = s.MultiSelect.Options
	}

	return optionList(opts)
}

func optionList(opts []Option) []string {
	names := make([]string, len(opts))
	for i, o := range opts {
		names[i] = o.Name
	}

	return names
}

// indexFold finds v among names, case-insensitively, or returns -1.
func indexFold(names []string, v string) int {
	return slices.IndexFunc(names, func(n string) bool { return strings.EqualFold(n, v) })
}

// filter builds a filter on a property, typed as the schema says it is.
func (t *Tracker) filter(name string, op Op, value any) Filter {
	return Filter{Property: name, Type: t.ds.Properties[name].Type, Op: op, Value: value}
}

func (t *Tracker) remember(pages []Page) {
	for i := range pages {
		t.ids[normalize(pages[i].ID)] = t.taskID(&pages[i])
	}
}

// fetchers bounds concurrent page reads, under Notion's average of three
// requests a second.
const fetchers = 3

// prefetchAfter reads the blockers the pages name and this run has not seen,
// a few at a time: list filters out done tasks, so most blockers are missing.
func (t *Tracker) prefetchAfter(ctx context.Context, pages []Page) error {
	if t.props.After == "" {
		return nil
	}

	var missing []string

	for i := range pages {
		for _, r := range pages[i].Properties[t.props.After].Relation {
			if _, ok := t.ids[normalize(r.ID)]; !ok && !slices.Contains(missing, r.ID) {
				missing = append(missing, r.ID)
			}
		}
	}

	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		first error
		next  = make(chan string)
	)

	for range min(fetchers, len(missing)) {
		wg.Go(func() {
			for id := range next {
				p, err := t.b.Page(ctx, id)

				mu.Lock()
				switch {
				case isNotFound(err):
					t.ids[normalize(id)] = ""
				case err != nil:
					first = cmp.Or(first, err)
				default:
					t.ids[normalize(id)] = t.taskID(p)
				}
				mu.Unlock()
			}
		})
	}

	for _, id := range missing {
		next <- id
	}

	close(next)
	wg.Wait()

	return first
}

// idOf maps a related page to its task id, fetching it when this run has
// not seen it yet.
func (t *Tracker) idOf(ctx context.Context, pageID string) (string, error) {
	if id, ok := t.ids[normalize(pageID)]; ok {
		return id, nil
	}

	p, err := t.b.Page(ctx, pageID)
	if isNotFound(err) {
		t.ids[normalize(pageID)] = ""

		return "", nil // a blocker the integration cannot see is no blocker f10 can act on
	}

	if err != nil {
		return "", err
	}

	t.remember([]Page{*p})

	return t.ids[normalize(pageID)], nil
}

func (t *Tracker) taskID(p *Page) string {
	u := p.Properties[t.props.ID].UniqueID
	if u == nil || u.Number == nil || (u.Prefix != nil && *u.Prefix != "" && *u.Prefix != t.cfg.Prefix) {
		return "" // not a task of this database
	}

	return t.id(strconv.Itoa(*u.Number))
}

func (t *Tracker) id(number string) string { return t.cfg.Prefix + "-" + number }

func (t *Tracker) title(p *Page) string { return plain(p.Properties[t.props.Title].Title) }

func (t *Tracker) labels(p *Page) []string {
	prop, ok := p.Properties[t.props.Labels]
	if t.props.Labels == "" || !ok {
		return nil
	}

	if prop.Select != nil {
		return []string{prop.Select.Name}
	}

	return optionList(prop.MultiSelect)
}

func optionName(p Property) string {
	switch {
	case p.Status != nil:
		return p.Status.Name
	case p.Select != nil:
		return p.Select.Name
	default:
		return ""
	}
}

// display renders a --set property's value for read.
func display(p Property) string {
	switch {
	case p.URL != nil:
		return *p.URL
	case p.Email != nil:
		return *p.Email
	case p.PhoneNumber != nil:
		return *p.PhoneNumber
	case p.Number != nil:
		return strconv.FormatFloat(*p.Number, 'f', -1, 64)
	case p.Checkbox != nil:
		return map[bool]string{true: "Yes", false: "No"}[*p.Checkbox]
	case p.Date != nil:
		if p.Date.End != nil && *p.Date.End != "" {
			return p.Date.Start + " -> " + *p.Date.End
		}

		return p.Date.Start
	case len(p.RichText) > 0:
		return plain(p.RichText)
	case len(p.MultiSelect) > 0:
		return strings.Join(optionList(p.MultiSelect), ", ")
	default:
		return optionName(p)
	}
}

func plain(rt []RichText) string {
	var b strings.Builder
	for _, r := range rt {
		b.WriteString(r.PlainText)
	}

	return b.String()
}

func author(c Comment) string {
	if n := c.DisplayName.ResolvedName; n != nil && *n != "" {
		return *n
	}

	return "unknown"
}

func stamp(rfc3339 string) string {
	ts, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}

	return ts.UTC().Format("2006-01-02 15:04 UTC")
}

func isNotFound(err error) bool {
	var apiErr *APIError

	return errors.As(err, &apiErr) && apiErr.Code == "object_not_found"
}

func normalize(id string) string { return strings.ReplaceAll(id, "-", "") }
