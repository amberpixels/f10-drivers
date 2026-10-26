// Command f10-drivers is a tracker driver for f10, configured per project:
//
//	f10-drivers <tracker> [--config <path>] <verb> [argument]
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
	"os/signal"
	"runtime/debug"
	"strings"

	"github.com/amberpixels/f10-drivers/config"
	"github.com/amberpixels/f10-drivers/contract"
	"github.com/amberpixels/f10-drivers/notion"
)

const usage = `usage: f10-drivers <tracker> [--config <path>] <verb> [argument]
       f10-drivers --version

trackers: notion
verbs:    read, url, search, branch, children, index, list
config:   .f10/driver.toml under the working directory, unless --config names one`

const exitUsage = 2

// trackers builds each tracker from its config; warn takes non-fatal notices.
var trackers = map[string]func(cfg *config.Config, warn io.Writer) (contract.Tracker, error){
	"notion": func(cfg *config.Config, warn io.Writer) (contract.Tracker, error) {
		base, err := testAPI("F10_DRIVERS_NOTION_API")
		if err != nil {
			return nil, err
		}

		return notion.New(cfg, base, warn)
	},
}

// testAPI reads an API root override, which exists for tests: it must point
// at this machine, so a stray value never sends the token elsewhere.
func testAPI(env string) (string, error) {
	v := os.Getenv(env)
	if v == "" {
		return "", nil
	}

	u, err := url.Parse(v)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
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
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)

	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, "f10-drivers", version())

		return 0
	}

	inv, err := parseArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "f10-drivers: %v\n\n%s\n", err, usage)

		return exitUsage
	}

	verb, arg := inv.rest[0], ""
	if len(inv.rest) > 1 {
		arg = inv.rest[1]
	}

	newTracker, ok := trackers[inv.tracker]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "f10-drivers: unknown tracker %q\n\n%s\n", inv.tracker, usage)

		return exitUsage
	}

	cfg, err := config.Load(inv.config)
	if err != nil {
		return fail(stderr, err)
	}

	tracker, err := newTracker(cfg, stderr)
	if err != nil {
		return fail(stderr, err)
	}

	err = contract.Serve(ctx, tracker, verb, arg, stdout)
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
	rest    []string // the verb and its argument
}

// parseArgs splits `<tracker> [--config <path>] <verb> [argument]`.
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
