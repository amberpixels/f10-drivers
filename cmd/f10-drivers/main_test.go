package main_test

import (
	"bufio"
	"flag"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/amberpixels/f10-drivers/conformance"
	"github.com/amberpixels/f10-drivers/notion/notiontest"
)

var update = flag.Bool("update", false, "rewrite golden files")

const notionConfig = `prefix = "MKT"
branch = "feat/{id}-{slug}"

[notion]
token_env = "MKT_NOTION_TOKEN"
database = "https://www.notion.so/workspace/Marketing-tasks-1a2b3c4d00004000800000000000db01?v=1"
done = ["Done"]

[notion.properties]
id = "ID"
status = "Status"
priority = "Priority"
assignee = "Assignee"
labels = "Tags"
result = "Result"
due = "Due"
feature = "Feature"
%s

[notion.priority]
urgent = "Urgent"
high = ["High", "P1"]
medium = "Medium"
low = "Low"
`

const relations = `parent = "Parent"
after = "Blocked by"`

// fixture is the fixture database for conformance.Check, every write verb
// answered.
var fixture = conformance.Fixture{
	Task: "2", Parent: "2", Missing: "99", Query: "summary",
	Implements: []string{"create", "comment", "update"},
	Update:     []string{"--status", "Done", "--set", "result=https://example.com/report", "--set", "due=2026-11-02"},
	UpdateWant: []string{"Status: Done\n", "Result: https://example.com/report\n", "Due: 2026-11-02\n"},
}

var binary string

func TestMain(m *testing.M) {
	flag.Parse()

	dir, err := os.MkdirTemp("", "f10-drivers")
	if err != nil {
		panic(err)
	}

	binary = filepath.Join(dir, "f10-drivers")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}

	code := m.Run()

	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// project lays out a checkout with the one-line shim and a driver.toml, and
// returns a runner that calls the shim the way f10 does; env overrides the
// defaults.
func project(t *testing.T, cfg string, env ...string) conformance.Runner {
	t.Helper()

	return projectOn(t, notiontest.New(t), cfg, env...)
}

func projectOn(t *testing.T, srv *notiontest.Server, cfg string, env ...string) conformance.Runner {
	t.Helper()

	run, _ := projectIn(t, srv, cfg, env...)

	return run
}

// projectIn is projectOn, also returning the checkout root.
func projectIn(t *testing.T, srv *notiontest.Server, cfg string, env ...string) (conformance.Runner, string) {
	t.Helper()

	root := t.TempDir()
	storage := filepath.Join(root, ".f10")

	if err := os.MkdirAll(storage, 0o755); err != nil {
		t.Fatal(err)
	}

	shim := "#!/bin/sh\nexec " + binary + " notion \"$@\"\n"
	write(t, filepath.Join(storage, "driver"), shim, 0o755)
	write(t, filepath.Join(storage, "driver.toml"), cfg, 0o644)

	env = append([]string{"MKT_NOTION_TOKEN=" + notiontest.Token, "F10_DRIVERS_NOTION_API=" + srv.URL},
		slices.DeleteFunc(env, func(e string) bool { return e == "" })...)

	return conformance.Command(filepath.Join(storage, "driver"), root, env...), root
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestNotionConformance(t *testing.T) {
	run := project(t, strings.Replace(notionConfig, "%s", relations, 1))
	conformance.Check(t, run, fixture)
}

func TestNotionWithoutRelationsDeclinesChildren(t *testing.T) {
	run := project(t, strings.Replace(notionConfig, "%s", "", 1))
	fx := fixture
	fx.Parent, fx.Declined = "", []string{"children"}
	conformance.Check(t, run, fx)
}

// TestNotionAnswers pins what each verb prints for the fixture database.
func TestNotionAnswers(t *testing.T) {
	run := project(t, strings.Replace(notionConfig, "%s", relations, 1))

	for _, tc := range []struct{ golden, verb, arg string }{
		{"read-2.md", "read", "2"},
		{"read-4.md", "read", "4"},
		{"list.json", "list", ""},
		{"search.json", "search", "summary"},
		{"children-2.json", "children", "2"},
		{"branch-3.txt", "branch", "3"},
		{"url-3.txt", "url", "3"},
		{"index.txt", "index", ""},
	} {
		t.Run(tc.golden, func(t *testing.T) {
			args := []string{tc.verb}
			if tc.arg != "" {
				args = append(args, tc.arg)
			}

			res := run(t, "", args...)
			if res.Code != 0 {
				t.Fatalf("exit %d: %s", res.Code, res.Stderr)
			}

			golden(t, tc.golden, res.Stdout)
		})
	}
}

func TestNotionWarnsOnTruncatedBody(t *testing.T) {
	run := project(t, strings.Replace(notionConfig, "%s", relations, 1))

	res := run(t, "", "read", "4")
	for _, want := range []string{"MKT-4 is too long", "1 block(s) Notion could not render"} {
		if !strings.Contains(res.Stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, res.Stderr)
		}
	}
}

