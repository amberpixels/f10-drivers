package contract_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/amberpixels/f10-drivers/contract"
)

// stub is a tracker with no tasks: fixed text and urls, empty rows, and
// branch declined.
type stub struct{}

func (stub) Read(context.Context, string) (string, error) { return "# body", nil }

func (stub) URL(context.Context, string) (string, error)                   { return "https://x.test/1", nil }
func (stub) Search(context.Context, string) ([]contract.Row, error)        { return nil, nil }
func (stub) Branch(context.Context, string) (string, error)                { return "", contract.ErrUnsupported }
func (stub) Children(context.Context, string) ([]contract.ChildRow, error) { return nil, nil }
func (stub) Index(context.Context) (string, error)                         { return "https://x.test", nil }

func (stub) List(context.Context) ([]contract.Row, error) { return []contract.Row{}, nil }

func TestServe(t *testing.T) {
	for _, tc := range []struct {
		verb, arg, want string
		err             error
	}{
		{verb: "read", arg: "1", want: "# body\n"},
		{verb: "url", arg: "1", want: "https://x.test/1\n"},
		{verb: "search", arg: "q", want: "[]\n"},
		{verb: "children", arg: "1", want: "[]\n"},
		{verb: "list", want: "[]\n"},
		{verb: "index", want: "https://x.test\n"},
		{verb: "branch", arg: "1", err: contract.ErrUnsupported},
		{verb: "create", arg: "1", err: contract.ErrUnsupported},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			var out bytes.Buffer

			err := contract.Serve(t.Context(), stub{}, tc.verb, []string{tc.arg}, strings.NewReader(""), &out)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}

			if out.String() != tc.want {
				t.Fatalf("stdout = %q, want %q", out.String(), tc.want)
			}
		})
	}
}

func TestServeRejectsNonNumbers(t *testing.T) {
	for _, arg := range []string{"", "MKT-12", "12a"} {
		if err := contract.Serve(t.Context(), stub{}, "read", []string{arg}, nil, &bytes.Buffer{}); err == nil {
			t.Errorf("read %q: want an error", arg)
		}
	}
}

// creator is a stub that implements create and keeps what it was given.
type creator struct {
	stub

	got contract.NewTask
}

func (c *creator) Create(_ context.Context, task contract.NewTask) (string, error) {
	c.got = task

	return "MKT-7", nil
}

func TestServeCreate(t *testing.T) {
	c := &creator{}

	var out bytes.Buffer

	args := []string{
		"--status",
		"In progress",
		"--priority",
		"high",
		"--label",
		"a",
		"--label",
		"b",
		"--parent",
		"2",
		"--after",
		"3",
	}

	err := contract.Serve(
		t.Context(),
		c,
		"create",
		args,
		strings.NewReader("\n# A title \n\nThe body.\n\nAfter: MKT-3\n\n"),
		&out,
	)
	if err != nil || out.String() != "MKT-7\n" {
		t.Fatalf("err = %v, stdout = %q", err, out.String())
	}

	want := contract.NewTask{
		Title: "A title", Body: "The body.\n\nAfter: MKT-3", Status: "In progress", Priority: "high",
		Labels: []string{"a", "b"}, Parent: "2", After: "3",
	}
	if c.got.Title != want.Title || c.got.Body != want.Body || c.got.Status != want.Status ||
		c.got.Priority != want.Priority || strings.Join(c.got.Labels, ",") != "a,b" ||
		c.got.Parent != want.Parent || c.got.After != want.After {
		t.Fatalf("task = %+v, want %+v", c.got, want)
	}
}

func TestServeCreateKeepsTheBodysIndent(t *testing.T) {
	c := &creator{}

	err := contract.Serve(
		t.Context(),
		c,
		"create",
		nil,
		strings.NewReader("# T\n\n  \n    code\n\ntext\n"),
		&bytes.Buffer{},
	)
	if err != nil || c.got.Body != "    code\n\ntext" {
		t.Fatalf("err = %v, body = %q", err, c.got.Body)
	}
}

func TestServeCreateDropsUnknownFlags(t *testing.T) {
	c := &creator{}

	args := []string{"--assignee", "ann", "--status", "Done", "--type=bug", "--urgent", "--label", "a"}

	err := contract.Serve(t.Context(), c, "create", args, strings.NewReader("# T"), &bytes.Buffer{})
	if err != nil || strings.Join(c.got.Dropped, ",") != "assignee,type,urgent" ||
		c.got.Status != "Done" || strings.Join(c.got.Labels, ",") != "a" {
		t.Fatalf("err = %v, task = %+v", err, c.got)
	}
}

func TestServeCreateRejects(t *testing.T) {
	for _, tc := range []struct {
		name, stdin, want string
		args              []string
	}{
		{"no title line", "Just a body", "the first line is \"Just a body\"", nil},
		{"empty title", "#  \nbody", "the first line", nil},
		{"empty stdin", "", "the first line is \"\"", nil},
		{"bad priority", "# T", `--priority "p1" is not one of`, []string{"--priority", "p1"}},
		{"parent id", "# T", `--parent needs a task number, got "MKT-2"`, []string{"--parent", "MKT-2"}},
		{"positional", "# T", "takes no arguments", []string{"2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &creator{}

			err := contract.Serve(t.Context(), c, "create", tc.args, strings.NewReader(tc.stdin), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}

			if c.got.Title != "" {
				t.Fatal("Create was called")
			}
		})
	}
}
