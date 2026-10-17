package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readServer is a canned Home Assistant in the shapes the real endpoints answer:
// a flat /api/states, a grouped /api/services, a template render for areas, and a
// logbook whose entries are not all the same shape.
func readServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/states":
			io.WriteString(w, `[
				{"entity_id":"light.kitchen_light","state":"on","attributes":{"friendly_name":"Kitchen Light"},"last_changed":"2026-10-16T18:09:00.123456+00:00","last_updated":"2026-10-16T18:09:00.123456+00:00"},
				{"entity_id":"light.garage_light","state":"off","attributes":{"friendly_name":"Garage Light"},"last_changed":"2026-10-16T18:09:00.123456+00:00","last_updated":"2026-10-16T18:09:00.123456+00:00"},
				{"entity_id":"light.hall","state":"on","attributes":{"friendly_name":"Hall & Stairs"},"last_changed":"2026-10-16T18:09:00.123456+00:00","last_updated":"2026-10-16T18:09:00.123456+00:00"},
				{"entity_id":"lock.front_door","state":"locked","attributes":{"friendly_name":"Front Door"},"last_changed":"2026-10-16T18:09:00.123456+00:00","last_updated":"2026-10-16T18:09:00.123456+00:00"},
				{"entity_id":"switch.coffee","state":"off","attributes":{"friendly_name":"Coffee"},"last_changed":"2026-10-16T18:09:00.123456+00:00","last_updated":"2026-10-16T18:09:00.123456+00:00"},
				{"entity_id":"sensor.temperature","state":"21.5","attributes":{"friendly_name":"Temperature","unit_of_measurement":"°C"},"last_changed":"2026-10-16T18:09:00.123456+00:00","last_updated":"2026-10-16T18:09:00.123456+00:00"}]`)
		case r.URL.Path == "/api/services":
			io.WriteString(w, `[
				{"domain":"light","services":{
					"turn_on":{"description":"Turn on","fields":{},"target":{"entity":[{"domain":["light"]}]}},
					"turn_off":{"description":"Turn off","fields":{},"target":{"entity":[{"domain":["light"]}]}},
					"toggle":{"description":"Toggle","fields":{},"target":{"entity":[{"domain":["light"]}]}}}},
				{"domain":"switch","services":{
					"turn_on":{"description":"Turn on","fields":{},"target":{"entity":[{"domain":["switch"]}]}}}},
				{"domain":"homeassistant","services":{
					"restart":{"description":"Restart","fields":{}}}}]`)
		case r.URL.Path == "/api/template":
			io.WriteString(w, "light.kitchen_light\tKitchen\nswitch.coffee\tKitchen\nlock.front_door\tHallway\n")
		case strings.HasPrefix(r.URL.Path, "/api/logbook/"):
			io.WriteString(w, `[
				{"when":"2026-10-16T18:09:00.120000+00:00","name":"Kitchen Light","entity_id":"light.kitchen_light","state":"on"},
				{"when":"2026-10-16T18:11:02.500000+00:00","name":"Good Night","domain":"script","message":"running"}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wantInOrder checks that each want appears in out, and in the order given: a
// table whose columns moved is still the same words in a different place, and
// the order is half of what the command promised.
func wantInOrder(t *testing.T, out string, wants ...string) {
	t.Helper()
	at := 0
	for _, w := range wants {
		i := strings.Index(out[at:], w)
		if i < 0 {
			t.Errorf("%q is missing from the output:\n%s", w, out)
			return
		}
		at += i + len(w)
	}
}

// The read commands' tables are the product: a column that moves, a header that
// changes, a group count that lies, and every one of them still exits 0. The
// writes are pinned elsewhere (TestExitCodes, TestCallDryRun); this is the other
// half - what each read actually prints against a server shaped like a real one.
func TestReadCommandOutput(t *testing.T) {
	srv := readServer(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"url":"`+srv.URL+`","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAS_CONFIG", cfg)

	cases := []struct {
		name string
		args []string
		want []string
		not  []string // must be absent
	}{
		{"groups", []string{"groups"},
			[]string{"DOMAIN", "COUNT", "light", "3", "lock", "1", "sensor", "1", "switch", "1"},
			nil},
		{"list", []string{"list"},
			[]string{"ENTITY", "NAME", "STATE", "light (3)", "light.kitchen_light", "Kitchen Light", "on",
				"lock (1)", "sensor (1)", "switch (1)"},
			nil},
		// A text sort has no group headers: they would imply an order the rows
		// are not in.
		{"list sorted by name, reversed", []string{"list", "--sort", "name", "--reverse"},
			[]string{"Temperature", "Kitchen Light", "Hall & Stairs", "Garage Light", "Coffee"},
			[]string{"light (3)"}},
		// The count describes the whole domain, not the rows that survived --limit.
		{"list limited", []string{"list", "--limit", "1"},
			[]string{"light (3)", "light.garage_light"},
			nil},
		{"get", []string{"get", "light.kitchen_light"},
			[]string{"ENTITY", "NAME", "STATE", "CHANGED", "Kitchen Light", "on"},
			nil},
		// TARGETS is what says whether a service can be aimed at an entity at all.
		{"services", []string{"services"},
			[]string{"DOMAIN", "SERVICE", "TARGETS", "DESCRIPTION", "homeassistant", "restart",
				"light", "toggle", "light", "switch", "turn_on", "switch"},
			nil},
		{"areas", []string{"areas"},
			[]string{"AREA", "COUNT", "Kitchen", "2", "Hallway", "1"},
			nil},
		// The shape varies per entry: a state change carries state, a script run
		// carries message, and only when and name are always there.
		{"logbook", []string{"logbook"},
			[]string{"WHEN", "ENTITY", "NAME", "CHANGE", "Kitchen Light", "on", "running"},
			nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				if err := runRead(c.args); err != nil {
					t.Fatal(err)
				}
			})
			wantInOrder(t, out, c.want...)
			for _, n := range c.not {
				if strings.Contains(out, n) {
					t.Errorf("%q should not be in the output:\n%s", n, out)
				}
			}
		})
	}
}

// runRead dispatches to the read command behind args[0], so the table above can
// name a command the way the user typed it.
func runRead(args []string) error {
	switch args[0] {
	case "groups":
		return cmdGroups(args[1:])
	case "list":
		return cmdList(args[1:])
	case "get":
		return cmdGet(args[1:])
	case "services":
		return cmdServices(args[1:])
	case "areas":
		return cmdAreas(args[1:])
	case "logbook":
		return cmdLogbook(args[1:])
	}
	return fmt.Errorf("no such read command %q", args[0])
}

// -o json is for jq and for agents, not for a browser: an entity called
// "Hall & Stairs" should come back as itself and not as "Hall \u0026 Stairs".
func TestReadCommandJSON(t *testing.T) {
	srv := readServer(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"url":"`+srv.URL+`","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAS_CONFIG", cfg)
	t.Cleanup(func() { *flagOutput = "table" })

	*flagOutput = "json"
	out := captureStdout(t, func() {
		if err := cmdList(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "Hall & Stairs") {
		t.Errorf("json output should not html-escape a name:\n%s", out)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("list -o json is not json: %v\n%s", err, out)
	}
	if len(rows) != 6 {
		t.Errorf("got %d rows, want 6: %s", len(rows), out)
	}

	out = captureStdout(t, func() {
		if err := cmdGroups(nil); err != nil {
			t.Fatal(err)
		}
	})
	var counts []map[string]any
	if err := json.Unmarshal([]byte(out), &counts); err != nil {
		t.Fatalf("groups -o json is not json: %v\n%s", err, out)
	}
	if len(counts) != 4 || counts[0]["domain"] != "light" || counts[0]["count"] != 3.0 {
		t.Errorf("groups -o json = %s, want light first with 3", out)
	}
	*flagOutput = "table"
}

// newAPIError keeps Home Assistant's own message so a caller can print it
// verbatim, and a 401 is the one status with its own exit code: a script has to
// be able to tell a dead token from a dead server without reading the message.
func TestAPIErrorStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string // what the command will print
		code   int
	}{
		{"401 is the token", 401, `{"message":"Unauthorized"}`,
			"token rejected (401); run `has login` to enter a new one", exitAuth},
		{"HA's own message", 400, `{"message":"Invalid datetime"}`, "Invalid datetime", exitError},
		{"no message in the body", 503, ``, "HTTP 503: Service Unavailable", exitError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				io.WriteString(w, c.body)
			}))
			t.Cleanup(srv.Close)
			dir := t.TempDir()
			cfg := filepath.Join(dir, "config.json")
			if err := os.WriteFile(cfg, []byte(`{"url":"`+srv.URL+`","token":"t"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HAS_CONFIG", cfg)

			err := cmdList(nil)
			if err == nil {
				t.Fatal("the command should have failed")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("message = %q, want it to contain %q", err, c.want)
			}
			if got := exitCode(err); got != c.code {
				t.Errorf("exit code = %d, want %d", got, c.code)
			}
		})
	}
}