func TestConfigErrors(t *testing.T) {
	for _, tc := range []struct{ name, cfg, env, want string }{
		{"unknown key", strings.Replace(notionConfig, "[notion]", "colour = \"red\"\n\n[notion]", 1), "", "unknown key colour"},
		{"bad level", strings.Replace(notionConfig, "urgent =", "critical =", 1), "", "notion.priority.critical"},
		{"missing property", strings.Replace(notionConfig, "%s", `parent = "Mother"`, 1), "", `notion.properties.parent: the data source has no property "Mother"`},
		{"wrong type", strings.Replace(notionConfig, "%s", `parent = "Tags"`, 1), "", `"Tags" is a multi_select property, want relation`},
		{"property mapped twice", strings.Replace(notionConfig, "%s", `tags = "Tags"`, 1), "", `notion.properties.tags: "Tags" is already mapped by notion.properties.labels`},
		{"no token", notionConfig, "MKT_NOTION_TOKEN=", "MKT_NOTION_TOKEN is not set"},
		{"prefix mismatch", strings.Replace(notionConfig, `prefix = "MKT"`, `prefix = "MK"`, 1), "", `prefix is "MK", but Notion's "ID" property is prefixed "MKT"`},
		{"unknown done status", strings.Replace(notionConfig, `done = ["Done"]`, `done = ["Shipped"]`, 1), "", `notion.done: "Shipped" is not an option of "Status"`},
		{"bad token", notionConfig, "MKT_NOTION_TOKEN=wrong", "notion: unauthorized: API token is invalid."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := project(t, strings.Replace(tc.cfg, "%s", "", 1), tc.env)(t, "", "list")
			if res.Code == 0 || !strings.Contains(res.Stderr, tc.want) {
				t.Fatalf("want a failure naming %q, got exit %d, stderr %q", tc.want, res.Code, res.Stderr)
			}
		})
	}
}

// TestNotionFollowsCursors pages every list one row at a time: children and
// comments follow next_cursor to the same answer, list stops at its first page.
func TestNotionFollowsCursors(t *testing.T) {
	srv := notiontest.New(t)
	srv.PageLimit = 1
	run := projectOn(t, srv, strings.Replace(notionConfig, "%s", relations, 1))

	for _, tc := range []struct{ golden, verb, arg string }{
		{"children-2.json", "children", "2"},
		{"read-2.md", "read", "2"},
	} {
		res := run(t, "", tc.verb, tc.arg)
		if res.Code != 0 {
			t.Fatalf("%s: exit %d: %s", tc.verb, res.Code, res.Stderr)
		}

		golden(t, tc.golden, res.Stdout)
	}

	res := run(t, "", "list")
	if res.Code != 0 || strings.Count(res.Stdout, `"id":`) != 1 {
		t.Fatalf("list should answer its first page only, got exit %d: %s", res.Code, res.Stdout)
	}
}

func TestNotionDoneMatchesTheOptionsSpelling(t *testing.T) {
	run := project(
		t,
		strings.Replace(strings.Replace(notionConfig, "%s", relations, 1), `done = ["Done"]`, `done = ["done"]`, 1),
	)

	res := run(t, "", "list")
	if res.Code != 0 || strings.Contains(res.Stdout, `"id":"MKT-1"`) ||
		strings.Contains(res.Stdout, `"status":"Done"`) {
		t.Fatalf(
			"list should drop Done tasks for done = [\"done\"], got exit %d: %s%s",
			res.Code,
			res.Stdout,
			res.Stderr,
		)
	}
}

