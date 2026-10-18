package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
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
	// An existing token is offered as a default, not reused silently:
	// re-running `has login` is how you replace a revoked token.
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
		// 401 is the common case: point at the fix.
		if exitCode(err) == exitAuth {
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
	area := fs.String("area", "", "restrict to entities whose area matches")
	tokens, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	c, err := configured()
	if err != nil {
		return err
	}
	matched, err := scopedMatch(c, *area, tokens, true)
	if err != nil {
		return err
	}

	// HA returns states in an internal order that looks random; sorting by
	// entity id groups domains together.
	by, err := stateSort(*sortBy)
	if err != nil {
		return err
	}
	sort.SliceStable(matched, func(i, j int) bool { return by(&matched[i], &matched[j]) })
	if *reverse {
		for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
			matched[i], matched[j] = matched[j], matched[i]
		}
	}
	// Counts come from the full match, before --limit truncates.
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
	// Show the timestamp sorted on, so the column and the order always agree.
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
		rows = append(rows, []string{s.EntityID, friendlyName(&s), s.State, when})
	}

	// Group headers only under the default sort: under any other sort the
	// rows are not in domain order.
	if *sortBy == "entity" {
		rows = withGroupHeaders(rows, counts)
	}
	return table(os.Stdout,
		[]string{"ENTITY", "NAME", "STATE", col},
		[]int{50, 28, 24, len(timeFormat)},
		rows)
}

// briefState is what `list --brief -o json` emits: the table's three columns,
// nothing else. The full state is mostly unread attributes and two timestamps
// that differ by microseconds; `list -o json` keeps it as "everything the
// server said".
type briefState struct {
	EntityID string `json:"entity_id"`
	Name     string `json:"name"`
	State    string `json:"state"`
}

// briefStates projects states onto briefState, keeping the order they arrive in.
func briefStates(states []State) []briefState {
	out := make([]briefState, 0, len(states))
	for _, s := range states {
		out = append(out, briefState{s.EntityID, friendlyName(&s), s.State})
	}
	return out
}

// withGroupHeaders inserts a "domain (n)" line wherever the domain changes,
// counting from counts so they describe the whole domain, not just the rows
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
		[][]string{{s.EntityID, friendlyName(&s), s.State, timestamp(s.LastChanged)}})
}

// stateSort returns a "less" function for a --sort key.
func stateSort(key string) (func(a, b *State) bool, error) {
	switch key {
	case "entity":
		return func(a, b *State) bool { return a.EntityID < b.EntityID }, nil
	case "name":
		return func(a, b *State) bool { return friendlyName(a) < friendlyName(b) }, nil
	case "state":
		return func(a, b *State) bool { return a.State < b.State }, nil
	case "changed":
		return func(a, b *State) bool { return a.LastChanged.Before(b.LastChanged) }, nil
	case "updated":
		return func(a, b *State) bool { return a.LastUpdated.Before(b.LastUpdated) }, nil
	default:
		return nil, coded{exitUsage, fmt.Errorf("bad --sort %q: want entity, name, state, changed or updated", key)}
	}
}

// friendlyName takes a pointer: it is called from sort comparators.
func friendlyName(s *State) string {
	if v, ok := s.Attributes["friendly_name"].(string); ok {
		return v
	}
	return ""
}

// filterArea narrows states to entities whose area name matches every token.
// A scope, not a selector: resolution has already answered elsewhere.
func filterArea(states []State, areas map[string]string, tok string) []State {
	var kept []State
	for _, s := range states {
		if matchAll([]string{areas[s.EntityID]}, []string{tok}) {
			kept = append(kept, s)
		}
	}
	return kept
}

// refuseArea applies the write rules to a scope that resolved to n entities:
// --area is opt-in, so acting on everything it holds stays as explicit as
// acting on any fuzzy group.
func refuseArea(n int, area string) error {
	if n == 0 {
		return coded{exitSelector, fmt.Errorf("no entity is in an area matching %q; run \"has areas\" to list them", area)}
	}
	return coded{exitSelector, fmt.Errorf("area %q contains %d entities; add --all to act on all of them", area, n)}
}

