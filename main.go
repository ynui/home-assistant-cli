package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"
)

var (
	flagURL     = flag.String("url", "", "override configured server URL")
	flagToken   = flag.String("token", "", "override configured long-lived token")
	flagOutput  = flag.String("o", "table", "output format: table or json")
	flagTimeout = flag.Duration("timeout", 30*time.Second, "per-request timeout")
	flagVersion = flag.Bool("v", false, "print version and exit")
	// --version is the same flag with a longer name; the stdlib treats them as
	// different names, so both have to be registered.
	flagVersion2 = flag.Bool("version", false, "print version and exit")
)

// version is set at build time via -ldflags "-X main.version=v0.3.0".
// Defaults to "dev" for local builds (go run, go build without ldflags).
var version = "dev"

// ctx is cancelled when the user presses ^C. One context for the whole
// program, set up by main: an in-flight request stops now instead of waiting
// out --timeout, and `has watch` leaves its read loop and exits cleanly.
var ctx = context.Background()

const usageText = `has - a small command-line client for Home Assistant

usage: has [global flags] <command> [args]

commands:
  version                    print the version
  login                       connect to Home Assistant and save the config
  groups                      list entity domains, busiest first
  areas [selector...]         list areas and the entities in them
  list [selector...]          list entity states (default table, -o json for attributes)
  get <entity>                show exactly one entity
  ui                          interactive browser and toggle (--poll 0 to disable live updates)
  on <selector...>            turn entities on
  off <selector...>           turn entities off
  toggle <selector...>        toggle entities
  call <domain.service> [k=v]  call any service with optional arguments
  history <entity...>         show recent state changes
  logbook [selector...]       what happened today, house-wide or for a selector
  watch [selector...]         tail state changes as they happen
  services [pattern]          list callable services
  template <jinja>            render a template on the server (stdin if omitted)

A selector is an entity id (light.kitchen), a domain (light, or lights), or a
fuzzy name (kitchen). Reads accept many matches; writes need --all when a
selector is not a single exact entity id.

list flags: --sort entity|name|state|changed|updated, --reverse, --limit N,
  --brief (with -o json, entity_id, name and state only).
on/off/toggle/call flags: --all, --dry-run (resolve and print, without calling).
logbook flags: --day today|yesterday|YYYY-MM-DD, --limit N.
watch and template print their own output: a stream of changes, and whatever the
template rendered.

exit codes: 1 error, 2 bad command line, 3 a selector matched nothing or too
much, 4 the token was refused, 5 the server could not be reached.

config lives in ~/.config/has/config.json. HAS_URL, HAS_TOKEN and HAS_CONFIG
override it; --url and --token override those.
`

func main() {
	stop := onInterrupt()
	defer stop()

	flag.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	flag.Parse()

	if *flagVersion || *flagVersion2 {
		fmt.Println(version)
		return
	}

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(exitUsage)
	}

	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "version", "--version":
		fmt.Println(version)
		return
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return
	case "login":
		err = cmdLogin(rest)
	case "groups":
		err = cmdGroups(rest)
	case "list", "ls":
		err = cmdList(rest)
	case "ui":
		err = cmdUI(rest)
	case "get":
		err = cmdGet(rest)
	case "on":
		err = cmdCall(rest, "on", "homeassistant", "turn_on")
	case "off":
		err = cmdCall(rest, "off", "homeassistant", "turn_off")
	case "toggle":
		err = cmdCall(rest, "toggle", "homeassistant", "toggle")
	case "call":
		err = cmdCall(rest, "call", "", "")
	case "history":
		err = cmdHistory(rest)
	case "logbook":
		err = cmdLogbook(rest)
	case "watch":
		err = cmdWatch(rest)
	case "areas":
		err = cmdAreas(rest)
	case "services":
		err = cmdServices(rest)
	case "template":
		err = cmdTemplate(rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		flag.Usage()
		os.Exit(exitUsage)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "has: "+err.Error())
	os.Exit(exitCode(err))
}

// onInterrupt makes ^C cancel ctx. It is assigned rather than shadowed, which
// is why it is a function and not three lines in main.
func onInterrupt() context.CancelFunc {
	var stop context.CancelFunc
	ctx, stop = signal.NotifyContext(ctx, os.Interrupt)
	return stop
}

// Exit codes, so a script can tell one failure from another without reading the
// message. Everything non-zero stays non-zero, so `if has list kitchen; then`
// behaves as before; what changes is that the reason is machine-readable.
const (
	exitError    = 1 // something went wrong, the message says what
	exitUsage    = 2 // the command line was wrong
	exitSelector = 3 // a selector matched nothing, or matched more than one
	exitAuth     = 4 // the token was refused; `has login` is the only fix
	exitNetwork  = 5 // the server could not be reached
)

// coded is an error that knows which exit code it should produce. Wrapping this
// way rather than tagging the message keeps two promises at once: a script can
// tell one failure from another, and every message a user reads is byte for byte
// what it was before the codes existed.
type coded struct {
	code int
	err  error
}

func (e coded) Error() string { return e.err.Error() }
func (e coded) Unwrap() error { return e.err }

// exitCode maps an error to the code it should exit with. errors.As unwraps, so
// it still finds the code after Client.do has wrapped the error on the way out
// of an HTTP call.
func exitCode(err error) int {
	var c coded
	var unreachable *url.Error
	switch {
	case err == nil:
		return 0
	case errors.As(err, &c):
		return c.code
	case errors.As(err, &unreachable), errors.Is(err, context.DeadlineExceeded):
		return exitNetwork
	}
	return exitError
}