func TestAPIOverrideMustBeLoopback(t *testing.T) {
	run := project(t, strings.Replace(notionConfig, "%s", "", 1), "F10_DRIVERS_NOTION_API=https://api.example.com/v1")

	res := run(t, "", "list")
	if res.Code == 0 || !strings.Contains(res.Stderr, "must be a loopback url") {
		t.Fatalf("want the override refused, got exit %d, stderr %q", res.Code, res.Stderr)
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"notion"}, {"jira", "read", "1"}} {
		res := conformance.Command(binary, t.TempDir())(t, "", args...)
		if res.Code != 2 || !strings.Contains(res.Stderr, "usage:") {
			t.Errorf("%v: want exit 2 with usage, got %d, %q", args, res.Code, res.Stderr)
		}
	}
}

func TestVersion(t *testing.T) {
	res := conformance.Command(binary, t.TempDir())(t, "", "--version")
	if res.Code != 0 || !strings.HasPrefix(res.Stdout, "f10-drivers ") {
		t.Fatalf("want exit 0 with the version, got %d, %q", res.Code, res.Stdout)
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()

	path := filepath.Join("testdata", name)
	if *update {
		write(t, path, got, 0o644)

		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}

	if got != string(want) {
		t.Errorf("%s differs:\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

const newTask = "# Plan the launch email\n\nDraft the copy and pick the send date.\n\nAfter: MKT-3\n"

// TestNotionCreate creates a task with every property and reads it back:
// the same title and body, each property mapped through the config.
func TestNotionCreate(t *testing.T) {
	srv := notiontest.New(t)
	run := projectOn(t, srv, strings.Replace(notionConfig, "%s", relations, 1))

	res := run(t, newTask, "create", "--status", "in progress", "--priority", "urgent",
		"--label", "launch", "--label", "Report", "--parent", "2", "--after", "3")
	if res.Code != 0 || res.Stdout != "MKT-6\n" || res.Stderr != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", res.Code, res.Stdout, res.Stderr)
	}

	res = run(t, "", "read", "6")
	if res.Code != 0 {
		t.Fatalf("read: exit %d: %s", res.Code, res.Stderr)
	}

	golden(t, "create-read.md", res.Stdout)

	if res = run(t, "", "children", "2"); !strings.Contains(res.Stdout, `"id":"MKT-6"`) {
		t.Errorf("children of MKT-2 lacks MKT-6: %s", res.Stdout)
	}

	if srv.Creates() != 1 {
		t.Errorf("POST /pages sent %d times, want 1", srv.Creates())
	}
}

// TestNotionCreatePriorityFallsBackToTheLevel maps a level whose configured
// values are not options to the option named like the level.
func TestNotionCreatePriorityFallsBackToTheLevel(t *testing.T) {
	cfg := strings.Replace(strings.Replace(notionConfig, "%s", "", 1), `high = ["High", "P1"]`, `high = "P1"`, 1)
	run := project(t, cfg)

	if res := run(t, "# T", "create", "--priority", "high"); res.Code != 0 {
		t.Fatalf("exit %d: %s", res.Code, res.Stderr)
	}

	if res := run(t, "", "read", "6"); !strings.Contains(res.Stdout, "Priority: high\n") {
		t.Fatalf("read lacks the priority:\n%s", res.Stdout)
	}
}

func TestNotionCreateRejectsBeforeSending(t *testing.T) {
	base := strings.Replace(notionConfig, "%s", relations, 1)
	// Status as the priority property has no option named like a level, and
	// Priority as the labels property is a select.
	swapped := strings.NewReplacer(`priority = "Priority"`, `priority = "Status"`,
		`labels = "Tags"`, `labels = "Priority"`).Replace(base)

	for _, tc := range []struct {
		name, cfg string
		args      []string
		want      string
	}{
		{
			"status", base,
			[]string{"--status", "Blocked"},
			`--status "Blocked": no option of "Status" matches (Not started, In progress, Done)`,
		},
		{
			"label", base,
			[]string{"--label", "launch", "--label", "ads"},
			`--label "ads" is not an option of "Tags" (report, data, launch)`,
		},
		{"parent", base, []string{"--parent", "99"}, "--parent: MKT-99 not found"},
		{"after", base, []string{"--after", "99"}, "--after: MKT-99 not found"},
		{
			"priority without an option", swapped,
			[]string{"--priority", "urgent"},
			`--priority "urgent": no option of "Status" matches (Not started, In progress, Done)`,
		},
		{
			"two labels on a select", swapped,
			[]string{"--label", "High", "--label", "Low"},
			`--label: "Priority" is a select property and holds one label, got 2`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := notiontest.New(t)
			run := projectOn(t, srv, tc.cfg)

			res := run(t, "# T", append([]string{"create"}, tc.args...)...)
			if res.Code == 0 || !strings.Contains(res.Stderr, tc.want) {
				t.Fatalf("want a failure naming %q, got exit %d, stderr %q", tc.want, res.Code, res.Stderr)
			}

			if srv.Creates() != 0 {
				t.Fatalf("POST /pages sent %d times, want none", srv.Creates())
			}
		})
	}
}

// TestNotionCreateAfterWithoutTheLine prints the relation's After: line when
// the body does not end with it.
func TestNotionCreateAfterWithoutTheLine(t *testing.T) {
	run := project(t, strings.Replace(notionConfig, "%s", relations, 1))

	if res := run(t, "# T\n\nAfter: MKT-2 is not the blocker", "create", "--after", "3"); res.Code != 0 {
		t.Fatalf("exit %d: %s", res.Code, res.Stderr)
	}

	res := run(t, "", "read", "6")
	if !strings.HasSuffix(res.Stdout, "After: MKT-2 is not the blocker\n\nAfter: MKT-3\n") {
		t.Fatalf("read should end with the body, then the relation's After: line:\n%s", res.Stdout)
	}
}

func TestNotionCreateNeverRetriesServerErrors(t *testing.T) {
	srv := notiontest.New(t)
	srv.CreateStatus = 502
	run := projectOn(t, srv, strings.Replace(notionConfig, "%s", relations, 1))

	res := run(t, "# T", "create")

	want := "the task may have been created: check https://www.notion.so/" + notiontest.DatabaseID
	if res.Code == 0 || !strings.Contains(res.Stderr, want) || srv.Creates() != 1 {
		t.Fatalf("want one request and a failure naming %q, got exit %d, %d request(s), stderr %q",
			want, res.Code, srv.Creates(), res.Stderr)
	}
}

func TestNotionCreateDropsUnconfiguredProperties(t *testing.T) {
	run := project(t, strings.Replace(notionConfig, "%s", "", 1))

	res := run(t, newTask, "create", "--parent", "2", "--after", "3", "--assignee", "ann")
	if res.Code != 0 || !strings.Contains(res.Stderr, "warn: --parent dropped") ||
		!strings.Contains(res.Stderr, "warn: --after dropped") ||
		!strings.Contains(res.Stderr, "warn: --assignee dropped: create does not know it") {
		t.Fatalf("exit %d, stderr %q", res.Code, res.Stderr)
	}

	// Without the relation the body's After: line is the only record of it.
	if res = run(t, "", "read", "6"); !strings.Contains(res.Stdout, "After: MKT-3") {
		t.Fatalf("read lost the After: line:\n%s", res.Stdout)
	}
}

// TestConformanceAcceptsADriverThatDeclinesWrites runs the check against a
// driver that answers every verb but the write verbs, with the fixture not
// listing them.
func TestConformanceAcceptsADriverThatDeclinesWrites(t *testing.T) {
	run := project(t, strings.Replace(notionConfig, "%s", relations, 1))
	declining := func(t *testing.T, stdin string, args ...string) conformance.Result {
		if len(args) > 0 && slices.Contains([]string{"create", "comment", "update"}, args[0]) {
			return conformance.Result{Code: 3}
		}

		return run(t, stdin, args...)
	}

	conformance.Check(t, declining, conformance.Fixture{Task: "2", Parent: "2", Missing: "99", Query: "summary"})
}

// The MCP way in: the same config with transport = "mcp" and no token.
var mcpConfig = strings.Replace(notionConfig, `token_env = "MKT_NOTION_TOKEN"`, `transport = "mcp"`, 1)

// mcpProject is a project on srv's MCP face, with a home of its own for the
// login. It logs in first unless told not to.
func mcpProject(t *testing.T, srv *notiontest.Server, cfg string, login bool) (conformance.Runner, string, string) {
	t.Helper()

	home := t.TempDir()
	env := []string{
		"F10_DRIVERS_NOTION_MCP=" + srv.MCPURL(), "F10_DRIVERS_TOKEN_STORE=file", "F10_DRIVERS_NO_BROWSER=1",
		"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
	}

	run, root := projectIn(t, srv, cfg, env...)

	if login {
		loginAt(t, root, env)
	}

	return run, home, root
}

// loginAt runs `notion login` and approves it the way a user's browser
// does: it opens the printed url, which the fake redirects to the callback.
func loginAt(t *testing.T, root string, env []string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), filepath.Join(root, ".f10", "driver"), "login")
	cmd.Dir = root
	cmd.Env = append(cmd.Environ(), env...)

	var stderr strings.Builder

	cmd.Stderr = &stderr

	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var printed strings.Builder

	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line := sc.Text()
		printed.WriteString(line + "\n")

		if strings.HasPrefix(line, "http") {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, line, http.NoBody)
			if err != nil {
				t.Fatal(err)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}

			_ = resp.Body.Close()
		}
	}

	if err := cmd.Wait(); err != nil || !strings.Contains(printed.String(), "Logged in to ") {
		t.Fatalf("login: %v, stdout %q, stderr %q", err, printed.String(), stderr.String())
	}
}

