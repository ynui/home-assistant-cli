package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain points the config at a throwaway directory for the whole package.
//
// The UI writes ui.json — pinning a favorite, reordering a group, switching view
// all save it — and those saves are reachable from a plain `m.Update(...)`. Tests
// that forgot to redirect the config therefore wrote to the real
// ~/.config/has/ui.json, and a zero-valued uiConfig replaced the user's
// favorites and group order with `{"view": "..."}` every time the suite ran.
// Setting it once here means a test that forgets cannot reach the real file;
// a test that cares sets HAS_CONFIG itself with t.Setenv.
// The version is injected at build time and defaults to "dev" for a local
// build; a release sets it with -ldflags. Both spellings of the flag have to be
// registered, and both have to start false.
func TestVersionFlag(t *testing.T) {
	if *flagVersion || *flagVersion2 {
		t.Error("the version flags should start false")
	}
	for _, name := range []string{"v", "version"} {
		if flag.CommandLine.Lookup(name) == nil {
			t.Errorf("-%s is not registered", name)
		}
	}
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "has-test-*")
	if err != nil {
		panic(err)
	}
	os.Setenv("HAS_CONFIG", filepath.Join(dir, "config.json"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// TestSelfCheck covers the logic that can actually be wrong without a server:
// argument handling, URL normalisation, filtering, and column rendering.
func TestSelfCheck(t *testing.T) {
	t.Run("parseArgs", func(t *testing.T) {
		// Flags after positionals must still parse, and must not be mistaken
		// for entity ids.
		fs := testFlags()
		got, err := parseArgs(fs, []string{"light.x", "--since", "5m", "-limit", "10"})
		if err != nil {
			t.Fatal(err)
		}
		if s := fs.Lookup("since").Value.String(); s != "5m" {
			t.Errorf("since = %q, want 5m", s)
		}
		if fs.Lookup("limit").Value.String() != "10" {
			t.Errorf("limit = %q, want 10", fs.Lookup("limit").Value.String())
		}
		want := []string{"light.x"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("positionals = %v, want %v", got, want)
		}
	})

	t.Run("parseArgs after --", func(t *testing.T) {
		got, err := parseArgs(testFlags(), []string{"--", "-not-a-flag"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "-not-a-flag" {
			t.Errorf("got %v, want [-not-a-flag]", got)
		}
	})

	t.Run("normalizeURL", func(t *testing.T) {
		cases := map[string]string{
			"homeassistant.local:8123": "http://homeassistant.local:8123",
			"http://ha.local:8123/":    "http://ha.local:8123",
			"https://ha.example.com":   "https://ha.example.com",
			"":                         "",
		}
		for in, want := range cases {
			if got := normalizeURL(in); got != want {
				t.Errorf("normalizeURL(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("agent brief", func(t *testing.T) {
		// The brief is the one page an agent may read instead of guessing, so
		// the invariants it promises must still be the shipped behavior:
		// call's positional selectors, exact-name writes, field discovery, and
		// the two things an agent invents instead of checking (scenes and
		// parsing error text).
		out := captureStdout(t, func() {
			cmdAgent()
		})
		for _, want := range []string{
			"has call <domain.service> [entity...] [key=value ...]",
			"\"Dresser Lamp\"",
			"--dry-run",
			"has services <domain.service> -o json",
			"has call scene.turn_on entity_id=evening",
			"verbatim",
			"when, entity_id, name, from, to",
			"Exit codes:",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("agent brief lacks %q", want)
			}
		}
	})

	t.Run("parseFields", func(t *testing.T) {
		got, err := parseFields([]string{"entity_id=light.a", "brightness_pct=40"})
		if err != nil {
			t.Fatal(err)
		}
		if got["entity_id"] != "light.a" || got["brightness_pct"] != "40" {
			t.Errorf("got %v", got)
		}
		if _, err := parseFields([]string{"oops"}); err == nil {
			t.Error("want error for argument without =")
		}
		if _, err := parseFields([]string{"=x"}); err == nil {
			t.Error("want error for empty key")
		}
	})

	t.Run("matchAll", func(t *testing.T) {
		fields := []string{"light.kitchen_light", "Kitchen Light"}
		if !matchAll(fields, nil) {
			t.Error("no patterns should match everything")
		}
		if !matchAll(fields, []string{"kitchen"}) {
			t.Error("match on friendly name failed")
		}
		if !matchAll(fields, []string{"light", "KITCHEN"}) {
			t.Error("multiple patterns should all match, case-insensitively")
		}
		if matchAll(fields, []string{"light", "bedroom"}) {
			t.Error("a non-matching pattern should fail the whole entity")
		}
	})

	t.Run("domain", func(t *testing.T) {
		cases := map[string]string{
			"light.kitchen": "light", "binary_sensor.door": "binary_sensor",
			"nodot": "nodot", "": "",
		}
		for in, want := range cases {
			if got := domain(in); got != want {
				t.Errorf("domain(%q) = %q, want %q", in, got, want)
			}
		}
	})

	// Exact id beats group beats fuzzy; writes refuse an ambiguous match.
	t.Run("match", func(t *testing.T) {
		states := []State{
			{EntityID: "light.kitchen", Attributes: map[string]any{"friendly_name": "Kitchen"}},
			{EntityID: "light.kitchen_2", Attributes: map[string]any{"friendly_name": "Kitchen 2"}},
			{EntityID: "switch.kitchen_hob", Attributes: map[string]any{"friendly_name": "Hob"}},
			{EntityID: "light.garage", Attributes: map[string]any{"friendly_name": "Garage"}},
		}

		if got := match(states, "light.kitchen"); len(got) != 1 || got[0].EntityID != "light.kitchen" {
			t.Errorf("exact id: got %v", entityIDs(got))
		}
		if got := match(states, "light"); len(got) != 3 {
			t.Errorf("group: got %d, want 3", len(got))
		}
		if got := match(states, "lights"); len(got) != 3 {
			t.Errorf("plural group: got %d, want 3", len(got))
		}
		// "garage" is not a domain, so it falls through to fuzzy.
		if got := match(states, "garage"); len(got) != 1 || got[0].EntityID != "light.garage" {
			t.Errorf("fuzzy: got %v", entityIDs(got))
		}
		// Fuzzy on the friendly name, not just the id.
		if got := match(states, "Hob"); len(got) != 1 || got[0].EntityID != "switch.kitchen_hob" {
			t.Errorf("fuzzy by name: got %v", entityIDs(got))
		}
		if got := match(states, "kitchen"); len(got) != 3 {
			t.Errorf("fuzzy multi: got %d, want 3", len(got))
		}
		if got := match(states, "nonexistent"); len(got) != 0 {
			t.Errorf("no match: got %v", entityIDs(got))
		}
	})

	t.Run("resolve", func(t *testing.T) {
		states := []State{
			{EntityID: "light.kitchen"},
			{EntityID: "light.kitchen_2"},
			{EntityID: "switch.hob"},
			{EntityID: "light.garage"},
		}

		// Reads take many.
		got, err := resolve(states, []string{"light"}, true)
		if err != nil || len(got) != 3 {
			t.Errorf("read group: %d %v", len(got), err)
		}
		// Writes refuse an ambiguous group without --all.
		if _, err := resolve(states, []string{"light"}, false); err == nil {
			t.Error("write to a group should need --all")
		} else if !strings.Contains(err.Error(), "--all") {
			t.Errorf("error should mention --all: %v", err)
		}
		// A fuzzy name hitting several is equally refused.
		_, err = resolve(states, []string{"kitchen"}, false)
		if err == nil {
			t.Error("ambiguous fuzzy should error")
		}
		if !strings.Contains(err.Error(), "light.kitchen") {
			t.Errorf("error should list candidates: %v", err)
		}
		// An exact id is never ambiguous, so it needs no --all.
		if got, err := resolve(states, []string{"light.garage"}, false); err != nil || len(got) != 1 {
			t.Errorf("exact id: %v %v", got, err)
		}
		// Several exact ids are fine without --all.
		if got, err := resolve(states, []string{"light.garage", "switch.hob"}, false); err != nil || len(got) != 2 {
			t.Errorf("two exact ids: %v %v", got, err)
		}
		// Unknown token errors in both modes.
		if _, err := resolve(states, []string{"nope"}, true); err == nil {
			t.Error("unknown token should error")
		}
		// Overlapping selectors must not yield duplicates.
		got, err = resolve(states, []string{"light", "light.garage"}, true)
		if err != nil || len(got) != 3 {
			t.Errorf("overlap: got %v %v", entityIDs(got), err)
		}
	})

	t.Run("groupCounts", func(t *testing.T) {
		states := []State{
			{EntityID: "light.a"}, {EntityID: "light.b"},
			{EntityID: "switch.c"},
		}
		got := groupCounts(states)
		if len(got) != 2 {
			t.Fatalf("got %v", got)
		}
		if got[0].Domain != "light" || got[0].Count != 2 {
			t.Errorf("busiest first: %+v", got[0])
		}
	})

	t.Run("friendlyName", func(t *testing.T) {
		if got := friendlyName(&State{Attributes: map[string]any{"friendly_name": "Hall"}}); got != "Hall" {
			t.Errorf("got %q", got)
		}
		// Entities without the attribute must not render "(nil)".
		if got := friendlyName(&State{}); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("timestamp", func(t *testing.T) {
		if got := timestamp(time.Time{}); got != "" {
			t.Errorf("zero time should be blank, got %q", got)
		}
		// Rendered in local time, so compare against the same conversion.
		when := time.Date(2026, 4, 1, 12, 34, 56, 0, time.UTC)
		if got, want := timestamp(when), when.Local().Format(timeFormat); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if len(timestamp(when)) != len(timeFormat) {
			t.Errorf("seconds should be dropped: %q", timestamp(when))
		}
	})

	t.Run("unmarshalStates", func(t *testing.T) {
		flat := []byte(`[{"entity_id":"light.a","state":"on"}]`)
		got, err := unmarshalStates(flat)
		if err != nil || len(got) != 1 || got[0].State != "on" {
			t.Fatalf("flat: %v %v", got, err)
		}
		grouped := []byte(`[[{"entity_id":"light.a","state":"on"}],[{"entity_id":"light.a","state":"off"}]]`)
		got, err = unmarshalStates(grouped)
		if err != nil || len(got) != 2 {
			t.Fatalf("history shape: %v %v", got, err)
		}
	})

	t.Run("isTTY", func(t *testing.T) {
		// /dev/null is a character device, so a stat-only check wrongly says
		// "interactive" and makes `login` prompt when it should not.
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		old := os.Stdin
		os.Stdin = f
		defer func() { os.Stdin = old }()
		if isTTY() {
			t.Error("isTTY() = true for /dev/null, want false")
		}
	})

	t.Run("truncate", func(t *testing.T) {
		if got := truncate("on", 5); got != "on" {
			t.Errorf("short value changed: %q", got)
		}
		if got := truncate("2026-10-19T01:45:00+00:00", 8); got != "2026-10…" {
			t.Errorf("got %q, want %q", got, "2026-10…")
		}
		if got := truncate("abcdef", 0); got != "abcdef" {
			t.Errorf("zero width should not truncate: %q", got)
		}
		// Must not split a multi-byte rune in half.
		if got := truncate("מקררת", 3); got != "מק…" {
			t.Errorf("got %q, want %q", got, "מק…")
		}
	})

	t.Run("stateSort", func(t *testing.T) {
		a := State{EntityID: "light.b", State: "on",
			Attributes:  map[string]any{"friendly_name": "Zebra"},
			LastChanged: time.Unix(100, 0), LastUpdated: time.Unix(200, 0)}
		b := State{EntityID: "light.a", State: "off",
			Attributes:  map[string]any{"friendly_name": "Apple"},
			LastChanged: time.Unix(300, 0), LastUpdated: time.Unix(100, 0)}

		cases := []struct {
			key     string
			aFirst  bool // does a sort before b
			columns int  // trailing timestamp cells we expect to be filled
		}{
			{"entity", false, 0},
			{"name", false, 0},
			{"state", false, 0},
			{"changed", true, 1},
			{"updated", false, 1},
		}
		for _, tc := range cases {
			by, err := stateSort(tc.key)
			if err != nil {
				t.Fatalf("--sort %s: %v", tc.key, err)
			}
			if got := by(&a, &b); got != tc.aFirst {
				t.Errorf("--sort %s: a before b = %v, want %v", tc.key, got, tc.aFirst)
			}
		}
		if _, err := stateSort("nonsense"); err == nil {
			t.Error("want error for an unknown sort key")
		}
	})

	t.Run("parseSince", func(t *testing.T) {
		cases := map[string]time.Duration{
			"5m":  5 * time.Minute,
			"2h":  2 * time.Hour,
			"90s": 90 * time.Second,
			"7d":  7 * 24 * time.Hour,
			"1d":  24 * time.Hour,
			"2w":  14 * 24 * time.Hour,
			"24h": 24 * time.Hour,
		}
		for in, want := range cases {
			got, err := parseSince(in)
			if err != nil {
				t.Errorf("parseSince(%q): %v", in, err)
				continue
			}
			if got != want {
				t.Errorf("parseSince(%q) = %v, want %v", in, got, want)
			}
		}
		for _, bad := range []string{"yesterday", "0d", "-1d", "7", "d", "", "-5m"} {
			if _, err := parseSince(bad); err == nil {
				t.Errorf("parseSince(%q) should fail", bad)
			}
		}
	})

	t.Run("table", func(t *testing.T) {
		var buf bytes.Buffer
		err := table(&buf,
			[]string{"ENTITY", "STATE"},
			[]int{50, 5},
			[][]string{{"light.kitchen", "on"},
				{"light.an-entity-id-well-over-fifty-characters-long-indeed", "off"}})
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
		if len(lines) != 3 {
			t.Fatalf("got %d lines: %q", len(lines), buf.String())
		}
		if !strings.HasPrefix(lines[0], "ENTITY") || !strings.Contains(lines[0], "STATE") {
			t.Errorf("header not aligned: %q", lines[0])
		}
		if !strings.HasPrefix(lines[1], "light.kitchen") || !strings.Contains(lines[1], "on") {
			t.Errorf("row not aligned: %q", lines[1])
		}
		// A cell longer than its column must be cut, not widen the column.
		if strings.Contains(lines[2], "indeed") {
			t.Errorf("long entity id was not truncated: %q", lines[2])
		}
		if !strings.Contains(lines[2], "…") {
			t.Errorf("truncation should be marked: %q", lines[2])
		}
		// The ENTITY column must not have widened to fit the longest value.
		if w := len(lines[0]); w > 60 {
			t.Errorf("header is %d wide, columns were not capped: %q", w, lines[0])
		}
	})
}

// --area is a scope, not a selector: tokens resolve first (refusals and
// blast-radius rules intact), then only entities living in the named area
// survive. With --all: has on --area living means every entity in the area.
func TestAreaScopeFilter(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/template":
			io.WriteString(w, "light.living_lamp\tLiving Room\nlight.dresser_lamp\tLiving Room\n"+
				"media_player.tv\tLiving Room\nlight.hall_light\tHallway\n")
		case "/api/states":
			io.WriteString(w, `[
				{"entity_id":"light.living_lamp","state":"off","attributes":{"friendly_name":"Living Lamp"}},
				{"entity_id":"light.dresser_lamp","state":"off","attributes":{"friendly_name":"Dresser Lamp"}},
				{"entity_id":"media_player.tv","state":"idle","attributes":{"friendly_name":"TV"}},
				{"entity_id":"light.hall_light","state":"on","attributes":{"friendly_name":"Hall Light"}}]`)
			return
		}
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/services") {
			posts++
		}
		io.WriteString(w, `[]`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"url":"`+srv.URL+`","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAS_CONFIG", cfg)

	// Read intersection: selector "light" plus --area living keeps the two
	// lamps, not the TV.
	out := captureStdout(t, func() {
		if err := cmdList([]string{"light", "--area", "living"}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"light.living_lamp", "light.dresser_lamp"} {
		if !strings.Contains(out, want) {
			t.Errorf("lights in the area: %q", out)
		}
	}
	if strings.Contains(out, "media_player.tv") || strings.Contains(out, "light.hall_light") {
		t.Errorf("--area must scope outside and across domains: %q", out)
	}
	// --all without a selector is the area.
	out = captureStdout(t, func() {
		if err := cmdList([]string{"--area", "living"}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"light.living_lamp", "light.dresser_lamp", "media_player.tv"} {
		if !strings.Contains(out, want) {
			t.Errorf("the whole area: %q", out)
		}
	}
	// A write into a scope of three is refused without --all, never guessed.
	err := cmdCall([]string{"--area", "living"}, "on", "homeassistant", "turn_on")
	if err == nil || !strings.Contains(err.Error(), "--all") {
		t.Errorf("multi-entity produce area write must refuse: %v", err)
	}
	// ...and with it, a dry run resolves instead and fires no POSTs.
	n := posts
	if err := cmdCall([]string{"--area", "living", "--all", "--dry-run"}, "on", "homeassistant", "turn_on"); err != nil {
		t.Fatal(err)
	}
	if posts != n {
		t.Errorf("dry run must not fire, got %d new posts", posts-n)
	}
}

// history takes the same selectors as every other command. It used to forward
// the token verbatim, so `has history kitchen` asked Home Assistant for an
// entity actually called "kitchen", got an empty list back and exited 0 - the
// exact output of an entity that never changed.
func TestHistoryResolvesSelectors(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/states" {
			io.WriteString(w, `[
				{"entity_id":"light.kitchen_light","state":"on","attributes":{"friendly_name":"Kitchen Light"}},
				{"entity_id":"light.kitchen_light_2","state":"off","attributes":{"friendly_name":"Kitchen Light 2"}},
				{"entity_id":"light.garage","state":"on","attributes":{"friendly_name":"Garage"}}]`)
			return
		}
		asked = r.URL.Query().Get("filter_entity_id")
		io.WriteString(w, `[[{"entity_id":"light.kitchen_light","state":"on","last_changed":"2026-10-09T18:09:00Z"}],
			[{"entity_id":"light.kitchen_light","state":"off","last_changed":"2026-10-10T00:57:00Z"}]]`)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"url":"`+srv.URL+`","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAS_CONFIG", cfg)

	out := captureStdout(t, func() {
		if err := cmdHistory([]string{"kitchen", "--since", "7d"}); err != nil {
			t.Fatal(err)
		}
	})

	if want := "light.kitchen_light,light.kitchen_light_2"; asked != want {
		t.Errorf("filter_entity_id = %q, want %q", asked, want)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines: %q", len(lines), out)
	}
	// The first change names the entity; the run below it does not repeat it.
	if !strings.HasPrefix(lines[1], "light.kitchen_light") {
		t.Errorf("first change should name the entity: %q", lines[1])
	}
	if strings.HasPrefix(lines[2], "light.kitchen_light") {
		t.Errorf("a repeated id should be left blank: %q", lines[2])
	}
	if !strings.Contains(lines[2], "off") {
		t.Errorf("the second change still has to show its state: %q", lines[2])
	}
}

// `has logbook kitchen` has to mean what `has list kitchen` means: the token is
// resolved against the entity list and the ids go into ?entity=, the logbook's
// only filter.
func TestLogbookResolvesSelectors(t *testing.T) {
	var asked, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/states" {
			io.WriteString(w, `[
				{"entity_id":"light.kitchen_light","state":"on","attributes":{"friendly_name":"Kitchen Light"}},
				{"entity_id":"light.kitchen_light_2","state":"off","attributes":{"friendly_name":"Kitchen Light 2"}}]`)
			return
		}
		path, asked = r.URL.Path, r.URL.Query().Get("entity")
		io.WriteString(w, `[
			{"when":"2026-10-16T18:09:00.120000+00:00","name":"Kitchen Light","entity_id":"light.kitchen_light","state":"on"},
			{"when":"2026-10-16T18:11:02.500000+00:00","name":"Good Night","domain":"script","message":"running"}]`)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"url":"`+srv.URL+`","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAS_CONFIG", cfg)

	out := captureStdout(t, func() {
		if err := cmdLogbook([]string{"kitchen"}); err != nil {
			t.Fatal(err)
		}
	})

	if want := "light.kitchen_light,light.kitchen_light_2"; asked != want {
		t.Errorf("entity = %q, want %q", asked, want)
	}
	// The date is a path segment, in the local day the user asked about.
	if !strings.HasPrefix(path, "/api/logbook/") || len(path) != len("/api/logbook/2006-01-02") {
		t.Errorf("path = %q", path)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines: %q", len(lines), out)
	}
	if !strings.Contains(lines[1], "Kitchen Light") || !strings.Contains(lines[1], "on") {
		t.Errorf("a state change shows its new state: %q", lines[1])
	}
	// The shape varies: an entry with no entity has no state, only a message.
	if !strings.Contains(lines[2], "running") {
		t.Errorf("a script run should show its message: %q", lines[2])
	}
	// Seconds are kept: one sensor managed 847 changes in a day. The hour is the
	// machine's, so measure the column rather than match a literal - and the
	// date and time are separated by a space, so take both words.
	f := strings.Fields(lines[1])
	if col := strings.Join(f[:2], " "); len(col) != len(logbookFormat) {
		t.Errorf("WHEN = %q, want the seconds too (%d columns)", col, len(logbookFormat))
	}
}

func TestLogbookDay(t *testing.T) {
	now := time.Now()
	for _, in := range []string{"today", "", "TODAY", " yesterday "} {
		got, err := parseDay(in)
		if err != nil {
			t.Errorf("parseDay(%q): %v", in, err)
			continue
		}
		want := now
		if strings.Contains(in, "yesterday") {
			want = now.AddDate(0, 0, -1)
		}
		if got.Format("2006-01-02") != want.Format("2006-01-02") {
			t.Errorf("parseDay(%q) = %v, want %v", in, got, want)
		}
	}
	d, err := parseDay("2026-10-16")
	if err != nil || d.Format("2006-01-02") != "2026-10-16" {
		t.Errorf("parseDay(2026-10-16) = %v, %v", d, err)
	}
	for _, bad := range []string{"nope", "16-10-2026", "2026-13-01"} {
		if _, err := parseDay(bad); err == nil {
			t.Errorf("parseDay(%q) should be rejected", bad)
		}
	}
}

// captureStdout redirects os.Stdout for the commands that print a table.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	f()
	w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// testServer runs a server whose responses are driven by the path, so a
// Client can be pointed at it.
func testServer(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/states/sensor.x") {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"entity_id":"sensor.x","state":"42"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL, token: "t", http: srv.Client()}
}

// testFlags builds a FlagSet shaped like the ones the commands use.
func testFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("since", "24h", "")
	fs.Int("limit", 50, "")
	// A bool, because that is the flag that does not take a value and used to
	// take the positional after it anyway.
	fs.Bool("all", false, "")
	return fs
}

// A bool flag does not take the next argument, so nothing may pull it in: doing
// so moved `has call light.turn_on --all entity_id=kitchen`'s payload in front
// of the service name, and silently dropped every flag written after it.
func TestParseArgsBoolFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string // positionals, in the order they were written
	}{
		{"positional then bool", []string{"light.turn_on", "--all", "entity_id=kitchen"},
			[]string{"light.turn_on", "entity_id=kitchen"}},
		{"bool then positional", []string{"--all", "kitchen"}, []string{"kitchen"}},
		{"bool between positionals", []string{"kitchen", "--all", "lamp"}, []string{"kitchen", "lamp"}},
		{"flag after a swallowed one", []string{"kitchen", "--all", "--limit", "5"}, []string{"kitchen"}},
		{"value is not a positional", []string{"-limit", "5", "kitchen"}, []string{"kitchen"}},
		{"equals form", []string{"--since=5m", "kitchen"}, []string{"kitchen"}},
		{"no positionals", []string{"--all", "-limit", "5"}, nil},
	}
	for _, c := range cases {
		fs := testFlags()
		got, err := parseArgs(fs, c.args)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: positionals = %v, want %v", c.name, got, c.want)
		}
	}
}

// The flags themselves still land, whatever side of a positional they were
// written on - a dropped --all is a write refused for the wrong reason, and a
// dropped --dry-run is a write that happened.
func TestParseArgsFlagsStillApply(t *testing.T) {
	fs := testFlags()
	if _, err := parseArgs(fs, []string{"kitchen", "--all", "--limit", "5", "--since=1h"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"all": "true", "limit": "5", "since": "1h"} {
		if got := fs.Lookup(name).Value.String(); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// -o is validated in newFlagSet, so it is caught whichever side of the
// subcommand it was written on. The check used to run only in main, which
// meant `has list -o bogus` ran and printed a table.
func TestOutputFlagValidatedAfterSubcommand(t *testing.T) {
	for _, format := range []string{"bogus", "yaml", "JSON", ""} {
		*flagOutput = format
		if err := checkOutputFormat(); err == nil {
			t.Errorf("-o %q should be rejected", format)
		}
	}
	for _, format := range []string{"table", "json"} {
		*flagOutput = format
		if err := checkOutputFormat(); err != nil {
			t.Errorf("-q %q should be accepted: %v", format, err)
		}
	}
	*flagOutput = "table"
}

// resolveTargets expands entity_id through the same selector rules as every
// other command, so `has call light.turn_on entity_id=kitchen` cannot fire at
// whatever a substring happened to match.
func TestCallResolvesEntityID(t *testing.T) {
	var posted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/states" {
			io.WriteString(w, `[
				{"entity_id":"light.kitchen_light","state":"off","attributes":{"friendly_name":"Kitchen Light"}},
				{"entity_id":"light.kitchen_light_2","state":"off","attributes":{"friendly_name":"Kitchen Light 2"}}]`)
			return
		}
		if r.Method == "POST" {
			b, _ := io.ReadAll(r.Body)
			posted = string(b)
			// light.turn_on changes states; notify.phone changes nothing.
			if strings.Contains(posted, "light.kitchen_light") {
				io.WriteString(w, `[{"entity_id":"light.kitchen_light","state":"on"}]`)
			} else {
				io.WriteString(w, `[]`)
			}
			return
		}
		io.WriteString(w, `[]`)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"url":"`+srv.URL+`","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAS_CONFIG", cfg)

	// A fuzzy name matching several entities is refused without --all.
	if err := cmdCall([]string{"light.turn_on", "entity_id=kitchen"}, "call", "", ""); err == nil {
		t.Error("an ambiguous entity_id should need --all")
	}

	// With --all it expands to both.
	out := captureStdout(t, func() {
		if err := cmdCall([]string{"light.turn_on", "entity_id=kitchen", "--all"}, "call", "", ""); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "light.kitchen_light → on") {
		t.Errorf("write results should go to stdout: %q", out)
	}
	if want := `"entity_id":"light.kitchen_light,light.kitchen_light_2"`; !strings.Contains(posted, want) {
		t.Errorf("posted entity_id = %q, want %s", posted, want)
	}

	// A value matching nothing is passed through untouched: entity_id is not
	// always an entity id, and the server is the authority on what it means.
	out = captureStdout(t, func() {
		if err := cmdCall([]string{"notify.phone", "entity_id=phone"}, "call", "", ""); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "called notify.phone") {
		t.Errorf("a service with no changed states should still report: %q", out)
	}
	if !strings.Contains(posted, `"entity_id":"phone"`) {
		t.Errorf("an unresolvable entity_id should be passed through: %q", posted)
	}
}

// --dry-run answers "what would this touch" without touching it, which is the
// only way to find out before finding out. The ids it prints come from the
// selector resolution, so it has to run that resolution and then stop.
func TestCallDryRun(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/states" {
			io.WriteString(w, `[
				{"entity_id":"light.kitchen_light","state":"off","attributes":{"friendly_name":"Kitchen Light"}},
				{"entity_id":"light.kitchen_light_2","state":"off","attributes":{"friendly_name":"Kitchen Light 2"}}]`)
			return
		}
		if r.Method == http.MethodPost {
			posts++
		}
		io.WriteString(w, `[]`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"url":"`+srv.URL+`","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAS_CONFIG", cfg)

	out := captureStdout(t, func() {
		if err := cmdCall([]string{"kitchen", "--all", "--dry-run"}, "on", "homeassistant", "turn_on"); err != nil {
			t.Fatal(err)
		}
	})
	if posts != 0 {
		t.Errorf("a dry run must not call the server, got %d posts", posts)
	}
	if want := "light.kitchen_light,light.kitchen_light_2"; !strings.Contains(out, want) {
		t.Errorf("a dry run should print the ids the selector resolved: %q", out)
	}
	if !strings.Contains(out, "homeassistant.turn_on") {
		t.Errorf("a dry run should name the service: %q", out)
	}
}

// A bare positional argument to `has call` is an entity selector like every
// other command's: it must resolve to the entity the name matches, in front
// of or behind the key=value tokens.
func TestCallPositionalSelector(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/states" {
			io.WriteString(w, `[
				{"entity_id":"light.kitchen_light","state":"off","attributes":{"friendly_name":"Kitchen Light"}},
				{"entity_id":"light.kitchen_light_2","state":"off","attributes":{"friendly_name":"Kitchen Light 2"}}]`)
			return
		}
		if r.Method == http.MethodPost {
			posts++
		}
		io.WriteString(w, `[]`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"url":"`+srv.URL+`","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAS_CONFIG", cfg)

	if posts != 0 {
		t.Errorf("a dry run must not call the server, got %d posts", posts)
	}
	dry := func(args ...string) string {
		args = append([]string{"light.turn_on"}, args...)
		return captureStdout(t, func() {
			if err := cmdCall(append(args, "--dry-run"), "call", "light", "turn_on"); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Positional selector the AGENTS.md shape, attributes after.
	if out := dry("kitchen_light", "--all", "brightness=187"); !strings.Contains(out, "light.kitchen_light,light.kitchen_light_2") || !strings.Contains(out, "brightness: 187") {
		t.Errorf("positional selector: %q", out)
	}
	// Attributes first, words out of order, same call.
	if out := dry("brightness=100", "kitchen_light", "--all"); !strings.Contains(out, "brightness: 100") {
		t.Errorf("position may not matter for selectors: %q", out)
	}
	// Merged with an entity_id already in the payload rather than overwritten.
	merged := dry("kitchen_light", "--all", "entity_id=kitchen_light_2", "brightness=255")
	if !strings.Contains(merged, "brightness: 255") || strings.Count(merged, "kitchen_light_2") < 2 {
		t.Errorf("positional and entity_id= should both name entities: %q", merged)
	}
	// An ambiguous bare name is refused like on/off refuses it, not guessed.
	// An ambiguous bare name is refused like on/off refuses it, not guessed.
	err := cmdCall([]string{"light.turn_on", "kitchen", "brightness=10"}, "call", "light", "turn_on")
	if err == nil || !strings.Contains(err.Error(), "--all") {
		t.Errorf("ambiguous selector should refuse: %v", err)
	}
}

// When a write matches one friendly name exactly, the substring relatives it
// spawned ("Kitchen Light Power-on behavior") must not force the --all dance:
// the write means the named light, the one entity named after it wins. Reads
// keep the wide answer.
func TestExactFriendlyNameWins(t *testing.T) {
	states := []State{
		{EntityID: "light.dresser_lamp", Attributes: map[string]any{"friendly_name": "Dresser Lamp"}},
		{EntityID: "select.dresser_lamp_power_on_behavior", Attributes: map[string]any{"friendly_name": "Dresser Lamp Power-on behavior"}},
		{EntityID: "switch.dresser_lamp_do_not_disturb", Attributes: map[string]any{"friendly_name": "Dresser Lamp Do not disturb"}},
	}
	matched, err := resolve(states, []string{"Dresser Lamp"}, false)
	if err != nil || len(matched) != 1 || matched[0].EntityID != "light.dresser_lamp" {
		t.Errorf("an exact friendly name should win for a write: %v %v", matched, err)
	}
	// Reads take everything kitchen-ish; nothing here matched one name exactly.
	wide, err := resolve(states, []string{"dresser"}, true)
	if err != nil || len(wide) != len(states) {
		t.Errorf("a read keeps the wide match: %v %v", entityIDs(wide), err)
	}
	// Still refused when the exact name is not unique.
	states = append(states,
		State{EntityID: "light.living_lamp", Attributes: map[string]any{"friendly_name": "Dresser Lamp"}},
	)
	if _, err := resolve(states, []string{"Dresser Lamp"}, false); err == nil {
		t.Error("a duplicated exact name stays ambiguous")
	}
}

// list --brief is the shape to ask for when the attributes are not the question:
// on a real instance they are half the payload, and every row carries two
// timestamps nobody asked for.
func TestListBrief(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"entity_id":"light.kitchen_light","state":"on",
			"attributes":{"friendly_name":"Kitchen Light","supported_features":42},
			"last_changed":"2026-10-09T18:09:00.123456Z","last_updated":"2026-10-09T18:09:00.123456Z"}]`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"url":"`+srv.URL+`","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAS_CONFIG", cfg)
	t.Cleanup(func() { *flagOutput = "table" })
	*flagOutput = "json"

	out := captureStdout(t, func() {
		if err := cmdList([]string{"--brief"}); err != nil {
			t.Fatal(err)
		}
	})
	// Unmarshalled into maps, not into briefState: decoding into the type under
	// test would pass even with the json tags wrong.
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("brief output is not json: %v\n%s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %s", len(rows), out)
	}
	row := rows[0]
	want := map[string]any{
		"entity_id": "light.kitchen_light",
		"name":      "Kitchen Light",
		"state":     "on",
	}
	if len(row) != len(want) {
		t.Errorf("brief row has keys %v, want exactly %v", row, want)
	}
	for k, v := range want {
		if row[k] != v {
			t.Errorf("brief row[%q] = %v, want %v", k, row[k], v)
		}
	}
}

// Every failure used to exit 1, so a script could not tell a mistyped selector
// from a dead server from a dead token without reading the message.
func TestExitCodes(t *testing.T) {
	t.Run("classified", func(t *testing.T) {
		cases := []struct {
			name string
			err  error
			want int
		}{
			{"success", nil, 0},
			{"plain", errors.New("boom"), exitError},
			{"usage", coded{exitUsage, errors.New("bad --sort")}, exitUsage},
			{"selector", coded{exitSelector, errors.New("no entity matches")}, exitSelector},
			{"auth", coded{exitAuth, errors.New("token rejected")}, exitAuth},
			{"network", &url.Error{Op: "GET", URL: "http://ha/api/states", Err: syscall.ECONNREFUSED}, exitNetwork},
			{"timeout", context.DeadlineExceeded, exitNetwork},
			// Client.do wraps everything on its way out, so the code has to
			// survive one wrap - this is the shape a real failure arrives in.
			{"wrapped", fmt.Errorf("GET /api/states: %w",
				coded{exitSelector, errors.New("no entity matches")}), exitSelector},
		}
		for _, c := range cases {
			if got := exitCode(c.err); got != c.want {
				t.Errorf("%s: exitCode = %d, want %d", c.name, got, c.want)
			}
		}
	})

	// And end to end, so the wiring from a real command is covered too.
	t.Run("from a command", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `[
				{"entity_id":"light.kitchen_light","state":"on"},
				{"entity_id":"light.kitchen_light_2","state":"on"}]`)
		}))
		t.Cleanup(srv.Close)
		dir := t.TempDir()
		cfg := filepath.Join(dir, "config.json")
		if err := os.WriteFile(cfg, []byte(`{"url":"`+srv.URL+`","token":"t"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HAS_CONFIG", cfg)

		if err := cmdList([]string{"zzzznope"}); exitCode(err) != exitSelector {
			t.Errorf("a selector that matches nothing should exit %d, got %v (%d)",
				exitSelector, err, exitCode(err))
		}
		if err := cmdList([]string{"--sort", "bogus"}); exitCode(err) != exitUsage {
			t.Errorf("a bad flag value should exit %d, got %v (%d)",
				exitUsage, err, exitCode(err))
		}
	})
}

// `has areas kitchen` has to work on a house with no entity called kitchen: the
// area is the column this command prints, so it is a selector like any other.
func TestAreasSelectsByAreaName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/template":
			io.WriteString(w, "light.kitchen_light\tKitchen\nswitch.coffee\tKitchen\nlock.front_door\tHallway\n"+
				"light.dresser_lamp\tLiving Room\nlight.living_lamp\tLiving Room\n")
		case "/api/states":
			io.WriteString(w, `[
				{"entity_id":"light.kitchen_light","state":"off"},
				{"entity_id":"switch.coffee","state":"off"},
				{"entity_id":"lock.front_door","state":"locked"},
				{"entity_id":"light.dresser_lamp","state":"off"},
				{"entity_id":"light.living_lamp","state":"on"},
				{"entity_id":"sensor.no_area","state":"42"}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"url":"`+srv.URL+`","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAS_CONFIG", cfg)

	run := func(args ...string) string {
		return captureStdout(t, func() {
			if err := cmdAreas(args); err != nil {
				t.Fatal(err)
			}
		})
	}
	all := strings.Split(strings.TrimRight(run(), "\n"), "\n")
	if len(all) != 4 { // header + Kitchen(2) + Living Room(2) + Hallway(1); the sensor has no area
		t.Fatalf("got %d lines: %q", len(all), run())
	}
	if !strings.Contains(all[1], "Kitchen") || !strings.Contains(all[1], "2") {
		t.Errorf("busiest areas first with their counts: %q", all[1])
	}
	// By area name, not by entity name: nothing here is called "kitchen".
	one := strings.Split(strings.TrimRight(run("kitchen"), "\n"), "\n")
	if len(one) != 2 || !strings.Contains(one[1], "Kitchen") {
		t.Errorf("`areas kitchen` = %q", one)
	}
	// ...and a token that also matches entity names still means the whole
	// area: "living" names one entity by substring but the area holds two,
	// and the dresser lamp shares none of the token.
	t.Cleanup(func() { *flagOutput = "table" })
	*flagOutput = "json"
	js := captureStdout(t, func() {
		if err := cmdAreas([]string{"living", "-o", "json"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(js, "light.dresser_lamp") || !strings.Contains(js, "light.living_lamp") {
		t.Errorf("`areas living` answered %q, not the whole area", js)
	}
	// And an entity selector still works, resolving the usual way.
	if got := run("lock"); !strings.Contains(got, "Hallway") {
		t.Errorf("`areas lock` = %q", got)
	}
	if err := cmdAreas([]string{"nothing"}); err == nil {
		t.Error("a selector that matches neither an entity nor an area should fail")
	}
}

func TestClientEntityAreas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/template" || r.Method != "POST" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, "light.kitchen_light\tKitchen\nswitch.coffee\tKitchen\nlock.front_door\tHallway\n")
	}))
	t.Cleanup(srv.Close)

	c, err := newClient(Config{URL: srv.URL, Token: "t"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	areas, err := c.entityAreas()
	if err != nil {
		t.Fatal(err)
	}
	if len(areas) != 3 {
		t.Fatalf("expected 3 areas, got %d", len(areas))
	}
	if areas["light.kitchen_light"] != "Kitchen" || areas["lock.front_door"] != "Hallway" {
		t.Errorf("unexpected areas: %v", areas)
	}
}

// `has template` exists so a question with no endpoint can be asked. The two
// things that must not drift are what gets sent - the template, verbatim, to the
// one endpoint that renders - and what comes back, unchanged, because that is
// what makes `| jq` work.
func TestTemplateRendersServerSide(t *testing.T) {
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, _ = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, "42")
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"url":"`+srv.URL+`","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAS_CONFIG", cfg)

	run := func(args ...string) string {
		return captureStdout(t, func() {
			if err := cmdTemplate(args); err != nil {
				t.Fatal(err)
			}
		})
	}
	if got := run("{{", "states('sensor.x')", "}}"); got != "42" {
		t.Errorf("output = %q, want the rendered result verbatim", got)
	}
	if path != "/api/template" {
		t.Errorf("posted to %q", path)
	}
	if body != `{"template":"{{ states('sensor.x') }}"}` {
		t.Errorf("body = %q, want the template verbatim", body)
	}
	// No arguments means stdin, which is what a piped or edited template uses.
	saved := stdin
	stdin = bufio.NewReader(strings.NewReader("{{ states('sensor.x') }}"))
	defer func() { stdin = saved }()
	if got := run(); got != "42" {
		t.Errorf("stdin output = %q, want the rendered result", got)
	}
	if err := cmdTemplate(nil); err == nil {
		t.Error("an empty template should fail rather than render nothing silently")
	}
}

// A tail is only useful if it says what changed and when the server thinks it
// changed, and if a selector means the same thing here as everywhere else.
func TestWatchPrintsChanges(t *testing.T) {
	// The `data` half of a real state_changed frame, including the null
	// new_state the server sends when an entity goes away.
	const (
		turnedOn = `{"entity_id":"light.kitchen_light",` +
			`"old_state":{"entity_id":"light.kitchen_light","state":"off","last_changed":"2026-10-17T09:00:00Z"},` +
			`"new_state":{"entity_id":"light.kitchen_light","state":"on","last_updated":"2026-10-17T09:00:01Z",` +
			`"attributes":{"friendly_name":"Kitchen Light"}}}`
		garage = `{"entity_id":"light.garage_light",` +
			`"old_state":{"entity_id":"light.garage_light","state":"on","last_changed":"2026-10-17T09:01:00Z",` +
			`"attributes":{"friendly_name":"Garage Light"}},"new_state":null}`
	)
	out := captureStdout(t, func() {
		print := watchPrinter(map[string]bool{"light.kitchen_light": true})
		print(wsStateChanged, []byte(turnedOn))
		print(wsStateChanged, []byte(garage)) // filtered out: not in `only`
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %q", len(lines), out)
	}
	if !strings.Contains(lines[0], "WHEN") || !strings.Contains(lines[0], "CHANGE") {
		t.Errorf("first line is the header: %q", lines[0])
	}
	// The server's timestamp, not the moment the line was printed.
	changed := time.Date(2026, 10, 17, 9, 0, 1, 0, time.UTC)
	if !strings.Contains(lines[1], timestampSec(changed)) {
		t.Errorf("line = %q, want the server's own timestamp", lines[1])
	}
	// The name comes out of the event, so the reader does not have to translate
	// an id with `has list` to know what moved.
	if !strings.Contains(lines[1], "light.kitchen_light") || !strings.Contains(lines[1], "off → on") {
		t.Errorf("line = %q, want the entity and its transition", lines[1])
	}
	if !strings.Contains(lines[1], "Kitchen Light") {
		t.Errorf("line = %q, want the friendly name from the event", lines[1])
	}
	if strings.Contains(out, "garage") {
		t.Errorf("an entity outside the filter was printed: %q", out)
	}

	// A removed entity says so rather than printing an empty state.
	out = captureStdout(t, func() {
		watchPrinter(nil)(wsStateChanged, []byte(garage))
	})
	if !strings.Contains(out, "removed") {
		t.Errorf("a null new_state should read as removed: %q", out)
	}
	// A frame we cannot read is skipped, not fatal.
	out = captureStdout(t, func() {
		print := watchPrinter(nil)
		print(wsStateChanged, []byte(`not json`))
		print("call_service", []byte(`{}`))
	})
	if out != "" {
		t.Errorf("junk and other event types should print nothing: %q", out)
	}
}