// scopedMatch resolves selectors and then, when area is set, keeps only the
// slice that lives in the named area: rest==0 with --area means everything in
// the area. all is the command's write rule: writes refuse a scope of more
// than one unless the caller passed --all, reads take everything.
func scopedMatch(c *Client, area string, tokens []string, all bool) ([]State, error) {
	states, err := c.states()
	if err != nil {
		return nil, err
	}
	var matched []State
	if len(tokens) == 0 {
		matched = states
	} else if matched, err = resolve(states, tokens, all); err != nil {
		return nil, err
	}
	if area == "" {
		return matched, nil
	}
	areas, err := c.entityAreas()
	if err != nil {
		return nil, err
	}
	kept := filterArea(matched, areas, area)
	if len(kept) != 1 {
		if !all {
			return nil, refuseArea(len(kept), area)
		}
		if len(kept) == 0 {
			return nil, refuseArea(0, area)
		}
	}
	return kept, nil
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

// cmdCall is `has call domain.service [entity...] key=value ...` and, when
// name is set, the entity-list form behind on/off/toggle.
func cmdCall(args []string, name, dom, service string) error {
	fs := newFlagSet(name)
	all := fs.Bool("all", false, "act on every entity a group or fuzzy name matches")
	dry := fs.Bool("dry-run", false, "resolve and print what would be called, without calling it")
	area := fs.String("area", "", "restrict to entities whose area matches")
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
			return coded{exitUsage, errors.New("usage: has call <domain.service> [entity...] [key=value ...]")}
		}
		d, s, ok := strings.Cut(rest[0], ".")
		if !ok || d == "" || s == "" {
			return coded{exitUsage, fmt.Errorf("expected domain.service, got %q", rest[0])}
		}
		dom, service = d, s
		// Everything after the service is either key=value or an entity
		// selector like every other command's: `has call light.turn_on
		// "Living Room Lamp" brightness=187` names the entities in position
		// and attributes wherever they fall, so no flag/arg order rule to
		// learn. k=v tokens stay in the payload.
		var fields, sels []string
		for _, a := range rest[1:] {
			if strings.Contains(a, "=") {
				fields = append(fields, a)
			} else {
				sels = append(sels, a)
			}
		}
		data, err = parseFields(fields)
		if err != nil {
			return err
		}
		if len(sels) > 0 {
			states, err := c.states()
			if err != nil {
				return err
			}
			matched, err := resolve(states, sels, *all)
			if err != nil {
				return err
			}
			ids := entityIDs(matched)
			// Merge with an entity_id already in the payload rather than
			// overwrite it: both spellings may name entities at once.
			if s, _ := data["entity_id"].(string); s != "" {
				for _, t := range strings.Split(s, ",") {
					if t = strings.TrimSpace(t); t != "" {
						ids = append(ids, t)
					}
				}
			}
			data["entity_id"] = ids
		}
		// An entity_id in the payload is a selector like any other.
		if err := resolveTargets(c, data, *all); err != nil {
			return err
		}
		// --area scopes what the payload ended up naming. Kept empty, no
		// named entity lives in the area; refusing cannot fire at anything.
		if *area != "" {
			areas, err := c.entityAreas()
			if err != nil {
				return err
			}
			var kept []string
			for _, key := range targetKeys {
				s, _ := data[key].(string)
				for _, tok := range strings.Split(s, ",") {
					if tok = strings.TrimSpace(tok); tok != "" &&
						matchAll([]string{areas[tok]}, []string{*area}) {
						kept = append(kept, tok)
					}
				}
				delete(data, key)
			}
			if len(kept) != 1 && !*all {
				return refuseArea(len(kept), *area)
			}
			data["entity_id"] = strings.Join(kept, ",")
		}
	} else {
		if len(rest) == 0 && *area == "" {
			return coded{exitUsage, fmt.Errorf("usage: has %s <entity...>", name)}
		}
		matched, err := scopedMatch(c, *area, rest, *all)
		if err != nil {
			return err
		}
		// Warn about the blast radius only when --all was used implicitly.
		if *all && len(matched) > 1 {
			fmt.Fprintf(os.Stderr, "%s will act on %d entities\n", name, len(matched))
		}
		data = map[string]any{"entity_id": entityIDs(matched)}
	}

	// Everything above resolves; nothing below has fired. A dry run stops
	// here, so it costs no extra request and still shows the ids the selector
	// really matched.
	if *dry {
		return reportDryRun(dom, service, data)
	}

	changed, err := c.call(dom, service, data)
	if err != nil {
		return err
	}
	// The response carries the resulting states, so print what happened.
	// This is stdout; warnings stay on stderr for scripts.
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