func TestNotionMCPConformance(t *testing.T) {
	run, _, _ := mcpProject(t, notiontest.New(t), strings.Replace(mcpConfig, "%s", relations, 1), true)
	conformance.Check(t, run, fixture)
}

// TestNotionMCPAnswers pins that MCP answers what the REST path answers,
// but for app.notion.com urls, and no created in list and search: rows mode
// carries it only from a created_time property, which the fixture lacks.
func TestNotionMCPAnswers(t *testing.T) {
	run, _, _ := mcpProject(t, notiontest.New(t), strings.Replace(mcpConfig, "%s", relations, 1), true)

	for _, tc := range []struct{ golden, verb, arg string }{
		{"read-2.md", "read", "2"},
		{"read-4.mcp.md", "read", "4"},
		{"list.mcp.json", "list", ""},
		{"search.mcp.json", "search", "summary"},
		{"children-2.mcp.json", "children", "2"},
		{"branch-3.txt", "branch", "3"},
		{"url-3.mcp.txt", "url", "3"},
		{"index.mcp.txt", "index", ""},
	} {
		t.Run(tc.golden, func(t *testing.T) {
			args := []string{tc.verb}
			if tc.arg != "" {
				args = append(args, tc.arg)
			}

			res := run(t, "", args...)
			if res.Code != 0 {
				t.Fatalf("exit %d: %s", res.Code, res.Stderr)
			}

			golden(t, tc.golden, res.Stdout)
		})
	}
}

