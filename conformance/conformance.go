// Package conformance checks a driver against f10's driver contract through
// its process boundary: the verb and argument in, stdout and the exit code
// out. It knows the contract, not any tracker, so every tracker in this
// module runs the same checks against its own fixtures.
package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amberpixels/f10-drivers/contract"
)

// Result is one driver run.
type Result struct {
	Stdout, Stderr string
	Code           int
}

// Runner runs the driver with stdin, a verb and its arguments.
type Runner func(t *testing.T, stdin string, args ...string) Result

// Command runs an executable from dir, as f10 runs .f10/driver from the
// checkout root, with env added to the test's environment.
func Command(path, dir string, env ...string) Runner {
	return func(t *testing.T, stdin string, args ...string) Result {
		t.Helper()

		cmd := exec.CommandContext(t.Context(), path, args...)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(cmd.Environ(), env...)

		var stdout, stderr bytes.Buffer

		cmd.Stdout, cmd.Stderr = &stdout, &stderr

		err := cmd.Run()

		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("running %s: %v", path, err)
		}

		return Result{Stdout: stdout.String(), Stderr: stderr.String(), Code: cmd.ProcessState.ExitCode()}
	}
}

// Fixture names what the tracker under test holds.
type Fixture struct {
	Task     string   // the number of a task that exists
	Parent   string   // the number of a task with subtasks; Task when empty
	Missing  string   // a number no task has
	Query    string   // a search query matching at least one task
	Declined []string // verbs this configuration declines with exit 3
	// Implements lists verbs that must answer, for those a driver may also
	// decline: the write verbs, which pass with exit 3 unless listed here.
	Implements []string
	// Update is the flags update sends to Task, and UpdateWant the text read
	// must show after it; a driver that implements update needs both.
	Update, UpdateWant []string
}

// writeVerbs may be declined by any driver: f10 falls back without them.
var writeVerbs = []string{contract.VerbCreate, contract.VerbComment, contract.VerbUpdate}

var (
	idShape = regexp.MustCompile(`^[A-Z][A-Z0-9]*-\d+$`)
	numeral = regexp.MustCompile(`^\d+$`)
)

// Check runs every verb of the contract and checks each answer's shape.
// Unless the fixture declines them, that includes the write verbs, which
// create a task, comment on Task and update it in whatever tracker run
// reaches: point it at a fake or a scratch database.
func Check(t *testing.T, run Runner, fx Fixture) {
	t.Helper()

	for _, verb := range contract.Verbs {
		t.Run(verb, func(t *testing.T) {
			arg := fx.Task

			switch verb {
			case contract.VerbSearch:
				arg = fx.Query
			case contract.VerbChildren:
				if fx.Parent != "" {
					arg = fx.Parent
				}
			case contract.VerbIndex, contract.VerbList, contract.VerbCreate:
				arg = ""
			}

			args := []string{verb}
			if arg != "" {
				args = append(args, arg)
			}

			if verb == contract.VerbUpdate {
				args = append(args, fx.Update...)
			}

			stdin := map[string]string{contract.VerbCreate: newTask, contract.VerbComment: newComment}[verb]

			res := run(t, stdin, args...)

			// A declined write verb always has a fallback, so it passes unlisted.
			declined := slices.Contains(writeVerbs, verb) && res.Code == contract.ExitUnsupported &&
				!slices.Contains(fx.Implements, verb)
			if slices.Contains(fx.Declined, verb) || declined {
				if res.Code != contract.ExitUnsupported || res.Stdout != "" {
					t.Fatalf("%s: want exit %d and no stdout for a declined verb, got exit %d, stdout %q",
						verb, contract.ExitUnsupported, res.Code, res.Stdout)
				}

				return
			}

			if verb == contract.VerbUpdate && (len(fx.Update) == 0 || len(fx.UpdateWant) == 0) {
				t.Fatalf("the driver answers update (exit %d), but the fixture sets no Update and UpdateWant "+
					"to check it with; set them, or list update in Declined", res.Code)
			}

			if res.Code != 0 {
				t.Fatalf("%s %s: exit %d, stderr: %s", verb, arg, res.Code, res.Stderr)
			}

			switch verb {
			case contract.VerbRead:
				checkMarkdown(t, res.Stdout)
			case contract.VerbURL, contract.VerbIndex:
				checkURL(t, oneLine(t, res.Stdout))
			case contract.VerbBranch:
				checkBranch(t, oneLine(t, res.Stdout))
			case contract.VerbSearch, contract.VerbList:
				checkRows(t, res.Stdout, verb == contract.VerbSearch)
			case contract.VerbChildren:
				checkChildren(t, res.Stdout)
			case contract.VerbCreate:
				checkCreated(t, run, oneLine(t, res.Stdout))
			case contract.VerbComment:
				oneLine(t, res.Stdout)
				checkRead(t, run, fx.Task, []string{commentText})
			case contract.VerbUpdate:
				if id := oneLine(t, res.Stdout); !idShape.MatchString(id) && !numeral.MatchString(id) {
					t.Fatalf("update printed %q, want the task id or number", id)
				}

				checkRead(t, run, fx.Task, fx.UpdateWant)
			}
		})
	}

	t.Run("unknown verb declines", func(t *testing.T) {
		if res := run(t, "", "frobnicate"); res.Code != contract.ExitUnsupported {
			t.Fatalf("want exit %d for a verb outside the contract, got %d", contract.ExitUnsupported, res.Code)
		}
	})

	t.Run("missing task fails", func(t *testing.T) {
		res := run(t, "", contract.VerbRead, fx.Missing)
		if res.Code == 0 || res.Code == contract.ExitUnsupported || strings.TrimSpace(res.Stderr) == "" {
			t.Fatalf("want a failure with stderr for a missing task, got exit %d, stderr %q", res.Code, res.Stderr)
		}
	})
}

