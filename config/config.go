// Package config reads a project's driver.toml: the facts that differ
// between projects on the same tracker, so no project writes driver code.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/amberpixels/f10-drivers/contract"
)

// DefaultPath is where the config lives, relative to the checkout root f10
// runs the driver from.
const DefaultPath = ".f10/driver.toml"

// DefaultBranch is f10's own default branch shape.
const DefaultBranch = "{id}/{slug}"

// Config is one project's driver.toml.
type Config struct {
	Prefix string  `toml:"prefix"` // the task id prefix: "MKT" makes "MKT-12"
	Branch string  `toml:"branch"` // a template over {id}, {number} and {slug}; DefaultBranch when empty
	Notion *Notion `toml:"notion"`

	Path string `toml:"-"` // the file Load read, for error messages
}

// The ways into Notion notion.transport names.
const (
	TransportAPI = "api" // the REST API, with an integration token
	TransportMCP = "mcp" // Notion's hosted MCP server, as the logged-in user
)

// Notion is the [notion] table.
type Notion struct {
	Transport  string                `toml:"transport"`   // TransportAPI by default
	TokenEnv   string                `toml:"token_env"`   // the env var holding the token, NOTION_TOKEN by default; api only
	Database   string                `toml:"database"`    // a database id or url
	DataSource string                `toml:"data_source"` // set when the database has several
	Done       []string              `toml:"done"`        // statuses that close a task, ["Done"] by default
	Properties Properties            `toml:"properties"`
	Priority   map[string]StringList `toml:"priority"` // contract level to the tracker's values
}

// Properties names the Notion properties each row field reads from.
// ID is required; an empty Title is found in the schema; any other empty one
// is a field the database does not have.
type Properties struct {
	ID       string `toml:"id"`
	Title    string `toml:"title"`
	Status   string `toml:"status"`
	Priority string `toml:"priority"`
	Assignee string `toml:"assignee"`
	Labels   string `toml:"labels"`
	Parent   string `toml:"parent"`
	After    string `toml:"after"`

	// Extra is every other key: a property create and update --set write, by its key.
	Extra map[string]string `toml:"-"`
}

// UnmarshalTOML implements toml.Unmarshaler: the known keys fill their
// fields, any other key goes to Extra.
func (p *Properties) UnmarshalTOML(v any) error {
	table, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("notion.properties: want a table, got %v", v)
	}

	known := map[string]*string{
		"id": &p.ID, "title": &p.Title, "status": &p.Status, "priority": &p.Priority,
		"assignee": &p.Assignee, "labels": &p.Labels, "parent": &p.Parent, "after": &p.After,
	}

	for key, raw := range table {
		name, ok := raw.(string)
		if !ok {
			return fmt.Errorf("notion.properties.%s: want a property name, got %v", key, raw)
		}

		if field, ok := known[key]; ok {
			*field = name

			continue
		}

		for k := range known {
			if nearMiss(key, k) {
				return fmt.Errorf("notion.properties.%s: did you mean %s?", key, k)
			}
		}

		if !extraKey.MatchString(key) {
			return fmt.Errorf(
				"notion.properties.%s: a key --set names takes lowercase letters, digits and _",
				key,
			)
		}

		if p.Extra == nil {
			p.Extra = map[string]string{}
		}

		p.Extra[key] = name
	}

	return nil
}

// nearMiss reports whether a differs from b by one edit or one swap of
// neighbouring letters: a misspelled known key, not a property of its own.
func nearMiss(a, b string) bool {
	if a == b {
		return false
	}

	switch len(a) - len(b) {
	case 0:
		var diff []int

		for i := range len(a) {
			if a[i] != b[i] {
				diff = append(diff, i)
			}
		}

		return len(diff) == 1 ||
			(len(diff) == 2 && diff[1] == diff[0]+1 && a[diff[0]] == b[diff[1]] && a[diff[1]] == b[diff[0]])
	case 1, -1:
		long, short := a, b
		if len(b) > len(a) {
			long, short = b, a
		}

		for i := range len(long) {
			if long[:i]+long[i+1:] == short {
				return true
			}
		}
	}

	return false
}

// extraKey is the shape of a key --set names on a command line.
var extraKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// StringList takes one string or an array of them.
type StringList []string

// UnmarshalTOML implements toml.Unmarshaler.
func (l *StringList) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case string:
		*l = StringList{v}
	case []any:
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("want a string, got %v", e)
			}

			*l = append(*l, s)
		}
	default:
		return fmt.Errorf("want a string or an array of strings, got %v", v)
	}

	return nil
}