func TestNotionMCPCreate(t *testing.T) {
	srv := notiontest.New(t)
	run, _, _ := mcpProject(t, srv, strings.Replace(mcpConfig, "%s", relations, 1), true)

	res := run(t, newTask, "create", "--status", "in progress", "--priority", "urgent",
		"--label", "launch", "--label", "Report", "--parent", "2", "--after", "3")
	if res.Code != 0 || res.Stdout != "MKT-6\n" || res.Stderr != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", res.Code, res.Stdout, res.Stderr)
	}

	res = run(t, "", "read", "6")
	if res.Code != 0 {
		t.Fatalf("read: exit %d: %s", res.Code, res.Stderr)
	}

	golden(t, "create-read.md", res.Stdout)

	if srv.Creates() != 1 {
		t.Errorf("notion-create-pages called %d times, want 1", srv.Creates())
	}
}

func TestNotionMCPCreateNeverRetriesServerErrors(t *testing.T) {
	srv := notiontest.New(t)
	run, _, _ := mcpProject(t, srv, strings.Replace(mcpConfig, "%s", relations, 1), true)
	srv.CreateStatus = 502

	res := run(t, "# T", "create")

	want := "the task may have been created: check https://www.notion.so/" + notiontest.DatabaseID
	if res.Code == 0 || !strings.Contains(res.Stderr, want) || srv.Creates() != 1 {
		t.Fatalf("want one call and a failure naming %q, got exit %d, %d call(s), stderr %q",
			want, res.Code, srv.Creates(), res.Stderr)
	}
}

