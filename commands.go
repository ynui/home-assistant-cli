package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

var stdin = bufio.NewReader(os.Stdin)

// ---------------------------------------------------------------- login

func cmdLogin(args []string) error {
	fs := newFlagSet("login")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}

	saved, err := loadConfig()
	if err != nil {
		return err
	}
	url, token := *flagURL, *flagToken
	if url == "" {
		url = saved.URL
	}
	if token == "" {
		token = saved.Token
	}
	if !isTTY() {
		return login(url, token)
	}

	// Prompt only for what is still missing; supplied values are not re-asked.
	if url == "" {
		url = prompt("Home Assistant URL", "http://homeassistant.local:8123")
	}
	// An existing token is offered as a default rather than reused silently:
	// re-running `has login` is how you replace a token that has been revoked.
	token, err = promptSecret("Long-lived token", token)
	if err != nil {
		return err
	}
	return login(url, token)
}

func login(url, token string) error {
	url = normalizeURL(url)
	if url == "" || token == "" {
		return errors.New("both a url and a token are required")
	}

	cfg := Config{URL: url, Token: token}
	c, err := newClient(cfg, *flagTimeout)
	if err != nil {
		return err
	}
	if err := c.ping(); err != nil {
		// A bad token is the common case, so point at the fix.
		if strings.Contains(err.Error(), "Unauthorized") || strings.Contains(err.Error(), "401") {
			return fmt.Errorf("%s rejected the token: %w\n  generate a new one at <url>/profile, then run `has login`", url, err)
		}
		return fmt.Errorf("cannot use %s: %w", url, err)
	}

	info, err := c.serverConfig()
	if err != nil {
		return err
	}
	states, _ := c.states() // non-fatal: version alone proves the token works
	name := info.LocationName
	if name == "" {
		name = "Home Assistant"
	}
	fmt.Fprintf(os.Stderr, "connected: %s %s, %d entities\n", name, info.Version, len(states))

	p, _ := configPath()
	if err := saveConfig(cfg); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "saved "+p)
	return nil
}

// normalizeURL adds a scheme if the user forgot one and trims trailing slashes.
func normalizeURL(s string) string {
	s = strings.TrimSpace(strings.TrimRight(s, "/"))
	if s == "" || strings.Contains(s, "://") {
		return s
	}
	return "http://" + s
}

// ---------------------------------------------------------------- list

func cmdList(args []string) error {
	fs := newFlagSet("list")
	sortBy := fs.String("sort", "entity", "sort by: entity, name, state, changed, updated")
	reverse := fs.Bool("reverse", false, "reverse the sort order")
	limit := fs.Int("limit", 0, "show at most this many entities, 0 for all")
	brief := fs.Bool("brief", false, "with -o json, emit only entity_id, name and state (the table's columns)")
	tokens, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	c, err := configured()
	if err != nil {
		return err
	}
	states, err := c.states()
	if err != nil {
		return err
	}
	var matched []State
	if len(tokens) == 0 {
		matched = states
	} else if matched, err = resolve(states, tokens, true); err != nil {
		return err
	}

	// Home Assistant returns states in internal order, which looks random.
	// Sorting by entity_id groups related entities and domains together.
	by, err := stateSort(*sortBy)
	if err != nil {
		return err
	}
	sort.SliceStable(matched, func(i, j int) bool { return by(matched[i], matched[j]) })
	if *reverse {
		for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
			matched[i], matched[j] = matched[j], matched[i]
		}
	}
	// Counts come from the full match, not the truncated view, so a group
	// header never claims a domain is smaller than it is.
	counts := domainCounts(matched)
	if *limit > 0 && len(matched) > *limit {
		matched = matched[:*limit]
	}

	if *flagOutput == "json" {
		if *brief {
			return emitJSON(briefStates(matched))
		}
		return emitJSON(matched)
	}
	// Show the timestamp that was actually sorted on, so the column and the
	// ordering never disagree. A text sort has no meaningful time column.
	col := ""
	switch *sortBy {
	case "changed":
		col = "CHANGED"
	case "updated":
		col = "UPDATED"
	}
	rows := make([][]string, 0, len(matched))
	for _, s := range matched {
		when := ""
		switch *sortBy {
		case "changed":
			when = timestamp(s.LastChanged)
		case "updated":
			when = timestamp(s.LastUpdated)
		}
		rows = append(rows, []string{s.EntityID, friendlyName(s), s.State, when})
	}

	// Only under the default sort: grouping by domain under a time or text
	// sort would imply an order the rows are not in.
	if *sortBy == "entity" {
		rows = withGroupHeaders(rows, counts)
	}
	return table(os.Stdout,
		[]string{"ENTITY", "NAME", "STATE", col},
		[]int{50, 28, 24, len(timeFormat)},
		rows)
}