// The task Check creates; checkCreated reads its title and body back.
var (
	newTitle = fmt.Sprintf("Conformance check %d", time.Now().UnixNano())
	newBody  = "Created by conformance.Check.\n\n- one\n- two"
	newTask  = "# " + newTitle + "\n\n" + newBody + "\n"
)

// The comment Check posts, and the plain text read must show for it.
var (
	commentText = fmt.Sprintf("Conformance comment %d, with a link and bold text.", time.Now().UnixNano())
	newComment  = strings.Replace(strings.Replace(commentText, "a link", "[a link](https://example.com/c)", 1),
		"bold", "**bold**", 1) + "\n"
)

// checkRead checks read of number shows each of want.
func checkRead(t *testing.T, run Runner, number string, want []string) {
	t.Helper()

	res := run(t, "", contract.VerbRead, number)
	if res.Code != 0 {
		t.Fatalf("read %s: exit %d, stderr: %s", number, res.Code, res.Stderr)
	}

	for _, w := range want {
		if !strings.Contains(res.Stdout, w) {
			t.Errorf("read %s lacks %q:\n%s", number, w, res.Stdout)
		}
	}
}

// checkCreated checks create printed a task id and read finds that task
// with the title and body it was created with.
func checkCreated(t *testing.T, run Runner, id string) {
	t.Helper()

	if !idShape.MatchString(id) && !numeral.MatchString(id) {
		t.Fatalf("create printed %q, want a task id or number", id)
	}

	checkRead(t, run, id[strings.LastIndex(id, "-")+1:], []string{newTitle, newBody})
}

func oneLine(t *testing.T, out string) string {
	t.Helper()

	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 || strings.TrimSpace(out) == "" {
		t.Fatalf("want one line alone, got %q", out)
	}

	return strings.TrimSuffix(out, "\n")
}

func checkURL(t *testing.T, s string) {
	t.Helper()

	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		t.Fatalf("want an absolute http(s) url, got %q", s)
	}
}

func checkBranch(t *testing.T, s string) {
	t.Helper()

	bad := s == "" || strings.ContainsAny(s, " ~^:?*[\\") || strings.HasPrefix(s, "-") ||
		strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") ||
		strings.HasSuffix(s, ".lock") || strings.Contains(s, "..") || strings.Contains(s, "//") ||
		strings.Contains(s, "@{") || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
	if bad {
		t.Fatalf("not a usable branch name (git check-ref-format): %q", s)
	}
}

func checkMarkdown(t *testing.T, s string) {
	t.Helper()

	if strings.TrimSpace(s) == "" {
		t.Fatal("read printed nothing")
	}

	if strings.Contains(s, "\x1b[") {
		t.Fatal("read printed terminal escapes; the contract wants plain markdown")
	}

	if strings.Contains(s, "data:image/") {
		t.Fatal("read inlined image data; the contract wants images as links")
	}
}

func checkRows(t *testing.T, out string, wantAny bool) {
	t.Helper()

	var rows []contract.Row
	decodeArray(t, out, &rows)

	if wantAny && len(rows) == 0 {
		t.Fatal("search matched nothing; the fixture's query should match a task")
	}

	for _, r := range rows {
		if !idShape.MatchString(r.ID) || r.Title == "" || r.URL == "" {
			t.Errorf("row lacks a required field or has a malformed id: %+v", r)
		}

		if r.Priority != "" && !contract.IsPriority(r.Priority) {
			t.Errorf("%s: priority %q is outside %v", r.ID, r.Priority, contract.Priorities)
		}

		if r.Created != "" {
			if _, err := time.Parse(time.RFC3339, r.Created); err != nil {
				t.Errorf("%s: created %q is not RFC 3339", r.ID, r.Created)
			}
		}

		if r.After != "" && !idShape.MatchString(r.After) {
			t.Errorf("%s: after %q is not a task id", r.ID, r.After)
		}
	}
}

func checkChildren(t *testing.T, out string) {
	t.Helper()

	var rows []contract.ChildRow
	decodeArray(t, out, &rows)

	for _, r := range rows {
		if !idShape.MatchString(r.ID) || r.Title == "" || r.URL == "" {
			t.Errorf("child lacks a required field or has a malformed id: %+v", r)
		}

		if r.State != "open" && r.State != "closed" {
			t.Errorf("%s: state %q is neither open nor closed", r.ID, r.State)
		}
	}
}

// decodeArray decodes a JSON array, refusing `null` and anything after it.
// Extra fields are allowed, as the contract allows them.
func decodeArray(t *testing.T, out string, into any) {
	t.Helper()

	if !strings.HasPrefix(strings.TrimSpace(out), "[") {
		t.Fatalf("want a JSON array, got %q", out)
	}

	dec := json.NewDecoder(strings.NewReader(out))

	if err := dec.Decode(into); err != nil {
		t.Fatalf("decoding rows: %v\n%s", err, out)
	}

	if dec.More() {
		t.Fatalf("more than one JSON value on stdout:\n%s", out)
	}
}
