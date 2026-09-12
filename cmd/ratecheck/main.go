// Command ratecheck finds out whether an HTTP rate limiter holds when requests arrive
// together, which is the only time it matters and the one case sequential testing misses.
//
// Point it at your own service. It sends real requests and deliberately exhausts a quota.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Nappuccino-tlg/ratecheck/internal/probe"
)

// version is stamped by the release build with -ldflags. Left alone otherwise, and
// resolved at runtime by versionString.
var version = "dev"

// versionString answers for all three ways this binary comes into existence.
//
// A release binary carries the tag, stamped in at build time. One from `go install
// ...@v0.1.0` carries nothing, but Go records the module version it was built from and
// that is the same answer -- so read it rather than making everyone who installs the
// normal way see "dev". A build from a working tree really is dev, and says so.
func versionString() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return version
}

// Exit codes, so this is usable as a CI gate.
const (
	exitHeld    = 0 // the limiter held, or there was nothing to test
	exitLeaked  = 1 // more got through together than the limit allows
	exitUnknown = 2 // could not answer: bad usage, unreachable host, inconclusive burst
)

type headerFlag http.Header

func (h headerFlag) String() string { return "" }

func (h headerFlag) Set(raw string) error {
	name, value, err := probe.ParseHeader(raw)
	if err != nil {
		return err
	}
	http.Header(h).Add(name, value)
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ratecheck", flag.ContinueOnError)
	flags.SetOutput(stderr)

	headers := headerFlag(http.Header{})
	flags.Var(headers, "H", "header to send, as \"Name: value\" (repeatable)")
	method := flags.String("X", http.MethodGet, "HTTP method")
	reject := flags.Int("status", http.StatusTooManyRequests, "the status that means denied")
	maxProbes := flags.Int("n", 200, "most requests to spend finding the limit")
	burst := flags.Int("burst", 0, "requests to fire at once (default: the limit plus half again)")
	resetWait := flags.Duration("reset", 90*time.Second, "how long to wait for quota to return; 0 to skip")
	pace := flags.Duration("pace", 0, "gap between the requests that look for the limit")
	timeout := flags.Duration("timeout", 10*time.Second, "per-request timeout")
	asJSON := flags.Bool("json", false, "write the report as JSON")
	showVersion := flags.Bool("version", false, "print the version and exit")

	flags.Usage = func() {
		fmt.Fprintf(stderr, "ratecheck -- does this rate limiter hold when requests arrive together?\n\n")
		fmt.Fprintf(stderr, "usage: ratecheck [flags] URL\n\n")
		flags.PrintDefaults()
		fmt.Fprintf(stderr, "\nexit codes: %d held, %d leaked, %d could not tell\n",
			exitHeld, exitLeaked, exitUnknown)
		fmt.Fprintf(stderr, "\nPoint it at a service you are responsible for. It sends real\n")
		fmt.Fprintf(stderr, "requests and deliberately exhausts a quota.\n")
	}

	if err := flags.Parse(args); err != nil {
		return exitUnknown
	}
	if *showVersion {
		fmt.Fprintf(stdout, "ratecheck %s\n", version)
		return exitHeld
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return exitUnknown
	}

	target := flags.Arg(0)
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		fmt.Fprintf(stderr, "ratecheck: %q needs an http:// or https:// scheme\n", target)
		return exitUnknown
	}
	if *maxProbes < 1 {
		fmt.Fprintln(stderr, "ratecheck: -n must be at least 1")
		return exitUnknown
	}
	if *burst < 0 {
		fmt.Fprintln(stderr, "ratecheck: -burst cannot be negative")
		return exitUnknown
	}

	// Ctrl-C stops the run rather than the process, so a partial report still prints and
	// the connections still close.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	opts := probe.Options{
		Request: probe.Request{
			Method:  strings.ToUpper(*method),
			URL:     target,
			Headers: http.Header(headers),
			Timeout: *timeout,
		},
		RejectStatus: *reject,
		MaxProbes:    *maxProbes,
		Burst:        *burst,
		ResetWait:    *resetWait,
		Pace:         *pace,
	}

	progress := func(line string) { fmt.Fprintln(stderr, line) }
	if *asJSON {
		// Progress goes to stderr so it never lands in piped JSON, but a JSON caller is a
		// script and does not want it at all.
		progress = func(string) {}
	}

	report, err := probe.Run(ctx, opts, progress)
	if err != nil && !errors.Is(err, probe.ErrNoLimit) {
		return reportError(stderr, err)
	}

	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if encodeErr := encoder.Encode(report); encodeErr != nil {
			fmt.Fprintf(stderr, "ratecheck: %v\n", encodeErr)
			return exitUnknown
		}
	} else {
		report.WriteText(stdout)
	}

	switch report.Verdict {
	case probe.Leaked:
		return exitLeaked
	case probe.Held:
		return exitHeld
	default:
		// No limit at all is not a failure of this tool, and not something to fail CI on.
		if errors.Is(err, probe.ErrNoLimit) {
			return exitHeld
		}
		return exitUnknown
	}
}

func reportError(stderr io.Writer, err error) int {
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(stderr, "ratecheck: stopped")
		return exitUnknown
	}
	fmt.Fprintf(stderr, "ratecheck: %v\n", err)
	return exitUnknown
}