// briefState is what `list --brief -o json` emits: the three columns the table
// already shows, and nothing else.
//
// The full state is mostly things a reader did not ask for — attributes are
// half the payload on a real instance, and every row carries two ISO
// timestamps that differ by microseconds. `list -o json` keeps them because
// "everything the server said" is a fair thing to want; this is the shape for
// asking "which lights are on".
type briefState struct {
	EntityID string `json:"entity_id"`
	Name     string `json:"name"`
	State    string `json:"state"`
}

// briefStates projects states onto briefState, keeping the order they arrive in.
func briefStates(states []State) []briefState {
	out := make([]briefState, 0, len(states))
	for _, s := range states {
		out = append(out, briefState{s.EntityID, friendlyName(s), s.State})
	}
	return out
}

// withGroupHeaders inserts a "domain (n)" line wherever the domain changes.
// Counts come from counts so they describe the whole domain, not just the rows
// that survived --limit.
func withGroupHeaders(rows [][]string, counts map[string]int) [][]string {
	out := make([][]string, 0, len(rows)+8)
	prev := ""
	for _, r := range rows {
		d := domain(r[0])
		if d != prev {
			out = append(out, []string{fmt.Sprintf("%s (%d)", d, counts[d]), "", "", ""})
			prev = d
		}
		out = append(out, r)
	}
	return out
}

// cmdGroups lists the domains available as group selectors.
func cmdGroups(args []string) error {
	fs := newFlagSet("groups")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	c, err := configured()
	if err != nil {
		return err
	}
	states, err := c.states()
	if err != nil {
		return err
	}
	counts := groupCounts(states)
	if *flagOutput == "json" {
		return emitJSON(counts)
	}
	rows := make([][]string, 0, len(counts))
	for _, g := range counts {
		rows = append(rows, []string{g.Domain, strconv.Itoa(g.Count)})
	}
	return table(os.Stdout, []string{"DOMAIN", "COUNT"}, []int{30, 8}, rows)
}

// cmdGet shows exactly one entity. The name promises a single object, so an
// ambiguous name is an error rather than a list.
func cmdGet(args []string) error {
	fs := newFlagSet("get")
	tokens, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return coded{exitUsage, errors.New("usage: has get <entity>")}
	}
	c, err := configured()
	if err != nil {
		return err
	}
	states, err := c.states()
	if err != nil {
		return err
	}
	// get has no --all, so resolve freely and apply its own stricter rule.
	matched, err := resolve(states, tokens, true)
	if err != nil {
		return err
	}
	if len(matched) != 1 {
		return fmt.Errorf("%s matches %d entities, but get takes exactly one:\n%s\nname it by its full entity id",
			strings.Join(tokens, " "), len(matched), candidates(matched))
	}
	s := matched[0]
	if *flagOutput == "json" {
		return emitJSON(s)
	}
	return table(os.Stdout,
		[]string{"ENTITY", "NAME", "STATE", "CHANGED"},
		[]int{50, 28, 24, len(timeFormat)},
		[][]string{{s.EntityID, friendlyName(s), s.State, timestamp(s.LastChanged)}})
}

