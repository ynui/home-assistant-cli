package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

var (
	flagURL     = flag.String("url", "", "override configured server URL")
	flagToken   = flag.String("token", "", "override configured long-lived token")
	flagOutput  = flag.String("o", "table", "output format: table or json")
	flagTimeout = flag.Duration("timeout", 30*time.Second, "per-request timeout")
)

const usageText = `has - a small command-line client for Home Assistant

usage: has [global flags] <command> [args]

commands:
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

list flags: --sort entity|name|state|changed|updated, --reverse, --limit N.
on/off/toggle flag: --all.
logbook flags: --day today|yesterday|YYYY-MM-DD, --limit N.
watch and template print their own output: a stream of changes, and whatever the
template rendered.

config lives in ~/.config/has/config.json. HAS_URL, HAS_TOKEN and HAS_CONFIG
override it; --url and --token override those.
`

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
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
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "has: "+err.Error())
	os.Exit(1)
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
		return fmt.Errorf("bad -o %q: want table or json", *flagOutput)
	}
	return nil
}

// parseArgs splits args into flags and positionals before handing them to a
// subcommand's FlagSet. The stdlib parser stops at the first positional, which
// would silently ignore `has history light.x --since 5m`.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		if len(a) > 1 && strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			// Pull in the value for a flag written as `-flag value`.
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		rest = append(rest, a)
	}
	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	if err := checkOutputFormat(); err != nil {
		return nil, err
	}
	return append(fs.Args(), rest...), nil
}

func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
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
