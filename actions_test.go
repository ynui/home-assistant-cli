package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func tgt(domains ...string) *ServiceTarget {
	t := &ServiceTarget{}
	for _, d := range domains {
		t.Entity = append(t.Entity, struct {
			Domain []string `json:"domain"`
		}{Domain: []string{d}})
	}
	return t
}

func names(acts []action) []string {
	out := make([]string, 0, len(acts))
	for _, a := range acts {
		out = append(out, a.service)
	}
	return out
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestRegistryDerivesFromServices(t *testing.T) {
	r := newRegistry([]Service{
		// Generic power verbs, deliberately listed out of order to prove the
		// registry imposes its own ordering.
		{Domain: "light", Name: "turn_off", Target: tgt("light")},
		{Domain: "light", Name: "toggle", Target: tgt("light")},
		{Domain: "light", Name: "turn_on", Target: tgt("light")},

		// Covers do not use open/close; they use open_cover/close_cover.
		{Domain: "cover", Name: "toggle", Target: tgt("cover")},
		{Domain: "cover", Name: "open_cover", Target: tgt("cover")},
		{Domain: "cover", Name: "close_cover", Target: tgt("cover")},

		// A lock's meaningful actions.
		{Domain: "lock", Name: "unlock", Target: tgt("lock")},
		{Domain: "lock", Name: "lock", Target: tgt("lock")},
		{Domain: "lock", Name: "open", Target: tgt("lock")},

		// Read-only domains.
		{Domain: "sensor", Name: "check_calibration", Target: tgt("sensor")},
		{Domain: "sun", Name: "noop", Target: nil},
	})

	if got := names(r.actions("light")); !equal(got, []string{"toggle", "turn_on", "turn_off"}) {
		t.Errorf("light = %v", got)
	}
	// open_cover/close_cover are control verbs; plain open/close are not in
	// the cover domain's vocabulary here.
	if got := names(r.actions("cover")); !equal(got, []string{"toggle", "open_cover", "close_cover"}) {
		t.Errorf("cover = %v", got)
	}
	// lock leads with the meaningful pair; open means something different.
	if got := names(r.actions("lock")); !equal(got, []string{"lock", "unlock", "open"}) {
		t.Errorf("lock = %v", got)
	}
	for _, d := range []string{"sensor", "sun", "zone", "weather"} {
		if r.controllable(d) {
			t.Errorf("%s should not be controllable", d)
		}
	}
	if !r.usable {
		t.Error("registry should be usable when targets are present")
	}
}

// A global service like script.start_bedroom_ac takes no entity, so it must
// not make every script look actionable.
func TestRegistryIgnoresGlobalServices(t *testing.T) {
	r := newRegistry([]Service{
		{Domain: "script", Name: "start_bedroom_ac", Target: nil},
		{Domain: "script", Name: "turn_on", Target: tgt("script")},
	})
	if got := names(r.actions("script")); !equal(got, []string{"turn_on"}) {
		t.Errorf("script = %v", got)
	}
}

// Offering an action that needs an argument we cannot supply would only ever
// produce an error, so required-field services are dropped.
func TestRegistryDropsRequiredArgServices(t *testing.T) {
	need := map[string]ServiceField{"position": {Required: true}}
	optional := map[string]ServiceField{"brightness_pct": {Required: false}}
	r := newRegistry([]Service{
		{Domain: "cover", Name: "toggle", Target: tgt("cover")},
		{Domain: "cover", Name: "set_cover_position", Target: tgt("cover"), Fields: need},
		{Domain: "cover", Name: "open_cover", Target: tgt("cover"), Fields: optional},
		{Domain: "todo", Name: "add_item", Target: tgt("todo"),
			Fields: map[string]ServiceField{"item": {Required: true}}},
	})
	if got := names(r.actions("cover")); !equal(got, []string{"toggle", "open_cover"}) {
		t.Errorf("cover = %v, set_cover_position needs a position", got)
	}
	if r.controllable("todo") {
		t.Error("todo.add_item needs an item, so todo has nothing to offer")
	}
}

// One service may target several domains; each gets its own action.
func TestRegistryMultiTarget(t *testing.T) {
	r := newRegistry([]Service{
		{Domain: "homeassistant", Name: "toggle", Target: tgt("light", "switch", "fan")},
	})
	for _, d := range []string{"light", "switch", "fan"} {
		if !r.controllable(d) {
			t.Errorf("%s should be controllable", d)
		}
	}
}

// A server that advertises no targets at all must not leave us showing
// nothing; usable=false is the signal to fall back to showing everything.
func TestRegistryNoTargetData(t *testing.T) {
	r := newRegistry([]Service{
		{Domain: "light", Name: "turn_on", Target: nil},
		{Domain: "switch", Name: "turn_on"},
	})
	if r.usable {
		t.Error("no service carried a target, so usable should be false")
	}
	if r.controllable("light") {
		t.Error("nothing is derivable without targets")
	}
}

func TestRegistryFindAndLabels(t *testing.T) {
	r := newRegistry([]Service{
		{Domain: "light", Name: "toggle", Target: tgt("light")},
		{Domain: "light", Name: "turn_on", Target: tgt("light")},
	})
	if a := r.find("light", "turn_on"); a == nil || a.label != "on" {
		t.Errorf("find turn_on = %+v", a)
	}
	if a := r.find("light", "lock"); a != nil {
		t.Errorf("light has no lock action, got %+v", a)
	}
	if a := r.find("cover", "open_cover"); a != nil {
		t.Errorf("unknown domain should return nil, got %+v", a)
	}
	// Duplicate declarations of the same service must not appear twice.
	dup := newRegistry([]Service{
		{Domain: "light", Name: "toggle", Target: tgt("light")},
		{Domain: "other", Name: "toggle", Target: tgt("light")},
	})
	if got := names(dup.actions("light")); !equal(got, []string{"toggle"}) {
		t.Errorf("duplicates leaked: %v", got)
	}
}

// The grouped /api/services payload must flatten correctly. This is the shape
// real Home Assistant returns, and getting it wrong produced an empty
// `has services` that only the mock ever disagreed with.
func TestServicesFlattening(t *testing.T) {
	c := testServer(t, `[
	  {"domain":"light","services":{
	     "turn_on":{"name":"Turn on","description":"d1","fields":{},"target":{"entity":[{"domain":["light"]}]}},
	     "turn_off":{"name":"Turn off","description":"","fields":{},"target":null}}},
	  {"domain":"lock","services":{
	     "unlock":{"name":"Unlock","description":"","fields":{},"target":{"entity":[{"domain":["lock"]}]}}}}]`)

	got, err := c.services()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d services, want 3: %+v", len(got), got)
	}
	// Sorted by domain then name.
	if got[0].Domain != "light" || got[0].Name != "turn_off" || got[1].Name != "turn_on" {
		t.Errorf("bad ordering: %+v", got[:2])
	}
	if got[2].Domain != "lock" || got[2].Name != "unlock" {
		t.Errorf("got %+v", got[2])
	}
	if got[1].Description != "d1" {
		t.Errorf("description lost: %q", got[1].Description)
	}
	if d := got[1].entityDomains(); len(d) != 1 || d[0] != "light" {
		t.Errorf("entityDomains = %v", d)
	}
	if d := got[0].entityDomains(); len(d) != 0 {
		t.Errorf("turn_off has no target, got %v", d)
	}
	// The registry built from that payload must still work.
	if !newRegistry(got).controllable("lock") {
		t.Error("lock should be actionable from the real payload")
	}
}

