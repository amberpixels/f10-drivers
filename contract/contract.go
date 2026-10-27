// Package contract is f10's driver contract (docs/driver-contract.md in
// amberpixels/f10) as Go: the verbs, the row shapes, the exit codes, and
// Serve, which answers one verb from a Tracker.
//
// Read verbs change nothing; write verbs change only the tracker. Neither
// acts on the local machine: a url is printed, never opened; a branch name
// is printed, never created.
package contract

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Verbs f10 sends.
const (
	VerbRead     = "read"     // stdout: the task as markdown
	VerbURL      = "url"      // stdout: one url
	VerbSearch   = "search"   // stdout: JSON rows, one task each
	VerbBranch   = "branch"   // stdout: one branch name
	VerbChildren = "children" // stdout: JSON rows, one subtask each
	VerbIndex    = "index"    // stdout: one url, the task list; no argument
	VerbList     = "list"     // stdout: JSON rows, the first page of open tasks; no argument
	VerbCreate   = "create"   // stdin: the task as markdown; flags: its properties; stdout: the new id
	VerbComment  = "comment"  // stdin: the comment as markdown; stdout: the new comment's id
	VerbUpdate   = "update"   // flags: the properties to set; stdout: the task's id
)

// Verbs lists every verb, in the contract's order: the read verbs, then the
// write verbs.
var Verbs = []string{
	VerbRead, VerbURL, VerbSearch, VerbBranch, VerbChildren, VerbIndex, VerbList,
	VerbCreate, VerbComment, VerbUpdate,
}

// Priorities is the contract's priority vocabulary, most urgent first.
var Priorities = []string{"urgent", "high", "medium", "low"}

// IsPriority reports whether v is one of Priorities, exactly.
func IsPriority(v string) bool { return slices.Contains(Priorities, v) }

// ExitUnsupported is the reserved exit code for a verb the tracker does not
// implement. It is a normal answer, not a failure.
const ExitUnsupported = 3

// ErrUnsupported is what a Tracker returns for a verb it does not implement.
var ErrUnsupported = errors.New("verb not implemented by this tracker")

// Row is one task in a search or list answer.
type Row struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	URL      string `json:"url"`
	Priority string `json:"priority,omitempty"`
	Assignee string `json:"assignee,omitempty"`
	Created  string `json:"created,omitempty"` // RFC 3339
	After    string `json:"after,omitempty"`   // the id this task waits for
}

// ChildRow is one subtask in a children answer. State is "open" or "closed";
// f10 drops closed rows itself, so a tracker returns every subtask.
type ChildRow struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	URL    string   `json:"url"`
	State  string   `json:"state"`
	Labels []string `json:"labels,omitempty"`
	After  string   `json:"after,omitempty"`
}

// Tracker answers the contract's verbs. The number is the bare task number:
// f10 has already parsed "ABC-1042" down to "1042".
type Tracker interface {
	Read(ctx context.Context, number string) (string, error)
	URL(ctx context.Context, number string) (string, error)
	Search(ctx context.Context, query string) ([]Row, error)
	Branch(ctx context.Context, number string) (string, error)
	Children(ctx context.Context, number string) ([]ChildRow, error)
	Index(ctx context.Context) (string, error)
	List(ctx context.Context) ([]Row, error)
}

// NewTask is a task for create: the title and body from stdin, the
// properties from flags, each empty when the caller did not set it.
type NewTask struct {
	Title    string
	Body     string
	Status   string   // the tracker's own word, as given
	Priority string   // one of Priorities
	Labels   []string // in the order given
	Parent   string   // a task number
	After    string   // a task number
	Dropped  []string // flags create does not know, which the tracker warns about and drops
}

// Creator is a Tracker that implements create. A tracker without it
// declines the verb.
type Creator interface {
	Create(ctx context.Context, task NewTask) (string, error)
}

// TaskUpdate is the properties update sets on a task, each empty when the
// caller did not set it.
type TaskUpdate struct {
	Status   string     // the tracker's own word, as given
	Priority string     // one of Priorities
	Labels   []string   // the full set, replacing the task's labels
	Assignee string     // an email or a name
	Set      []KeyValue // other properties, by the tracker's config key, in the order given
}

// KeyValue is one --set key=value.
type KeyValue struct {
	Key, Value string
}

// Empty reports whether the update sets nothing.
func (u TaskUpdate) Empty() bool {
	return u.Status == "" && u.Priority == "" && len(u.Labels) == 0 && u.Assignee == "" && len(u.Set) == 0
}