// stateSort returns a "less" function for a --sort key.
func stateSort(key string) (func(a, b State) bool, error) {
	switch key {
	case "entity":
		return func(a, b State) bool { return a.EntityID < b.EntityID }, nil
	case "name":
		return func(a, b State) bool { return friendlyName(a) < friendlyName(b) }, nil
	case "state":
		return func(a, b State) bool { return a.State < b.State }, nil
	case "changed":
		return func(a, b State) bool { return a.LastChanged.Before(b.LastChanged) }, nil
	case "updated":
		return func(a, b State) bool { return a.LastUpdated.Before(b.LastUpdated) }, nil
	default:
		return nil, coded{exitUsage, fmt.Errorf("bad --sort %q: want entity, name, state, changed or updated", key)}
	}
}

func friendlyName(s State) string {
	if v, ok := s.Attributes["friendly_name"].(string); ok {
		return v
	}
	return ""
}

// matchAll reports whether every needle appears in at least one of the fields.
func matchAll(fields, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		found := false
		for _, f := range fields {
			if strings.Contains(strings.ToLower(f), strings.ToLower(p)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- service calls

// cmdCall is both `has call domain.service k=v` and, when name is set, the
// entity-list form behind on/off/toggle. The on/off/toggle form resolves its
// arguments as selectors; call keeps literal ids as the escape hatch.
func cmdCall(args []string, name, dom, service string) error {
	fs := newFlagSet(name)
	all := fs.Bool("all", false, "act on every entity a group or fuzzy name matches")
	dry := fs.Bool("dry-run", false, "resolve and print what would be called, without calling it")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	c, err := configured()
	if err != nil {
		return err
	}

	var data map[string]any
	if name == "call" {
		if len(rest) == 0 {
			return coded{exitUsage, errors.New("usage: has call <domain.service> [key=value ...]")}
		}
		d, s, ok := strings.Cut(rest[0], ".")
		if !ok || d == "" || s == "" {
			return coded{exitUsage, fmt.Errorf("expected domain.service, got %q", rest[0])}
		}
		dom, service = d, s
		data, err = parseFields(rest[1:])
		if err != nil {
			return err
		}
		// An entity_id in the payload is a selector like any other.
		if err := resolveTargets(c, data, *all); err != nil {
			return err
		}
	} else {
		if len(rest) == 0 {
			return coded{exitUsage, fmt.Errorf("usage: has %s <entity...>", name)}
		}
		states, err := c.states()
		if err != nil {
			return err
		}
		matched, err := resolve(states, rest, *all)
		if err != nil {
			return err
		}
		// Only report the blast radius when it was asked for; naming two
		// entity ids is not a surprise worth a line.
		if *all && len(matched) > 1 {
			fmt.Fprintf(os.Stderr, "%s will act on %d entities\n", name, len(matched))
		}
		data = map[string]any{"entity_id": entityIDs(matched)}
	}

	// Everything above resolves; nothing below has fired. A dry run stops here,
	// so it costs no extra request and still reports the ids the selector
	// actually matched, which is the question it exists to answer.
	if *dry {
		return reportDryRun(dom, service, data)
	}

	changed, err := c.call(dom, service, data)
	if err != nil {
		return err
	}
	// Home Assistant reports the resulting states with the call, so say what
	// happened rather than making the user follow up with `has get`. This is
	// the command's output and goes to stdout; stderr keeps the warnings, so
	// `has on kitchen > what-happened` is a complete record.
	if *flagOutput == "json" {
		return emitJSON(changed)
	}
	if len(changed) == 0 {
		fmt.Printf("called %s.%s\n", dom, service)
		return nil
	}
	for _, s := range changed {
		fmt.Printf("%s → %s\n", s.EntityID, s.State)
	}
	return nil
}

// reportDryRun prints the service and the payload it would be sent, without
// calling anything. The payload is printed as it stands rather than assuming
// its values are entity ids: entity_id is not always an entity id, and the
// resolved value is the honest answer.
func reportDryRun(dom, service string, data map[string]any) error {
	full := dom + "." + service
	if *flagOutput == "json" {
		return emitJSON(map[string]any{"service": full, "data": data})
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("would call %s\n", full)
	for _, k := range keys {
		fmt.Printf("  %s: %s\n", k, payloadValue(data[k]))
	}
	return nil
}

// payloadValue renders one payload value: the string a key=value argument
// produced, or the comma-joined ids a selector resolved to — which is the form
// that can be pasted straight back into another call.
func payloadValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []string:
		return strings.Join(t, ",")
	}
	return fmt.Sprint(v)
}

// parseSince accepts the Go duration units plus d and w, because "7d" is what
// people actually type and time.ParseDuration rejects it.
func parseSince(s string) (time.Duration, error) {
	bad := func() error {
		return coded{exitUsage, fmt.Errorf("bad --since %q: want something like 30m, 12h or 7d", s)}
	}
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	if strings.HasSuffix(s, "w") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "w")); err == nil && n > 0 {
			return time.Duration(n) * 7 * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, bad()
	}
	if d <= 0 {
		return 0, coded{exitUsage, fmt.Errorf("--since must be positive, got %q", s)}
	}
	return d, nil
}