// reportDryRun prints the service and the payload it would be sent. Values are
// printed as they stand: the resolved form is the honest answer.
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

// payloadValue renders one payload value; []string values are comma-joined, so
// the output can be pasted straight back into another call.
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
			return nil, coded{exitUsage, fmt.Errorf("expected key=value, got %q — wrap a multi-word entity name in quotes", a)}
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
	area := fs.String("area", "", "restrict to entities whose area matches")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 && *area == "" {
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
	// Selectors resolve like every other command. Sending the token verbatim
	// used to make HA answer with an empty list, so a fuzzy name looked like
	// an entity that never changed.
	matched, err := scopedMatch(c, *area, rest, true)
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

// cmdWatch tails state changes off the websocket. There is no polling
// fallback: a tail with no socket has nothing to keep fresh, so a refused
// connection is an error.
func cmdWatch(args []string) error {
	fs := newFlagSet("watch")
	area := fs.String("area", "", "tail only entities whose area matches")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	c, err := configured()
	if err != nil {
		return err
	}
	var only map[string]bool
	if len(rest) > 0 || *area != "" {
		// Resolved once, up front; never re-resolved per event.
		matched, err := scopedMatch(c, *area, rest, true)
		if err != nil {
			return err
		}
		only = map[string]bool{}
		for _, id := range entityIDs(matched) {
			only[id] = true
		}
	}
	emit := watchPrinter(only)

	for tries := 0; ; tries++ {
		connected, err := wsStream(ctx, c, *flagTimeout, "", emit)
		if err == nil || ctx.Err() != nil {
			return nil
		}
		// A server that was never reachable gets the few tries a refused
		// handshake gets; one that dropped mid-tail keeps reconnecting.
		if connected {
			tries = 0
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

// watchEntityWidth is the entity column of `has watch`, wide enough for real
// ids; longer names are truncated.
const watchEntityWidth = 44

// watchNameWidth matches the NAME column of `has list`, so the two tables read
// as the same list.
const watchNameWidth = 28

// watchChange is one state_changed event. The two states are pointers because
// null is normal data here: new_state is null when the entity was removed,
// old_state is null on an entity's first state after startup.
type watchChange struct {
	EntityID string `json:"entity_id"`
	OldState *State `json:"old_state"`
	NewState *State `json:"new_state"`
}

// watchLine is one row of `has watch`.
type watchLine struct {
	When   time.Time `json:"when"`
	Entity string    `json:"entity_id"`
	Name   string    `json:"name,omitempty"`
	From   string    `json:"from"`
	To     string    `json:"to"`
}

// watchPrinter turns the event stream into printed lines, so the socket side
// stays a stream of raw events.
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
			// HA's own words for the two ends of an entity's life.
			From: "unset",
			To:   "removed",
		}
		if ch.OldState != nil {
			line.From = ch.OldState.State
		}
		// The name rides along in the event's attributes; a tail that shows ids
		// only makes the reader run `has list` to translate them.
		if ch.NewState != nil {
			line.To = ch.NewState.State
			line.Name = friendlyName(ch.NewState)
			// The server's timestamp, not write time: a reconnect can deliver
			// an old event.
			line.When = ch.NewState.LastUpdated
		} else if ch.OldState != nil {
			line.When = ch.OldState.LastChanged
			line.Name = friendlyName(ch.OldState)
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
				pad("ENTITY", watchEntityWidth)+"  "+pad("NAME", watchNameWidth)+"  CHANGE")
		}
		// pad(), not a table: rows arrive one at a time and tabwriter would
		// never see the next row to align against.
		fmt.Fprintln(os.Stdout, pad(timestampSec(line.When), len(logbookFormat))+"  "+
			pad(line.Entity, watchEntityWidth)+"  "+
			pad(line.Name, watchNameWidth)+"  "+line.From+" → "+line.To)
	}
}

