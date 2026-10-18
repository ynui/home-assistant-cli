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
	// The stdlib treats --version and -v as unrelated flags, so both are
	// registered and checked together in main.
	flagVersion2 = flag.Bool("version", false, "print version and exit")
)

// version is set at build time with -ldflags "-X main.version=..."; local
// builds show "dev".
var version = "dev"

// ctx is cancelled by ^C, so an in-flight request stops instead of waiting
// out --timeout, and `has watch` exits cleanly. Set up in main (onInterrupt).
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
  call <domain.service> [entity...] [k=v]
                                 call any service, naming entities in position
  history <entity...>         show recent state changes
  logbook [selector...]       what happened today, house-wide or for a selector
  watch [selector...]         tail state changes as they happen
  services [pattern]          list callable services
  template <jinja>            render a template on the server (stdin if omitted)
  agent                       a usage brief written for AI agents

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

// agentText is `has agent`: a one-page brief for AI agents, who read the help
// once or not at all and then guess. It carries only the invariants — the
// things that make a wrong guess a wrong call — not a feature tour.
const agentText = `has: command-line client for Home Assistant — notes for AI agents

Select final answers from real output; run, don't guess:
  has groups                  # domains, busiest first
  has list <selector>         # entity/id/name matches; -o json for attributes
  has areas <area>            # entities in an area (name or partial, e.g. living)
  has get <entity>            # one entity's full attributes
  has services [pattern] -o json   # callable services; .fields = argument names
  has template '<jinja>'      # anything /api/template can answer

Selectors: an exact entity id (light.dresser_lamp), a domain (light, or
lights), or a substring of id or friendly name (dresser, "Dresser Lamp").
Reads accept many matches. Writes act on the match: a selector naming one
friendly name exactly means that entity even if helpers ("Dresser Lamp
Power-on behavior") share the substring; otherwise a write refuses and lists
candidates — name one exactly or pass --all, never guess.

Enacting a change:
  has on/off/toggle <selector...> [--all] [--dry-run]
  has call <domain.service> [entity...] [key=value ...] [--all] [--dry-run]

--area <name> scopes any selector-taking command to an area, by name or part
of one (has list --area living light, has on --area living --all). Resolve
still runs first, so a write refusing more than one entity without --all is
intact: --area is opt-in, never a silent blast radius.

Call details: quoted names are one token ("Dresser Lamp"); entities and
key=value arguments may come in either order. Relative or absolute values the
service schema advertises work as raw key=value pairs (has call light.turn_on
light.dresser_lamp brightness_pct=70) — field names come from
has services <domain.service> -o json, not from folklore. --dry-run first:
it resolves and prints the payload without a POST; the only confirmation a
write gets back is the resulting states it prints, entity -> state.

Output contract: results on stdout, warnings on stderr; -o json works on
every command. Exit codes: 0 ok, 1 error, 2 bad command line, 3 selector
matched nothing or too much, 4 token refused, 5 server unreachable. "has
watch <selector>" tails events; has needs a Home Assistant URL and long-lived
token (has login), overridable by --url/--token or HAS_URL/HAS_TOKEN.
`

// cmdAgent prints the agent brief; a function so tests can capture it.
func cmdAgent() {
	fmt.Print(agentText)
}

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
	case "agent":
		cmdAgent()
		return
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

// onInterrupt makes ^C cancel ctx.
func onInterrupt() context.CancelFunc {
	var stop context.CancelFunc
	ctx, stop = signal.NotifyContext(ctx, os.Interrupt)
	return stop
}

// Exit codes, so a script knows why a command failed without reading the
// message.
const (
	exitError    = 1 // something went wrong, the message says what
	exitUsage    = 2 // the command line was wrong
	exitSelector = 3 // a selector matched nothing, or matched more than one
	exitAuth     = 4 // the token was refused; `has login` is the only fix
	exitNetwork  = 5 // the server could not be reached
)

// coded is an error wrapped with the exit code it should produce. Wrapping
// instead of tagging the message keeps the text a user reads unchanged.
type coded struct {
	code int
	err  error
}

func (e coded) Error() string { return e.err.Error() }
func (e coded) Unwrap() error { return e.err }

// exitCode maps an error to its exit code. errors.As unwraps wrapped errors,
// so the code is found even after Client.do wraps it.
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

// newFlagSet gives each subcommand the global flags, so `has list -o json`
// works and not only `has -o json list`. The current values become the
// defaults, so a flag written before the subcommand survives. The output
// format is checked here because this is the parser that sees -o on either
// side of the subcommand name.
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

// checkOutputFormat rejects an output format we cannot produce.
func checkOutputFormat() error {
	if *flagOutput != "table" && *flagOutput != "json" {
		return coded{exitUsage, fmt.Errorf("bad -o %q: want table or json", *flagOutput)}
	}
	return nil
}

// parseArgs splits args into flags and positionals before the FlagSet sees
// them. The stdlib parser stops at the first positional, which would silently
// ignore `has history light.x --since 5m`, so it parses flags run by run and
// keeps each positional where it was written. The order matters: `has call`
// reads rest[0] as the service name. `--` still ends flag parsing.
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

// endOfFlags returns the index of the first non-flag argument: the end of the
// list, a bare `--`, or anything not starting with a dash. A flag of the form
// `-flag value` counts its value too, so `-limit 5` is two arguments.
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

// takesValue reports whether a flag needs the next argument as its value.
// Bool flags do not, so they do not swallow the positional that follows.
func takesValue(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(strings.TrimLeft(name, "-"))
	if f == nil {
		return true // unknown flag: let Parse produce the error
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !b.IsBoolFlag()
}

// emitJSON writes one json document to stdout. HTML escaping is off because
// the consumers are jq and scripts, not browsers.
func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// timeFormat drops seconds; -o json keeps the full timestamp.
const timeFormat = "2006-01-02 15:04"

// logbookFormat keeps seconds, because a busy sensor can produce many changes
// within one minute and identical times hide the order.
const logbookFormat = "2006-01-02 15:04:05"

// table writes space-aligned columns, truncating each cell to its width so one
// long value cannot stretch the whole table. Width 0 means no limit.
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
	// tabwriter pads even an empty last column; trim it for clean piped output.
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

// truncate shortens s to at most n runes, marking the cut with an ellipsis.
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

// pad makes s exactly n runes wide. fmt's %-13s pads by bytes, so truncated
// multi-byte names shifted their column; this never does.
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