// parseFields turns key=value arguments into a service payload. The value is
// kept as a string; Home Assistant coerces it against the service schema.
func parseFields(args []string) (map[string]any, error) {
	out := map[string]any{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return nil, coded{exitUsage, fmt.Errorf("expected key=value, got %q", a)}
		}
		out[k] = v
	}
	return out, nil
}

// ---------------------------------------------------------------- history

func cmdHistory(args []string) error {
	fs := newFlagSet("history")
	since := fs.String("since", "24h", "how far back to look, e.g. 30m, 12h, 7d")
	limit := fs.Int("limit", 50, "maximum number of changes to show, 0 for all")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return coded{exitUsage, errors.New("usage: has history <entity...> [--since 24h]")}
	}
	d, err := parseSince(*since)
	if err != nil {
		return err
	}
	from := time.Now().Add(-d)

	c, err := configured()
	if err != nil {
		return err
	}
	all, err := c.states()
	if err != nil {
		return err
	}
	// Same selector rules as every other command. Sending the token verbatim
	// used to make Home Assistant answer with an empty list, so a typo or a
	// fuzzy name looked exactly like an entity that never changed.
	matched, err := resolve(all, rest, true)
	if err != nil {
		return err
	}
	states, err := c.history(entityIDs(matched), from)
	if err != nil {
		return err
	}
	// ponytail: trim after fetching. HA has no server-side limit param, and a
	// bounded fetch means tracking the last-seen timestamp ourselves.
	if *limit > 0 && len(states) > *limit {
		states = states[len(states)-*limit:]
	}
	if *flagOutput == "json" {
		return emitJSON(states)
	}
	rows := make([][]string, 0, len(states))
	prev := ""
	for _, s := range states {
		// A run of one entity's changes reads better without the id repeated
		// on every line; the state column is what you are reading.
		id := s.EntityID
		if id == prev {
			id = ""
		}
		prev = s.EntityID
		rows = append(rows, []string{id, s.State, timestamp(s.LastChanged)})
	}
	return table(os.Stdout,
		[]string{"ENTITY", "STATE", "CHANGED"},
		[]int{50, 24, len(timeFormat)},
		rows)
}

// ---------------------------------------------------------------- watch