// ---------------------------------------------------------------- areas

// cmdAreas groups entities by area. Like the TUI's areas view, it reads the
// one template render: HA keeps areas out of /api/states. Entities with no
// area are not listed; an "unknown" row answers nothing.
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
		// When the args name an area, that reading wins: `has areas living`
		// is the whole Living Room, including entities whose names share
		// none of the token — the dresser lamp is not "living"-named, and a
		// token that also matches six living_room entities must not shadow
		// the area it names. Only when no area matches do the entity
		// selectors decide, as before.
		for _, s := range all {
			if a := areas[s.EntityID]; a != "" && matchAll([]string{a}, rest) {
				matched = append(matched, s)
			}
		}
		if len(matched) == 0 {
			var rerr error
			matched, rerr = resolve(all, rest, true)
			if rerr != nil {
				return rerr
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

// cmdLogbook answers "what just happened" for the whole house, including
// automations and scripts that have no state; `history` is the same idea
// narrowed to named entities and a window.
func cmdLogbook(args []string) error {
	fs := newFlagSet("logbook")
	day := fs.String("day", "today", "which day: today, yesterday or YYYY-MM-DD")
	limit := fs.Int("limit", 50, "maximum number of entries to show, 0 for all")
	area := fs.String("area", "", "restrict to entities whose area matches")
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
	// Selectors resolve like every other command; --area scopes them. With
	// --area and no selector, the snapshot itself is scoped to the area
	// instead of the whole house.
	var targets []string
	if len(rest) > 0 || *area != "" {
		matched, err := scopedMatch(c, *area, rest, true)
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
		rows = append(rows, []string{s.Domain, s.Name, strings.Join(s.entityDomains(), ","), s.Description})
	}
	if err := table(os.Stdout,
		[]string{"DOMAIN", "SERVICE", "TARGETS", "DESCRIPTION"},
		[]int{16, 26, 20, 50},
		rows); err != nil {
		return err
	}
	// One match means the question has moved from "which one" to "what does
	// it take", and the answer is already in the rows above: the schema came
	// with them. Many matches would put a field list after every row.
	if len(out) == 1 {
		return serviceFields(os.Stdout, out[0])
	}
	return nil
}

// serviceFields prints what one service accepts. Home Assistant's registry
// carries a description, an example and whether the field is required; it does
// not carry types or ranges, and reading one out of the example would be
// inventing a schema the server never sent.
func serviceFields(w io.Writer, s Service) error {
	if len(s.Fields) == 0 {
		return nil
	}
	// The gap goes here rather than at the call site: a service that takes no
	// fields printed an empty line after its table.
	fmt.Fprintln(w)
	rows := make([][]string, 0, len(s.Fields))
	// Sorted: the registry is a map, and two runs should print the same lines.
	for _, name := range slices.Sorted(maps.Keys(s.Fields)) {
		f := s.Fields[name]
		required, example := "-", "-"
		if f.Required {
			required = "required"
		}
		if f.Example != nil {
			example = fmt.Sprint(f.Example)
		}
		rows = append(rows, []string{name, required, example, f.Description})
	}
	return table(w,
		[]string{"FIELD", "REQUIRED", "EXAMPLE", "DESCRIPTION"},
		[]int{30, 9, 18, 50},
		rows)
}

// ---------------------------------------------------------------- template

// cmdTemplate renders a Jinja template on the server and prints the result
// exactly as it came back - the only way to ask HA a question it has no
// endpoint for. The output is deliberately not wrapped: a template can render
// a number, a yaml document or one word, and wrapping would break `| jq`.
func cmdTemplate(args []string) error {
	fs := newFlagSet("template")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	tpl := strings.Join(rest, " ")
	if tpl == "" {
		// No arguments means stdin, so a template can be piped in.
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
	// ModeCharDevice is also true for /dev/null; stty is the real test.
	return stty("-g") == nil
}

// stty must be given os.Stdin explicitly: os/exec wires a nil Stdin to
// /dev/null, which makes every call fail.
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

// promptSecret reads a line without echoing it (stty; no terminal library).
// If echo cannot be turned off it refuses rather than print the token in
// cleartext; echo is restored on every exit path.
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