// Load reads and validates the config at path, DefaultPath when empty.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath
	}

	var cfg Config

	md, err := toml.DecodeFile(path, &cfg)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no driver config at %s", path)
	}

	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}

		return nil, fmt.Errorf("%s: unknown key %s", path, strings.Join(keys, ", "))
	}

	cfg.Path = path
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return &cfg, nil
}

// Token reads the integration token from the env var the config names.
func (n *Notion) Token() (string, error) {
	tok := os.Getenv(n.TokenEnv)
	if tok == "" {
		return "", fmt.Errorf("%s is not set (notion.token_env names it)", n.TokenEnv)
	}

	return tok, nil
}

// PriorityOf maps a tracker value to its contract level, case-insensitively,
// or "" when the value maps to none. The table wins, the more urgent level
// for a value listed twice; a value named like a level maps to that level.
func (n *Notion) PriorityOf(value string) string {
	for _, level := range contract.Priorities {
		if slices.ContainsFunc(n.Priority[level], func(v string) bool { return strings.EqualFold(v, value) }) {
			return level
		}
	}

	if level := strings.ToLower(strings.TrimSpace(value)); contract.IsPriority(level) {
		return level
	}

	return ""
}

// IsDone reports whether a status closes a task, matched exactly as
// Notion's filters match it.
func (n *Notion) IsDone(status string) bool { return slices.Contains(n.Done, status) }

func (n *Notion) validate() error {
	switch n.Transport {
	case "":
		n.Transport = TransportAPI
	case TransportAPI, TransportMCP:
	default:
		return fmt.Errorf("notion.transport: %q is neither %q nor %q", n.Transport, TransportAPI, TransportMCP)
	}

	if n.Transport == TransportMCP && n.TokenEnv != "" {
		return fmt.Errorf("notion.token_env applies only to transport = %q", TransportAPI)
	}

	if n.Transport == TransportAPI && n.TokenEnv == "" {
		n.TokenEnv = "NOTION_TOKEN"
	}

	if n.Done == nil {
		n.Done = []string{"Done"}
	}

	if n.Database == "" && n.DataSource == "" {
		return errors.New("notion.database or notion.data_source is required")
	}

	if n.Database != "" {
		id, ok := NotionID(n.Database)
		if !ok {
			return fmt.Errorf("notion.database: no Notion id in %q", n.Database)
		}

		n.Database = id
	}

	if n.DataSource != "" {
		id, ok := NotionID(n.DataSource)
		if !ok {
			return fmt.Errorf("notion.data_source: no Notion id in %q", n.DataSource)
		}

		n.DataSource = id
	}

	if n.Properties.ID == "" {
		return errors.New("notion.properties.id is required (the Unique ID property)")
	}

	for level := range n.Priority {
		if !contract.IsPriority(level) {
			return fmt.Errorf("notion.priority.%s: the levels are %s", level, strings.Join(contract.Priorities, ", "))
		}
	}

	return nil
}

var (
	dashedID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	plainID  = regexp.MustCompile(`[0-9a-fA-F]{32}`)
)

// NotionID finds a Notion id in an id or a url and returns it undashed and
// lowercase. In a url the id is the last 32-hex run of the path, which a
// title slug in front of it cannot match.
func NotionID(s string) (string, bool) {
	s, _, _ = strings.Cut(s, "?")

	if ids := dashedID.FindAllString(s, -1); len(ids) > 0 {
		return strings.ToLower(strings.ReplaceAll(ids[len(ids)-1], "-", "")), true
	}

	if ids := plainID.FindAllString(s, -1); len(ids) > 0 {
		return strings.ToLower(ids[len(ids)-1]), true
	}

	return "", false
}

// BranchName fills the branch template for a task. A title that slugs to
// nothing leaves no dangling separator, so `{id}/{slug}` gives the bare id.
func (c *Config) BranchName(id, number, title string) string {
	name := strings.NewReplacer("{id}", id, "{number}", number, "{slug}", Slug(title)).Replace(c.Branch)

	return strings.TrimRight(name, "/-._")
}

func (c *Config) validate() error {
	if c.Prefix == "" {
		return errors.New("prefix is required (the task id prefix, as in MKT-12)")
	}

	if c.Branch == "" {
		c.Branch = DefaultBranch
	}

	if c.Notion != nil {
		return c.Notion.validate()
	}

	return nil
}

const slugWords = 6

// Slug makes a branch-safe slug of a title: lowercase words joined by
// hyphens, at most a few of them.
func Slug(title string) string {
	words := strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})

	if len(words) > slugWords {
		words = words[:slugWords]
	}

	return strings.Join(words, "-")
}