// cmdWatch tails state changes off the websocket: the live answer to what
// logbook answers about yesterday. Selectors narrow it the same way every other
// read does, and there is nothing to fall back to - a tail with no socket has no
// list to keep fresh, so a refused connection is an error rather than a slower
// mode.
func cmdWatch(args []string) error {
	fs := newFlagSet("watch")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	c, err := configured()
	if err != nil {
		return err
	}
	// A nil filter watches the whole house; anything else is a set of entity
	// ids resolved once, up front, so a selector is never re-resolved per event.
	var only map[string]bool
	if len(rest) > 0 {
		all, err := c.states()
		if err != nil {
			return err
		}
		matched, err := resolve(all, rest, true)
		if err != nil {
			return err
		}
		only = map[string]bool{}
		for _, id := range entityIDs(matched) {
			only[id] = true
		}
	}
	emit := watchPrinter(only)

	ctx := context.Background()
	for tries := 0; ; tries++ {
		connected, err := wsStream(ctx, c, *flagTimeout, "", emit)
		if err == nil || ctx.Err() != nil {
			return nil
		}
		// A server that was never reachable is a dead server, not a blip, so it
		// gets the few tries a refused handshake gets. One that went away
		// mid-tail keeps the tail: a house that restarts should not need a
		// restart.
		if connected {
			tries = 0 // it was up: this drop is not part of a server that is down
		} else if tries+1 >= wsConnectTries {
			return err
		}
		if connected {
			fmt.Fprintf(os.Stderr, "has: watch lost the connection (%v), reconnecting\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "has: cannot subscribe (%v), retrying\n", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wsBackoff(tries)):
		}
	}
}

// watchEntityWidth is the entity column of `has watch`. Long enough for the
// longest id a real house produces, truncated with the usual ellipsis beyond it.
const watchEntityWidth = 44

// watchChange is one state_changed event, in the shape the wire sends it. The
// two states are pointers because null is not an edge case here: new_state is
// null when the entity was removed, and old_state is null the first time an
// entity ever takes a state.
type watchChange struct {
	EntityID string `json:"entity_id"`
	OldState *State `json:"old_state"`
	NewState *State `json:"new_state"`
}

// watchLine is one row of `has watch`.
type watchLine struct {
	When   time.Time `json:"when"`
	Entity string    `json:"entity_id"`
	From   string    `json:"from"`
	To     string    `json:"to"`
}

// watchPrinter turns the event stream into the lines a tail prints. Only it and
// cmdWatch know what a change looks like on screen, so the socket side stays a
// stream of raw events.
func watchPrinter(only map[string]bool) func(eventType string, data json.RawMessage) {
	first := true
	return func(eventType string, data json.RawMessage) {
		if eventType != wsStateChanged {
			return
		}
		var ch watchChange
		if json.Unmarshal(data, &ch) != nil {
			return
		}
		if only != nil && !only[ch.EntityID] {
			return
		}
		line := watchLine{
			When:   time.Now(),
			Entity: ch.EntityID,
			// `unset` and `removed` are Home Assistant's own words for the two
			// ends of an entity's life, and saying so beats an empty column
			// that reads like a bug.
			From: "unset",
			To:   "removed",
		}
		if ch.OldState != nil {
			line.From = ch.OldState.State
		}
		if ch.NewState != nil {
			line.To = ch.NewState.State
			// The server's own timestamp, not the moment this line was written:
			// a reconnect can deliver an event that happened minutes ago.
			line.When = ch.NewState.LastUpdated
		} else if ch.OldState != nil {
			line.When = ch.OldState.LastChanged
		}
		if line.When.IsZero() {
			line.When = time.Now()
		}
		if *flagOutput == "json" {
			// One object per line, not one array: this is a stream, and a
			// consumer reading it cannot wait for an array that never ends.
			if b, err := json.Marshal(line); err == nil {
				fmt.Fprintln(os.Stdout, string(b))
			}
			return
		}
		if first {
			first = false
			fmt.Fprintln(os.Stdout, "  "+pad("WHEN", len(logbookFormat))+"  "+
				pad("ENTITY", watchEntityWidth)+"  CHANGE")
		}
		// pad(), not a table: a tail arrives one row at a time, so tabwriter
		// would never see the row after this one to align against.
		fmt.Fprintln(os.Stdout, pad(timestampSec(line.When), len(logbookFormat))+"  "+
			pad(line.Entity, watchEntityWidth)+"  "+line.From+" → "+line.To)
	}
}

// ---------------------------------------------------------------- areas

// cmdAreas groups entities by the area Home Assistant has them in. It is the
// same grouping the TUI's "areas" view shows, over the same one template
// render - Home Assistant keeps areas out of /api/states, so this is the only
// way to ask.
//
// Most entities have no area and are not listed: an "unknown" row holding 300
// entities answers nothing.
func cmdAreas(args []string) error {
	fs := newFlagSet("areas")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	c, err := configured()
	if err != nil {
		return err
	}
	areas, err := c.entityAreas()
	if err != nil {
		return err
	}
	all, err := c.states()
	if err != nil {
		return err
	}
	var matched []State
	switch len(rest) {
	case 0:
		matched = all
	default:
		matched, err = resolve(all, rest, true)
		if err != nil {
			// A token that names no entity may name an area. This command is
			// grouped by area and the area is what it prints, so `has areas
			// kitchen` should mean the kitchen rather than fail on a house with
			// no entity called kitchen.
			for _, s := range all {
				if a := areas[s.EntityID]; a != "" && matchAll([]string{a}, rest) {
					matched = append(matched, s)
				}
			}
			if len(matched) == 0 {
				return err
			}
		}
	}

	byArea := map[string][]State{}
	for _, s := range matched {
		if a := areas[s.EntityID]; a != "" {
			byArea[a] = append(byArea[a], s)
		}
	}
	names := make([]string, 0, len(byArea))
	for a := range byArea {
		names = append(names, a)
	}
	sort.Slice(names, func(i, j int) bool {
		if len(byArea[names[i]]) != len(byArea[names[j]]) {
			return len(byArea[names[i]]) > len(byArea[names[j]])
		}
		return names[i] < names[j]
	})

	if *flagOutput == "json" {
		out := make([]areaCount, 0, len(names))
		for _, a := range names {
			out = append(out, areaCount{Name: a, Entities: entityIDs(byArea[a])})
		}
		return emitJSON(out)
	}
	rows := make([][]string, 0, len(names))
	for _, a := range names {
		rows = append(rows, []string{a, strconv.Itoa(len(byArea[a]))})
	}
	return table(os.Stdout, []string{"AREA", "COUNT"}, []int{30, 8}, rows)
}

// areaCount is one row of `has areas`.
type areaCount struct {
	Name     string   `json:"area"`
	Entities []string `json:"entities"`
}

// ---------------------------------------------------------------- logbook

// cmdLogbook answers "what just happened", for a whole house or for whatever
// the selectors match. `history` is the same idea narrowed to entities you name
// and a window you choose; the logbook is what Home Assistant recorded about
// everything, including automations and scripts that have no state of their own.
func cmdLogbook(args []string) error {
	fs := newFlagSet("logbook")
	day := fs.String("day", "today", "which day: today, yesterday or YYYY-MM-DD")
	limit := fs.Int("limit", 50, "maximum number of entries to show, 0 for all")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	d, err := parseDay(*day)
	if err != nil {
		return err
	}
	c, err := configured()
	if err != nil {
		return err
	}
	// Same selector rules as every other command, so `has logbook kitchen`
	// means what `has list kitchen` means. A read, so many matches are fine.
	var targets []string
	if len(rest) > 0 {
		states, err := c.states()
		if err != nil {
			return err
		}
		matched, err := resolve(states, rest, true)
		if err != nil {
			return err
		}
		targets = entityIDs(matched)
	}

	entries, err := c.logbook(d, targets)
	if err != nil {
		return err
	}
	if *limit > 0 && len(entries) > *limit {
		// ponytail: trim after fetching. The endpoint takes a date and an
		// entity list, so there is nothing to ask for fewer.
		entries = entries[len(entries)-*limit:]
	}
	if *flagOutput == "json" {
		return emitJSON(entries)
	}
	rows := make([][]string, 0, len(entries))
	prev := ""
	for _, e := range entries {
		// A run of one entity's changes reads better without the id repeated on
		// every line; what changed is the column being read.
		id := e.EntityID
		if id == prev {
			id = ""
		}
		prev = e.EntityID
		rows = append(rows, []string{
			timestampSec(e.When), id, e.Name, e.what(),
		})
	}
	return table(os.Stdout,
		[]string{"WHEN", "ENTITY", "NAME", "CHANGE"},
		[]int{len(logbookFormat), 50, 28, 44},
		rows)
}

// parseDay accepts today, yesterday or a plain date: the logbook is a day-based
// endpoint, so there is no --since to speak of.
func parseDay(s string) (time.Time, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "today":
		return time.Now(), nil
	case "yesterday":
		return time.Now().AddDate(0, 0, -1), nil
	}
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad --day %q: want today, yesterday or YYYY-MM-DD", s)
	}
	return d, nil
}