// Commenter is a Tracker that implements comment. A tracker without it
// declines the verb.
type Commenter interface {
	Comment(ctx context.Context, number, markdown string) (string, error)
}

// Updater is a Tracker that implements update. A tracker without it
// declines the verb.
type Updater interface {
	Update(ctx context.Context, number string, u TaskUpdate) (string, error)
}

// Serve answers one verb from t on w. args are the verb's arguments; the
// read verbs take the first, create parses them as flags and reads the task
// from stdin, comment reads its text from stdin, update parses the flags
// after the number. A verb outside the contract returns ErrUnsupported, so a verb
// added to f10 later declines correctly.
func Serve(ctx context.Context, t Tracker, verb string, args []string, stdin io.Reader, w io.Writer) error {
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}

	needsNumber := verb == VerbRead || verb == VerbURL || verb == VerbBranch || verb == VerbChildren ||
		verb == VerbComment || verb == VerbUpdate
	if needsNumber && !isNumber(arg) {
		return fmt.Errorf("%s needs a task number, got %q", verb, arg)
	}

	switch verb {
	case VerbCreate:
		c, ok := t.(Creator)
		if !ok {
			return ErrUnsupported
		}

		task, err := parseCreate(args, stdin)
		if err != nil {
			return err
		}

		return line(w)(c.Create(ctx, task))
	case VerbComment:
		c, ok := t.(Commenter)
		if !ok {
			return ErrUnsupported
		}

		md, err := parseComment(args[1:], stdin)
		if err != nil {
			return err
		}

		return line(w)(c.Comment(ctx, arg, md))
	case VerbUpdate:
		u, ok := t.(Updater)
		if !ok {
			return ErrUnsupported
		}

		upd, err := parseUpdate(args[1:])
		if err != nil {
			return err
		}

		return line(w)(u.Update(ctx, arg, upd))
	case VerbRead:
		md, err := t.Read(ctx, arg)
		if err != nil {
			return err
		}

		_, err = io.WriteString(w, strings.TrimRight(md, "\n")+"\n")

		return err
	case VerbURL:
		return line(w)(t.URL(ctx, arg))
	case VerbBranch:
		return line(w)(t.Branch(ctx, arg))
	case VerbIndex:
		return line(w)(t.Index(ctx))
	case VerbSearch:
		return rows[Row](w)(t.Search(ctx, arg))
	case VerbList:
		return rows[Row](w)(t.List(ctx))
	case VerbChildren:
		return rows[ChildRow](w)(t.Children(ctx, arg))
	default:
		return ErrUnsupported
	}
}

// parseCreate reads create's flags and the task on stdin.
func parseCreate(args []string, stdin io.Reader) (NewTask, error) {
	var (
		task   NewTask
		labels labelFlag
	)

	fs := flag.NewFlagSet(VerbCreate, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&task.Status, "status", "", "")
	fs.StringVar(&task.Priority, "priority", "", "")
	fs.Var(&labels, "label", "")
	fs.StringVar(&task.Parent, "parent", "", "")
	fs.StringVar(&task.After, "after", "", "")

	// A property f10 adds to create later degrades to a warning instead of
	// failing every driver built before it.
	args, task.Dropped = dropUnknown(fs, args)

	if err := fs.Parse(args); err != nil {
		return task, fmt.Errorf("create: %w", err)
	}

	if fs.NArg() > 0 {
		return task, fmt.Errorf("create takes no arguments, only flags; got %q", fs.Arg(0))
	}

	task.Labels = labels

	if task.Priority != "" && !IsPriority(task.Priority) {
		return task, fmt.Errorf("create: --priority %q is not one of %s", task.Priority, strings.Join(Priorities, ", "))
	}

	for name, v := range map[string]string{"parent": task.Parent, "after": task.After} {
		if v != "" && !isNumber(v) {
			return task, fmt.Errorf("create: --%s needs a task number, got %q", name, v)
		}
	}

	data, err := io.ReadAll(stdin)
	if err != nil {
		return task, fmt.Errorf("create: reading stdin: %w", err)
	}

	task.Title, task.Body, err = parseTask(string(data))

	return task, err
}