// newFlagSet gives each subcommand its own parser carrying the global flags,
// so `has list -o json` works and not only `has -o json list`. The current
// values become the defaults, so a flag given before the subcommand survives.
//
// The output format is checked here rather than in main: this is the parser that
// sees -o whichever side of the subcommand it was written on, so one check
// covers `has list -o bogus` as well as `has -o bogus list`.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(flagURL, "url", *flagURL, "server URL")
	fs.StringVar(flagToken, "token", *flagToken, "long-lived access token")
	fs.StringVar(flagOutput, "o", *flagOutput, "output format: table or json")
	fs.DurationVar(flagTimeout, "timeout", *flagTimeout, "per-request timeout")
	if err := checkOutputFormat(); err != nil {
		fatal(err)
	}
	return fs
}

// checkOutputFormat rejects an output format we cannot produce. It is a
// function so a test can call it without newFlagSet's fatal on failure.
func checkOutputFormat() error {
	if *flagOutput != "table" && *flagOutput != "json" {
		return coded{exitUsage, fmt.Errorf("bad -o %q: want table or json", *flagOutput)}
	}
	return nil
}

// parseArgs splits args into flags and positionals before handing them to a
// subcommand's FlagSet. The stdlib parser stops at the first positional, which
// would silently ignore `has history light.x --since 5m`.
//
// It walks the list in order instead of splitting it once: each run of flags
// goes to the FlagSet, each positional is kept where it was written, and the
// two come back interleaved. The order matters - `has call` reads rest[0] as the
// service to call - and it used not to survive a bool flag, which pulled the
// following positional into the flag list and pushed it to the front.
//
// `--` still ends flag parsing: everything after it is positional.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		n := endOfFlags(fs, args)
		if err := fs.Parse(args[:n]); err != nil {
			return nil, err
		}
		switch {
		case n == len(args): // flags only from here on
			if err := checkOutputFormat(); err != nil {
				return nil, err
			}
			return append(fs.Args(), rest...), nil
		case args[n] == "--":
			return append(fs.Args(), append(rest, args[n+1:]...)...), nil
		}
		rest = append(rest, args[n])
		args = args[n+1:]
	}
}

// endOfFlags returns the index of the first argument that is not a flag: the
// end of the list, a bare `--`, or anything not starting with a dash. A flag
// written as `-flag value` counts its value too, so `-limit 5` is two
// arguments rather than a flag and a positional.
func endOfFlags(fs *flag.FlagSet, args []string) int {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || len(a) < 2 || a[0] != '-' {
			return i
		}
		if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && takesValue(fs, a) {
			i++
		}
	}
	return len(args)
}

// takesValue reports whether a flag needs the next argument as its value. The
// stdlib bool flags answer this themselves, and that is the whole point: a bool
// does not, so swallowing whatever followed one put `entity_id=kitchen` in
// front of the service name in `has call light.turn_on --all entity_id=kitchen`,
// and dropped every flag written after it - the parse had already stopped.
func takesValue(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(strings.TrimLeft(name, "-"))
	if f == nil {
		return true // unknown flag: leave the complaint to Parse
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !b.IsBoolFlag()
}

// emitJSON writes one json document to stdout. HTML escaping is off because
// every consumer of this output is jq or an agent, not a browser: an entity
// called "Hall & Stairs" should come back as itself and not as "Hall \u0026
// Stairs".
func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// timeFormat drops the seconds. A one-minute resolution is enough to read at a
// glance; -o json keeps the full ISO timestamp for anything finer.
const timeFormat = "2006-01-02 15:04"

// logbookFormat keeps them. A day of logbook holds changes inside a single
// minute - one presence sensor managed 847 of them in a day on the house this
// was written against - and a column of identical times says nothing about
// which change came first.
const logbookFormat = "2006-01-02 15:04:05"

// table writes space-aligned columns, truncating each cell to its width so one
// long value cannot stretch the whole table. A width of 0 means no limit.
func table(w io.Writer, headers []string, widths []int, rows [][]string) error {
	var buf strings.Builder
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(fit(headers, widths), "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(fit(r, widths), "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// tabwriter pads the final column even when it is empty, so strip trailing
	// spaces to keep copied and piped output clean.
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if _, err := fmt.Fprintln(w, strings.TrimRight(line, " \t")); err != nil {
			return err
		}
	}
	return nil
}

func fit(cells []string, widths []int) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		w := 0
		if i < len(widths) {
			w = widths[i]
		}
		out[i] = truncate(c, w)
	}
	return out
}

// truncate shortens s to at most n characters, marking the cut with an ellipsis.
// Counts runes rather than bytes so multi-byte names are never split in half.
func truncate(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// pad makes s exactly n runes wide, truncating or space-padding as needed.
// This exists because fmt's %-13s pads by bytes, and the ellipsis is three
// bytes, so every truncated value shifted its column left by two.
func pad(s string, n int) string {
	if n <= 0 {
		return ""
	}
	t := truncate(s, n)
	r := []rune(t)
	if len(r) >= n {
		return t
	}
	return t + strings.Repeat(" ", n-len(r))
}

func timestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format(timeFormat)
}

func timestampSec(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format(logbookFormat)
}