// ---------------------------------------------------------------- services

func cmdServices(args []string) error {
	fs := newFlagSet("services")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	c, err := configured()
	if err != nil {
		return err
	}
	all, err := c.services()
	if err != nil {
		return err
	}
	out := make([]Service, 0, len(all))
	for _, s := range all {
		if matchAll([]string{s.Domain, s.Name, s.Description, s.Domain + "." + s.Name}, rest) {
			out = append(out, s)
		}
	}
	if *flagOutput == "json" {
		return emitJSON(out)
	}
	rows := make([][]string, 0, len(out))
	for _, s := range out {
		// Targets are what decide whether `has call` can aim this service at
		// anything, and the registry already parsed them.
		rows = append(rows, []string{s.Domain, s.Name, strings.Join(s.entityDomains(), ","), s.Description})
	}
	return table(os.Stdout,
		[]string{"DOMAIN", "SERVICE", "TARGETS", "DESCRIPTION"},
		[]int{16, 26, 20, 50},
		rows)
}

// ---------------------------------------------------------------- template

// cmdTemplate renders a Jinja template on the server and prints the result
// exactly as it came back. This is the same endpoint `has areas` reads, and the
// reason it exists is that a template is the only way to ask Home Assistant a
// question it has no endpoint for - what a sensor's unit says, what a script
// would return, whether a condition still holds.
//
// The output is deliberately not wrapped. A template can render a number, a
// yaml document or one word, and -o json says nothing about which: wrapping
// would break the `has template ... | jq` that makes it worth having.
func cmdTemplate(args []string) error {
	fs := newFlagSet("template")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	tpl := strings.Join(rest, " ")
	if tpl == "" {
		// No arguments means stdin, which is what makes a template usable in a
		// pipe or from an editor buffer without quoting it for the shell twice.
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		tpl = string(b)
	}
	if strings.TrimSpace(tpl) == "" {
		return coded{exitUsage, errors.New("usage: has template '<jinja>'  (or pipe a template in on stdin)")}
	}
	c, err := configured()
	if err != nil {
		return err
	}
	out, err := c.template(tpl)
	if err != nil {
		return err
	}
	fmt.Fprint(os.Stdout, out)
	return nil
}

