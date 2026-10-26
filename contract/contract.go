// Package contract is f10's driver contract (docs/driver-contract.md in
// amberpixels/f10) as Go: the verbs, the row shapes, the exit codes, and
// Serve, which answers one verb from a Tracker.
//
// Drivers return data and never act: a url is printed, never opened; a
// branch name is printed, never created.
package contract

import (
	"context"
	"encoding/json"
	"errors"
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
)

// Verbs lists every verb, in the contract's order.
var Verbs = []string{VerbRead, VerbURL, VerbSearch, VerbBranch, VerbChildren, VerbIndex, VerbList}

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

// Serve answers one verb from t on w. A verb outside the contract returns
// ErrUnsupported, so a verb added to f10 later declines correctly.
func Serve(ctx context.Context, t Tracker, verb, arg string, w io.Writer) error {
	needsNumber := verb == VerbRead || verb == VerbURL || verb == VerbBranch || verb == VerbChildren
	if needsNumber && !isNumber(arg) {
		return fmt.Errorf("%s needs a task number, got %q", verb, arg)
	}

	switch verb {
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
