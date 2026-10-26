package main_test

import (
	"flag"
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
%s

[notion.priority]
urgent = "Urgent"
high = ["High", "P1"]
medium = "Medium"
low = "Low"
`

const relations = `parent = "Parent"
after = "Blocked by"`

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

	return conformance.Command(filepath.Join(storage, "driver"), root, env...)
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestNotionConformance(t *testing.T) {
	run := project(t, strings.Replace(notionConfig, "%s", relations, 1))
	conformance.Check(t, run, conformance.Fixture{Task: "2", Parent: "2", Missing: "99", Query: "summary"})
}

func TestNotionWithoutRelationsDeclinesChildren(t *testing.T) {
	run := project(t, strings.Replace(notionConfig, "%s", "", 1))
	conformance.Check(t, run, conformance.Fixture{
		Task: "2", Missing: "99", Query: "summary", Declined: []string{"children"},
	})
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

			res := run(t, args...)
			if res.Code != 0 {
				t.Fatalf("exit %d: %s", res.Code, res.Stderr)
			}

			golden(t, tc.golden, res.Stdout)
		})
	}
}

func TestNotionWarnsOnTruncatedBody(t *testing.T) {
	run := project(t, strings.Replace(notionConfig, "%s", relations, 1))

	res := run(t, "read", "4")
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
		{"no token", notionConfig, "MKT_NOTION_TOKEN=", "MKT_NOTION_TOKEN is not set"},
		{"prefix mismatch", strings.Replace(notionConfig, `prefix = "MKT"`, `prefix = "MK"`, 1), "", `prefix is "MK", but Notion's "ID" property is prefixed "MKT"`},
		{"unknown done status", strings.Replace(notionConfig, `done = ["Done"]`, `done = ["Shipped"]`, 1), "", `notion.done: "Shipped" is not an option of "Status"`},
		{"bad token", notionConfig, "MKT_NOTION_TOKEN=wrong", "notion: unauthorized: API token is invalid."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := project(t, strings.Replace(tc.cfg, "%s", "", 1), tc.env)(t, "list")
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
		res := run(t, tc.verb, tc.arg)
		if res.Code != 0 {
			t.Fatalf("%s: exit %d: %s", tc.verb, res.Code, res.Stderr)
		}

		golden(t, tc.golden, res.Stdout)
	}

	res := run(t, "list")
	if res.Code != 0 || strings.Count(res.Stdout, `"id":`) != 1 {
		t.Fatalf("list should answer its first page only, got exit %d: %s", res.Code, res.Stdout)
	}
}

func TestNotionDoneMatchesTheOptionsSpelling(t *testing.T) {
	run := project(
		t,
		strings.Replace(strings.Replace(notionConfig, "%s", relations, 1), `done = ["Done"]`, `done = ["done"]`, 1),
	)

	res := run(t, "list")
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

	res := run(t, "list")
	if res.Code == 0 || !strings.Contains(res.Stderr, "must be a loopback url") {
		t.Fatalf("want the override refused, got exit %d, stderr %q", res.Code, res.Stderr)
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"notion"}, {"jira", "read", "1"}} {
		res := conformance.Command(binary, t.TempDir())(t, args...)
		if res.Code != 2 || !strings.Contains(res.Stderr, "usage:") {
			t.Errorf("%v: want exit 2 with usage, got %d, %q", args, res.Code, res.Stderr)
		}
	}
}

func TestVersion(t *testing.T) {
	res := conformance.Command(binary, t.TempDir())(t, "--version")
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
