// Command f10-drivers is a tracker driver for f10, configured per project:
//
//	f10-drivers <tracker> [--config <path>] <verb> [arguments]
//
// A project's .f10/driver is a one-line shim, `exec f10-drivers notion "$@"`,
// and its facts live in .f10/driver.toml.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/amberpixels/f10-drivers/config"
	"github.com/amberpixels/f10-drivers/contract"
	"github.com/amberpixels/f10-drivers/notion"
)

const usage = `usage: f10-drivers <tracker> [--config <path>] <verb> [arguments]
       f10-drivers notion login | logout
       f10-drivers --version

trackers: notion
verbs:    read, url, search, branch, children, index, list,
          create [--status <s>] [--priority <p>] [--label <l>]... [--parent <n>] [--after <n>] < task.md
          comment <n> < comment.md
          update <n> [--status <s>] [--priority <p>] [--label <l>]... [--assignee <email|name>]
                     [--set <key>=<value>]...
config:   .f10/driver.toml under the working directory, unless --config names one`

const exitUsage = 2

// trackers builds each tracker from its config; warn takes non-fatal notices.
var trackers = map[string]func(ctx context.Context, cfg *config.Config, warn io.Writer) (contract.Tracker, error){
	"notion": func(ctx context.Context, cfg *config.Config, warn io.Writer) (contract.Tracker, error) {
		api, err := testAPI("F10_DRIVERS_NOTION_API")
		if err != nil {
			return nil, err
		}

		mcp, err := notionMCP()
		if err != nil {
			return nil, err
		}

		return notion.New(ctx, cfg, notion.Endpoints{API: api, MCP: mcp}, warn)
	},
}

// notionMCP is the MCP server the notion transport "mcp" logs in to.
func notionMCP() (string, error) {
	mcp, err := testAPI("F10_DRIVERS_NOTION_MCP")
	if err != nil || mcp != "" {
		return mcp, err
	}

	return notion.DefaultMCPURL, nil
}

// noBrowserEnv set makes login only print the url, for a test or a machine
// with no browser to open.
const noBrowserEnv = "F10_DRIVERS_NO_BROWSER"

// notionAccount answers `notion login` and `notion logout`, which need no
// config: there is one MCP server.
func notionAccount(ctx context.Context, verb string, stdout io.Writer) error {
	server, err := notionMCP()
	if err != nil {
		return err
	}

	if verb == "logout" {
		return notion.Logout(ctx, server, stdout)
	}

	open := func(u string) error { return openBrowser(ctx, u) }
	if os.Getenv(noBrowserEnv) != "" {
		open = nil
	}

	return notion.Login(ctx, notion.LoginOptions{Server: server, Open: open, Out: stdout})
}

// openBrowser opens a url with the OS's opener; it does not wait for the
// browser.
func openBrowser(ctx context.Context, u string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", u)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", u)
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	go func() { _ = cmd.Wait() }()

	return nil
}

// testAPI reads an API root override, which exists for tests: it must point
// at this machine, so a stray value never sends the token elsewhere.
func testAPI(env string) (string, error) {
	v := os.Getenv(env)
	if v == "" {
		return "", nil
	}

	u, err := url.Parse(v)
	if err != nil || !notion.IsLoopback(u.Hostname()) {
		return "", fmt.Errorf("%s must be a loopback url, got %q", env, v)
	}

	return v, nil
}

// version is the module version Go stamps into the build: the tag for a
// tagged checkout or `go install ...@v0.1.0`, a pseudo-version past it.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}

	return "dev"
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)

	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, "f10-drivers", version())

		return 0
	}

	inv, err := parseArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "f10-drivers: %v\n\n%s\n", err, usage)

		return exitUsage
	}

	verb, verbArgs := inv.rest[0], inv.rest[1:]

	newTracker, ok := trackers[inv.tracker]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "f10-drivers: unknown tracker %q\n\n%s\n", inv.tracker, usage)

		return exitUsage
	}

	if inv.tracker == "notion" && (verb == "login" || verb == "logout") && len(verbArgs) == 0 {
		if err := notionAccount(ctx, verb, stdout); err != nil {
			return fail(stderr, err)
		}

		return 0
	}

	cfg, err := config.Load(inv.config)
	if err != nil {
		return fail(stderr, err)
	}

	tracker, err := newTracker(ctx, cfg, stderr)
	if err != nil {
		return fail(stderr, err)
	}

	err = contract.Serve(ctx, tracker, verb, verbArgs, stdin, stdout)
	if errors.Is(err, contract.ErrUnsupported) {
		return contract.ExitUnsupported
	}

	if err != nil {
		return fail(stderr, err)
	}

	return 0
}

// invocation is one parsed command line.
type invocation struct {
	tracker string
	config  string   // --config, empty for the default path
	rest    []string // the verb and its arguments
}

// parseArgs splits `<tracker> [--config <path>] <verb> [arguments]`.
func parseArgs(args []string) (invocation, error) {
	var inv invocation

	if len(args) == 0 {
		return inv, errors.New("no tracker given")
	}

	inv.tracker, args = args[0], args[1:]

	for len(args) > 0 {
		switch a := args[0]; {
		case a == "--config":
			if len(args) < 2 {
				return inv, errors.New("--config needs a path")
			}

			inv.config, args = args[1], args[2:]
		case strings.HasPrefix(a, "--config="):
			inv.config, args = strings.TrimPrefix(a, "--config="), args[1:]
		default:
			inv.rest, args = args, nil
		}
	}

	if len(inv.rest) == 0 {
		return inv, errors.New("no verb given")
	}

	return inv, nil
}

func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "f10-drivers: %v\n", err)

	return 1
}