// A server that was never reachable is exit 5, not 1: a script retries on that
// and gives up on everything else.
func TestUnreachableServerExitCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening any more

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"url":"`+url+`","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAS_CONFIG", cfg)

	if err := cmdList(nil); exitCode(err) != exitNetwork {
		t.Errorf("a dead server should exit %d, got %v (%d)", exitNetwork, err, exitCode(err))
	}
}

// config.json holds a bearer token that grants full control of the house, so it
// is 0600 inside a 0700 directory. The mode is applied explicitly, because
// WriteFile only sets it when it creates the file - a file that already exists
// with a loose mode has to be tightened, not left.
func TestConfigFileMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "has") // not created yet
	cfg := filepath.Join(dir, "config.json")
	t.Setenv("HAS_CONFIG", cfg)

	if err := saveConfig(Config{URL: "http://ha", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, cfg); got != 0o600 {
		t.Errorf("config.json is %o, want 600", got)
	}
	if got := mode(t, dir); got != 0o700 {
		t.Errorf("the config directory is %o, want 700", got)
	}

	// And an existing file with a loose mode is tightened on the way through.
	if err := os.WriteFile(cfg, []byte(`{"url":"http://ha","token":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(Config{URL: "http://ha", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, cfg); got != 0o600 {
		t.Errorf("an existing config.json is %o, want 600", got)
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// The list is the hot path: every keystroke and every websocket event rebuilds
// it, so a house ten times the size of the fixture is the number that matters.
func bigHouse() *uiModel {
	m := uiFixture()
	m.states = make([]State, 0, 4000)
	for i := 0; i < 4000; i++ {
		m.states = append(m.states, State{
			EntityID:   fmt.Sprintf("light.light_%d", i),
			State:      "on",
			Attributes: map[string]any{"friendly_name": fmt.Sprintf("Light %d", i)},
		})
	}
	m.resetCursor()
	return m
}

func BenchmarkUIRows(b *testing.B) {
	m := bigHouse()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = m.rows()
	}
}

func BenchmarkUIView(b *testing.B) {
	m := bigHouse()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = m.View()
	}
}