// The lazy single-entity read used by the UI to refresh only the hovered row.
func TestClientStateSingle(t *testing.T) {
	c := testServer(t, `[]`)
	s, err := c.state("sensor.x")
	if err != nil {
		t.Fatal(err)
	}
	if s.EntityID != "sensor.x" || s.State != "42" {
		t.Errorf("got %+v", s)
	}
}

// Home Assistant parses the period as a datetime string and returns 400
// "Invalid datetime" for a bare Unix epoch, so history must send RFC3339.
func TestClientHistoryPeriodFormat(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[[{"entity_id":"light.a","state":"on"}]]`)
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, token: "t", http: srv.Client()}

	_, err := c.history([]string{"light.a"}, time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := "/api/history/period/2026-04-01T12:00:00Z"
	if gotPath != want {
		t.Errorf("path = %q, want %q (RFC3339, not an epoch)", gotPath, want)
	}
}

// A service call answers with the states it changed. That body is the only
// confirmation a write ever gets, so it must not be thrown away.
func TestClientCallReturnsChangedStates(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"entity_id":"light.kitchen_light","state":"on"}]`)
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, token: "t", http: srv.Client()}

	changed, err := c.call("homeassistant", "turn_on", map[string]any{"entity_id": []string{"light.kitchen_light"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/services/homeassistant/turn_on" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, "light.kitchen_light") {
		t.Errorf("request body = %q", gotBody)
	}
	if len(changed) != 1 || changed[0].EntityID != "light.kitchen_light" || changed[0].State != "on" {
		t.Errorf("changed = %+v", changed)
	}

	// A service that changes nothing answers with an empty list, not an error.
	empty := testServer(t, `[]`)
	got, err := empty.call("homeassistant", "turn_on", nil)
	if err != nil || len(got) != 0 {
		t.Errorf("empty result: %v %v", got, err)
	}
}