// TestNotionMCPLoginStaysOutOfTheProject logs in and checks the tokens live
// in a 0600 file under the user's config dir, and nowhere in the project.
func TestNotionMCPLoginStaysOutOfTheProject(t *testing.T) {
	srv := notiontest.New(t)
	run, home, root := mcpProject(t, srv, strings.Replace(mcpConfig, "%s", "", 1), true)

	if res := run(t, "", "index"); res.Code != 0 {
		t.Fatalf("index: exit %d: %s", res.Code, res.Stderr)
	}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		data, err := os.ReadFile(path)
		if err == nil && (strings.Contains(string(data), "access-") || strings.Contains(string(data), "refresh-")) {
			t.Errorf("%s holds a token", path)
		}

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	var stored []string

	err = filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "notion-logins.json" {
			stored = append(stored, path)
		}

		return err
	})
	if err != nil || len(stored) != 1 {
		t.Fatalf("want one login file under the home, got %v (%v)", stored, err)
	}

	if info, err := os.Stat(stored[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("login file mode: %v, %v", info.Mode(), err)
	}

	if srv.Authorizations() != 1 {
		t.Errorf("authorize hit %d times, want 1 (the login)", srv.Authorizations())
	}
}

func TestNotionMCPRefreshesAnExpiredToken(t *testing.T) {
	srv := notiontest.New(t)
	run, _, _ := mcpProject(t, srv, strings.Replace(mcpConfig, "%s", "", 1), true)
	srv.ExpireAccess()

	for range 2 { // the second run reads the refreshed token back from the store
		if res := run(t, "", "list"); res.Code != 0 {
			t.Fatalf("list after expiry: exit %d: %s", res.Code, res.Stderr)
		}
	}

	if srv.Authorizations() != 1 {
		t.Errorf("authorize hit %d times, want 1: a refresh needs no new login", srv.Authorizations())
	}
}

func TestNotionMCPRevokedLoginNamesTheLoginCommand(t *testing.T) {
	srv := notiontest.New(t)
	run, _, _ := mcpProject(t, srv, strings.Replace(mcpConfig, "%s", "", 1), true)
	srv.RevokeRefresh()

	res := run(t, "", "list")
	if res.Code == 0 || res.Code == 3 || !strings.Contains(res.Stderr, "run f10-drivers notion login") {
		t.Fatalf("want a failure naming the login command, got exit %d, stderr %q", res.Code, res.Stderr)
	}
}

func TestNotionMCPWithoutLoginNeverOpensABrowser(t *testing.T) {
	srv := notiontest.New(t)
	run, _, _ := mcpProject(t, srv, strings.Replace(mcpConfig, "%s", "", 1), false)

	res := run(t, "", "list")
	if res.Code == 0 || !strings.Contains(res.Stderr, "no Notion login: run f10-drivers notion login") {
		t.Fatalf("want a failure naming the login command, got exit %d, stderr %q", res.Code, res.Stderr)
	}

	if srv.Authorizations() != 0 || srv.ToolCalls("notion-query-data-sources") != 0 {
		t.Errorf("a verb without a login reached the server: %d authorize, %d queries",
			srv.Authorizations(), srv.ToolCalls("notion-query-data-sources"))
	}
}

func TestNotionMCPLogout(t *testing.T) {
	run, _, _ := mcpProject(t, notiontest.New(t), strings.Replace(mcpConfig, "%s", "", 1), true)

	if res := run(t, "", "logout"); res.Code != 0 || !strings.Contains(res.Stdout, "Logged out of ") {
		t.Fatalf("logout: exit %d, stdout %q, stderr %q", res.Code, res.Stdout, res.Stderr)
	}

	if res := run(t, "", "list"); res.Code == 0 || !strings.Contains(res.Stderr, "no Notion login") {
		t.Fatalf("list after logout: exit %d, stderr %q", res.Code, res.Stderr)
	}
}

// TestNotionMCPChildrenPastOnePageDeclines: rows mode has no cursor, so a
// children answer that would be cut short declines instead.
func TestNotionMCPChildrenPastOnePageDeclines(t *testing.T) {
	srv := notiontest.New(t)
	srv.RowLimit = 1
	run, _, _ := mcpProject(t, srv, strings.Replace(mcpConfig, "%s", relations, 1), true)

	res := run(t, "", "children", "2")
	if res.Code != 3 || res.Stdout != "" || !strings.Contains(res.Stderr, "cannot page past them") {
		t.Fatalf("want exit 3 with a warning, got exit %d, stdout %q, stderr %q", res.Code, res.Stdout, res.Stderr)
	}
}