// ---------------------------------------------------------------- prompting

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// ModeCharDevice is also true for /dev/null. stty is the ioctl test that
	// does not need a dependency.
	return stty("-g") == nil
}

// stty must be given os.Stdin explicitly: os/exec wires a child's stdin to
// /dev/null when Cmd.Stdin is nil, which would make every call fail.
func stty(args ...string) error {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func prompt(label, def string) string {
	if def != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return def // EOF
	}
	if !strings.HasSuffix(line, "\n") {
		fmt.Fprintln(os.Stderr) // keep our prompt on its own line
	}
	if line = strings.TrimSpace(line); line == "" {
		return def
	}
	return line
}

// promptSecret reads a line without echoing it. stty is the native tool here;
// a terminal library would be the only dependency in the project. If we cannot
// turn off echo we refuse rather than print the token in cleartext. Echo is
// restored on every exit path so a Ctrl-C cannot leave the terminal mute.
func promptSecret(label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(os.Stderr, "%s [press enter to keep current]: ", label)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}
	restore, err := noEcho()
	if err != nil {
		return "", err
	}
	defer restore()
	line, err := stdin.ReadString('\n')
	fmt.Fprintln(os.Stderr) // the newline we typed is not echoed
	if err != nil && line == "" {
		return def, nil
	}
	if line = strings.TrimSpace(line); line == "" {
		return def, nil
	}
	return line, nil
}

func noEcho() (func(), error) {
	if err := stty("-echo"); err != nil {
		return func() {}, fmt.Errorf("hiding input: %w", err)
	}
	return func() { _ = stty("echo") }, nil
}