// parseComment reads comment's text from stdin; it takes no flags.
func parseComment(args []string, stdin io.Reader) (string, error) {
	if len(args) > 0 {
		return "", fmt.Errorf("comment takes only a task number; got %q", args[0])
	}

	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("comment: reading stdin: %w", err)
	}

	md := strings.TrimRight(string(data), " \t\r\n")
	if strings.TrimSpace(md) == "" {
		return "", errors.New("comment wants its text on stdin; got none")
	}

	return md, nil
}

// parseUpdate reads update's flags. Unlike create it fails on an unknown
// flag: an update whose one change was dropped did nothing.
func parseUpdate(args []string) (TaskUpdate, error) {
	var (
		u      TaskUpdate
		labels labelFlag
		sets   setFlag
	)

	fs := flag.NewFlagSet(VerbUpdate, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&u.Status, "status", "", "")
	fs.StringVar(&u.Priority, "priority", "", "")
	fs.Var(&labels, "label", "")
	fs.StringVar(&u.Assignee, "assignee", "", "")
	fs.Var(&sets, "set", "")

	if err := fs.Parse(args); err != nil {
		return u, fmt.Errorf("update: %w", err)
	}

	if fs.NArg() > 0 {
		return u, fmt.Errorf("update takes a task number, then only flags; got %q", fs.Arg(0))
	}

	u.Labels, u.Set = labels, sets

	if u.Priority != "" && !IsPriority(u.Priority) {
		return u, fmt.Errorf("update: --priority %q is not one of %s", u.Priority, strings.Join(Priorities, ", "))
	}

	if u.Empty() {
		return u, errors.New(
			"update sets nothing; give at least one of --status, --priority, --label, --assignee, --set",
		)
	}

	return u, nil
}

// dropUnknown removes the flags fs does not define, with their values, and
// returns the rest and the dropped flag names.
func dropUnknown(fs *flag.FlagSet, args []string) ([]string, []string) {
	var kept, dropped []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || !strings.HasPrefix(a, "-") {
			return append(kept, args[i:]...), dropped
		}

		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		known := fs.Lookup(name) != nil

		if known {
			kept = append(kept, a)
		} else {
			dropped = append(dropped, name)
		}

		// Every create flag takes a value; an unknown one takes the next
		// argument only when it does not look like a flag.
		if !hasValue && i+1 < len(args) && (known || !strings.HasPrefix(args[i+1], "-")) {
			i++

			if known {
				kept = append(kept, args[i])
			}
		}
	}

	return kept, dropped
}

// parseTask splits create's stdin into the `# <title>` line and the body
// after it.
func parseTask(md string) (string, string, error) {
	first, rest, _ := strings.Cut(strings.TrimLeft(md, " \t\r\n"), "\n")
	first = strings.TrimRight(first, "\r")

	title := strings.TrimSpace(strings.TrimPrefix(first, "# "))
	if !strings.HasPrefix(first, "# ") || title == "" {
		return "", "", fmt.Errorf(
			"create wants the task on stdin as `# <title>` then the body; the first line is %q", first)
	}

	// Only whole blank lines go: a body opening with an indented code block
	// keeps its indent.
	for {
		line, after, found := strings.Cut(rest, "\n")
		if !found || strings.TrimSpace(line) != "" {
			break
		}

		rest = after
	}

	return title, strings.TrimRight(rest, " \t\r\n"), nil
}

// labelFlag collects a repeated --label.
type labelFlag []string

func (l *labelFlag) String() string { return strings.Join(*l, ",") }

func (l *labelFlag) Set(v string) error {
	*l = append(*l, v)

	return nil
}

// setFlag collects a repeated --set key=value, each key once.
type setFlag []KeyValue

func (s *setFlag) String() string { return fmt.Sprint(*s) }

func (s *setFlag) Set(v string) error {
	key, value, ok := strings.Cut(v, "=")
	if !ok || key == "" {
		return fmt.Errorf("want key=value, got %q", v)
	}

	for _, kv := range *s {
		if kv.Key == key {
			return fmt.Errorf("%s is set twice", key)
		}
	}

	*s = append(*s, KeyValue{Key: key, Value: value})

	return nil
}

func line(w io.Writer) func(string, error) error {
	return func(s string, err error) error {
		if err != nil {
			return err
		}

		_, err = fmt.Fprintln(w, s)

		return err
	}
}

func rows[T any](w io.Writer) func([]T, error) error {
	return func(rs []T, err error) error {
		if err != nil {
			return err
		}

		if rs == nil {
			rs = []T{} // `[]`, never `null`
		}

		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)

		return enc.Encode(rs)
	}
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}

	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}