func TestNotionMCPConfigRefusesTokenEnv(t *testing.T) {
	cfg := strings.Replace(
		strings.Replace(mcpConfig, "%s", "", 1),
		`transport = "mcp"`,
		"transport = \"mcp\"\ntoken_env = \"T\"",
		1,
	)

	res := project(t, cfg)(t, "", "list")
	if res.Code == 0 || !strings.Contains(res.Stderr, `notion.token_env applies only to transport = "api"`) {
		t.Fatalf("exit %d, stderr %q", res.Code, res.Stderr)
	}
}

// transports runs f once per way into srv: the REST API and the MCP server.
func transports(t *testing.T, f func(t *testing.T, srv *notiontest.Server, run conformance.Runner)) {
	t.Helper()

	t.Run("api", func(t *testing.T) {
		srv := notiontest.New(t)
		f(t, srv, projectOn(t, srv, strings.Replace(notionConfig, "%s", relations, 1)))
	})

	t.Run("mcp", func(t *testing.T) {
		srv := notiontest.New(t)
		run, _, _ := mcpProject(t, srv, strings.Replace(mcpConfig, "%s", relations, 1), true)
		f(t, srv, run)
	})
}

// TestNotionUpdate sets every kind of property and reads them back.
func TestNotionUpdate(t *testing.T) {
	transports(t, func(t *testing.T, srv *notiontest.Server, run conformance.Runner) {
		res := run(t, "", "update", "3", "--status", "in progress", "--priority", "urgent",
			"--label", "launch", "--label", "data", "--assignee", "BO@example.com",
			"--set", "result=https://example.com/r", "--set", "due=2026-11-02")
		if res.Code != 0 || res.Stdout != "MKT-3\n" || res.Stderr != "" {
			t.Fatalf("exit %d, stdout %q, stderr %q", res.Code, res.Stdout, res.Stderr)
		}

		res = run(t, "", "read", "3")
		for _, want := range []string{
			"Status: In progress\n", "Priority: urgent\n", "Labels: launch, data\n", "Assignee: Bo Chen\n",
			"Due: 2026-11-02\n", "Result: https://example.com/r\n",
		} {
			if !strings.Contains(res.Stdout, want) {
				t.Errorf("read lacks %q:\n%s", want, res.Stdout)
			}
		}

		// An empty --set clears; an assignee by name replaces.
		if res = run(t, "", "update", "3", "--set", "due=", "--assignee", "ann lee"); res.Code != 0 {
			t.Fatalf("clearing: exit %d: %s", res.Code, res.Stderr)
		}

		res = run(t, "", "read", "3")
		if strings.Contains(res.Stdout, "Due:") || !strings.Contains(res.Stdout, "Assignee: Ann Lee\n") {
			t.Errorf("want Due cleared and Ann Lee assigned:\n%s", res.Stdout)
		}

		// A multi_select --set takes option names in any case and replaces the options.
		for _, tc := range []struct{ set, want string }{
			{"feature=onboarding, Billing,ONBOARDING", "Feature: Onboarding, Billing\n"},
			{"feature=Search", "Feature: Search\n"},
			{"feature=", ""},
		} {
			if res = run(t, "", "update", "3", "--set", tc.set); res.Code != 0 {
				t.Fatalf("--set %s: exit %d: %s", tc.set, res.Code, res.Stderr)
			}

			res = run(t, "", "read", "3")
			if tc.want == "" && strings.Contains(res.Stdout, "Feature:") ||
				tc.want != "" && !strings.Contains(res.Stdout, tc.want) {
				t.Errorf("--set %s: want %q in read:\n%s", tc.set, tc.want, res.Stdout)
			}
		}
	})
}

