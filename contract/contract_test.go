package contract_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/amberpixels/f10-drivers/contract"
)

// stub answers every verb with nothing, the way an empty tracker would.
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

			err := contract.Serve(t.Context(), stub{}, tc.verb, tc.arg, &out)
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
		if err := contract.Serve(t.Context(), stub{}, "read", arg, &bytes.Buffer{}); err == nil {
			t.Errorf("read %q: want an error", arg)
		}
	}
}