func TestNotionUpdateRejectsBeforeSending(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no flags", nil, "update sets nothing"},
		{"unknown flag", []string{"--type", "bug"}, "update: flag provided but not defined: -type"},
		{"positional", []string{"--status", "Done", "extra"}, `got "extra"`},
		{"status", []string{"--status", "Blocked"}, `--status "Blocked": no option of "Status" matches`},
		{"label", []string{"--label", "ads"}, `--label "ads" is not an option of "Tags"`},
		{"unknown key", []string{"--set", "owner=x"}, "--set owner: not a key of [notion.properties] (due, feature, result)"},
		{"fixed key", []string{"--set", "labels=x"}, "--set labels: use --label"},
		{"no equals", []string{"--set", "result"}, `want key=value, got "result"`},
		{"bad url", []string{"--set", "result=example.com"}, `"Result" is a url property, want an absolute http(s) url`},
		{"bad date", []string{"--set", "due=next week"}, `"Due" is a date property, want a date`},
		{"no such option", []string{"--set", "feature=Billing,Ads"}, `"Ads" is not an option of "Feature" (Onboarding, Billing, Search)`},
		{"empty option", []string{"--set", "feature=Billing,"}, `want a comma-separated list of option names`},
		{"no such user", []string{"--assignee", "zed@example.com"}, "no Notion user has that email"},
		{"two users", []string{"--assignee", "Sam Park"}, `matches 2 users (Sam Park <sam.park@example.com>, Sam Park <sam.p@example.com>)`},
		{"missing task", []string{"--status", "Done"}, "MKT-99 not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transports(t, func(t *testing.T, srv *notiontest.Server, run conformance.Runner) {
				number := "3"
				if tc.name == "missing task" {
					number = "99"
				}

				res := run(t, "", append([]string{"update", number}, tc.args...)...)
				if res.Code != 1 || !strings.Contains(res.Stderr, tc.want) {
					t.Fatalf("want exit 1 naming %q, got exit %d, stderr %q", tc.want, res.Code, res.Stderr)
				}

				if srv.Updates() != 0 {
					t.Fatalf("%d update(s) sent, want none", srv.Updates())
				}
			})
		})
	}
}

func TestNotionUpdateFailsOnAnUnconfiguredFlag(t *testing.T) {
	srv := notiontest.New(t)
	run := projectOn(
		t,
		srv,
		strings.Replace(strings.Replace(notionConfig, "%s", "", 1), "assignee = \"Assignee\"\n", "", 1),
	)

	res := run(t, "", "update", "3", "--assignee", "ann@example.com")
	if res.Code != 1 || !strings.Contains(res.Stderr, "--assignee: notion.properties sets no property for it") ||
		srv.Updates() != 0 {
		t.Fatalf("exit %d, %d update(s), stderr %q", res.Code, srv.Updates(), res.Stderr)
	}
}

func TestNotionUpdateRetriesServerErrors(t *testing.T) {
	transports(t, func(t *testing.T, srv *notiontest.Server, run conformance.Runner) {
		srv.UpdateStatus = 502

		res := run(t, "", "update", "3", "--status", "Done")
		if res.Code != 0 || srv.Updates() != 2 {
			t.Fatalf(
				"want a retried update, got exit %d, %d request(s), stderr %q",
				res.Code,
				srv.Updates(),
				res.Stderr,
			)
		}
	})
}

// TestNotionComment posts markdown and reads its text back among the
// comments.
func TestNotionComment(t *testing.T) {
	transports(t, func(t *testing.T, srv *notiontest.Server, run conformance.Runner) {
		res := run(t, "## Result\n\nThe report is **ready**: [link](https://example.com/r).\n", "comment", "3")
		if res.Code != 0 || strings.TrimSpace(res.Stdout) == "" || strings.Count(res.Stdout, "\n") != 1 {
			t.Fatalf("exit %d, stdout %q, stderr %q", res.Code, res.Stdout, res.Stderr)
		}

		res = run(t, "", "read", "3")
		if !strings.Contains(res.Stdout, "Result\n\nThe report is ready: link.") ||
			strings.Contains(res.Stdout, "## Result") ||
			!strings.Contains(res.Stdout, "**f10**") {
			t.Fatalf("read lacks the comment:\n%s", res.Stdout)
		}

		if res = run(t, " \n", "comment", "3"); res.Code != 1 || !strings.Contains(res.Stderr, "got none") {
			t.Fatalf("an empty comment: exit %d, stderr %q", res.Code, res.Stderr)
		}

		if srv.Comments() != 1 {
			t.Fatalf("%d comment(s) sent, want 1", srv.Comments())
		}
	})
}

func TestNotionCommentNeverRetriesServerErrors(t *testing.T) {
	transports(t, func(t *testing.T, srv *notiontest.Server, run conformance.Runner) {
		srv.CommentStatus = 502

		res := run(t, "Done.", "comment", "3")
		if res.Code == 0 || !strings.Contains(res.Stderr, "the comment may have been posted: check https://") ||
			srv.Comments() != 1 {
			t.Fatalf("want one request and an unknown outcome, got exit %d, %d request(s), stderr %q",
				res.Code, srv.Comments(), res.Stderr)
		}
	})
}
