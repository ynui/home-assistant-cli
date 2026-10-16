// Command fakeha serves a canned Home Assistant, so the demo in demo/demo.tape
// needs no house of its own.
//
// It answers the endpoints has actually uses, with the shapes Home Assistant
// really returns — a flat /api/states, a /api/services grouped under "services",
// a history body that is a list of lists — checks the bearer token, and speaks
// the real WebSocket handshake, so the TUI's header reads "live" and rows change
// while the tape is rolling. Entities also flip on a timer, which is what makes
// the demo look like a house rather than a screenshot.
//
//	go run ./demo/fakeha           # http://127.0.0.1:8123, token: demo
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// State mirrors the one field set has cares about. Deliberately the same shape
// as the real response, because that is the contract: anything the demo proves
// about the client has to come from a body the server would really send.
type State struct {
	EntityID     string         `json:"entity_id"`
	State        string         `json:"state"`
	Attributes   map[string]any `json:"attributes"`
	LastChanged  time.Time      `json:"last_changed"`
	LastUpdated  time.Time      `json:"last_updated"`
	LastReported time.Time      `json:"last_reported"`
}

type entity struct {
	id       string
	name     string
	state    string
	unit     string
	class    string
	area     string
	position int // cover only
}

// house is the apartment the demo shows. Read-only entities (sensors) are in
// here too: the TUI hides them until ctrl+a, and the header count is only
// interesting because it is counting them.
var house = []entity{
	{"light.kitchen_light", "Kitchen Light", "on", "", "", "Kitchen", 0},
	{"light.kitchen_light_2", "Kitchen Light 2", "off", "", "", "Kitchen", 0},
	{"light.living_room_lamp", "Living Room Lamp", "on", "", "", "Living Room", 0},
	{"light.bedroom_light", "Bedroom Light", "off", "", "", "Bedroom", 0},
	{"light.hallway_light", "Hallway Light", "off", "", "", "Hallway", 0},
	{"light.bathroom_light", "Bathroom Light", "off", "", "", "Bathroom", 0},
	{"light.porch_light", "Porch Light", "off", "", "", "", 0},

	{"switch.coffee_maker", "Coffee Maker", "off", "", "", "Kitchen", 0},
	{"switch.tv", "TV", "on", "", "", "Living Room", 0},
	{"switch.washing_machine", "Washing Machine", "off", "", "", "Garage", 0},
	{"switch.dehumidifier", "Dehumidifier", "off", "", "", "Garage", 0},
	{"switch.server_rack", "Server Rack", "on", "", "", "Attic", 0},

	{"sensor.hall_temperature", "Hall Temperature", "21.4", "°C", "temperature", "Hallway", 0},
	{"sensor.kitchen_humidity", "Kitchen Humidity", "48", "%", "humidity", "Kitchen", 0},
	{"sensor.outside_temperature", "Outside Temperature", "14.2", "°C", "temperature", "", 0},
	{"sensor.power_meter", "Power Meter", "312", "W", "power", "", 0},
	{"sensor.co2", "CO2", "620", "ppm", "carbon_dioxide", "", 0},
	{"sensor.bedroom_illuminance", "Bedroom Illuminance", "12", "lx", "illuminance", "Bedroom", 0},
	{"sensor.freezer_temperature", "Freezer Temperature", "-18.5", "°C", "temperature", "", 0},
	{"sensor.dishwasher_energy", "Dishwasher Energy", "0.2", "kWh", "energy", "", 0},
	{"sensor.solar_power", "Solar Power", "0.4", "W", "power", "", 0},
	{"sensor.attic_temperature", "Attic Temperature", "9.1", "°C", "temperature", "Attic", 0},

	{"binary_sensor.hallway_motion", "Hallway Motion", "off", "", "motion", "Hallway", 0},
	{"binary_sensor.front_door_contact", "Front Door", "off", "", "door", "Hallway", 0},
	{"binary_sensor.living_room_window", "Living Room Window", "off", "", "window", "Living Room", 0},
	{"binary_sensor.basement_moisture", "Basement Moisture", "off", "", "moisture", "", 0},
	{"binary_sensor.smoke_alarm", "Smoke Alarm", "off", "", "smoke", "Attic", 0},

	{"lock.front_door", "Front Door Lock", "locked", "", "", "Hallway", 0},
	{"lock.back_door", "Back Door Lock", "locked", "", "", "", 0},

	{"cover.living_room_blinds", "Living Room Blinds", "open", "", "", "Living Room", 100},
	{"cover.bedroom_blinds", "Bedroom Blinds", "closed", "", "", "Bedroom", 0},
	{"cover.garage_door", "Garage Door", "closed", "", "", "Garage", 0},

	{"climate.living_room_thermostat", "Living Room Thermostat", "heat", "", "", "Living Room", 0},
	{"climate.bedroom_thermostat", "Bedroom Thermostat", "heat", "", "", "Bedroom", 0},

	{"media_player.living_room_tv", "Living Room TV", "on", "", "", "Living Room", 0},
	{"media_player.kitchen_speaker", "Kitchen Speaker", "off", "", "", "Kitchen", 0},

	{"fan.ceiling_fan", "Ceiling Fan", "off", "", "", "Living Room", 0},
	{"fan.extractor_fan", "Extractor Fan", "off", "", "", "Bathroom", 0},

	{"automation.morning_lights", "Morning Lights", "off", "", "", "", 0},
	{"automation.good_night", "Good Night", "on", "", "", "", 0},
	{"automation.movie_time", "Movie Time", "off", "", "", "", 0},
	{"automation.vacuum_schedule", "Vacuum Schedule", "off", "", "", "", 0},

	{"script.good_morning", "Good Morning", "off", "", "", "", 0},
	{"script.sleep_now", "Sleep Now", "off", "", "", "", 0},
}

// server holds the one mutable thing the demo has: the states, plus everyone
// listening on the socket.
type server struct {
	mu     sync.Mutex
	states map[string]*State
	order  []string
	subs   map[*subscriber]bool
}

type subscriber struct {
	conn *websocket.Conn
	mu   sync.Mutex // websocket.Conn allows one writer at a time
}

var token = flag.String("token", "demo", "bearer token to require")
var addr = flag.String("addr", "127.0.0.1:8123", "address to listen on")

func main() {
	flag.Parse()

	s := &server{
		states: map[string]*State{},
		subs:   map[*subscriber]bool{},
	}
	// Every entity reports itself as last touched two hours ago, so the demo
	// does not claim a house where everything changed a second ago.
	for _, e := range house {
		st := &State{
			EntityID:     e.id,
			State:        e.state,
			Attributes:   attributes(e),
			LastChanged:  time.Now().Add(-2 * time.Hour).UTC(),
			LastUpdated:  time.Now().Add(-2 * time.Hour).UTC(),
			LastReported: time.Now().Add(-2 * time.Hour).UTC(),
		}
		s.states[e.id] = st
		s.order = append(s.order, e.id)
	}

	go s.idleFlips()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		log.Println("fakeha: bye")
		os.Exit(0)
	}()

	log.Printf("fakeha: http://%s (token %q), %d entities", *addr, *token, len(s.order))
	log.Fatal(http.ListenAndServe(*addr, s.mux()))
}

// attributes gives each entity the fields its domain really carries. It is what
// the details panel prints, so a bare friendly_name would make ctrl+d look like a
// bug in the client rather than a demo with nothing to show.
func attributes(e entity) map[string]any {
	a := map[string]any{"friendly_name": e.name}
	domain, _, _ := strings.Cut(e.id, ".")
	switch domain {
	case "light":
		a["supported_color_modes"] = []string{"brightness"}
		if e.state == "on" {
			a["brightness"] = 180
		}
	case "sensor":
		if e.unit != "" {
			a["unit_of_measurement"] = e.unit
		}
		if e.class != "" {
			a["device_class"] = e.class
			a["state_class"] = "measurement"
		}
	case "binary_sensor":
		if e.class != "" {
			a["device_class"] = e.class
		}
	case "cover":
		a["supported_features"] = 3
		a["current_position"] = e.position
	case "climate":
		a["current_temperature"] = strings.TrimSuffix(e.state, "")
		a["temperature"] = 21
		a["hvac_modes"] = []string{"off", "heat", "cool"}
		a["min_temp"] = 7.0
		a["max_temp"] = 35.0
	case "media_player":
		a["volume_level"] = 0.42
		a["supported_features"] = 44363
	case "fan":
		a["percentage"] = 0
		a["supported_features"] = 1
	}
	return a
}

// writeJSON is the one place a response body is written, so every handler
// agrees on the content type and on never writing twice.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("fakeha: write: %v", err)
	}
}

func (s *server) mux() *http.ServeMux {
	m := http.NewServeMux()

	// Everything below /api needs the bearer token, exactly as the real API does,
	// so the demo would fail the same way a revoked token does.
	authed := func(pattern string, h http.HandlerFunc) {
		m.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+*token {
				writeJSON(w, http.StatusUnauthorized, map[string]string{
					"message": "Invalid access token or password",
				})
				return
			}
			h(w, r)
		})
	}

	m.HandleFunc("GET /api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"message": "API running."})
	})

	authed("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		// The version the client was last verified against, so the demo does not
		// imply support for something newer.
		writeJSON(w, http.StatusOK, map[string]any{
			"version":       "2026.9.4",
			"location_name": "Home",
			"time_zone":     "UTC",
		})
	})

	authed("GET /api/states", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.all())
	})

	authed("GET /api/states/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		st, ok := s.states[r.PathValue("id")]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Entity not found."})
			return
		}
		writeJSON(w, http.StatusOK, *st)
	})

	authed("GET /api/services", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, services())
	})

	authed("GET /api/logbook/{day}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.logbook(strings.Split(r.URL.Query().Get("entity"), ",")))
	})

	authed("GET /api/history/period/{ts}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.history(strings.Split(r.URL.Query().Get("filter_entity_id"), ",")))
	})

	// The real endpoint renders a template; this one answers the one the areas
	// map is built from and says so plainly for anything else.
	authed("POST /api/template", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Template string }
		json.NewDecoder(r.Body).Decode(&body)
		if !strings.Contains(body.Template, "area_name") {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, "fakeha: only the area_name template is implemented")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, e := range house {
			if e.area != "" {
				fmt.Fprintf(w, "%s\t%s\n", e.id, e.area)
			}
		}
	})

	authed("POST /api/services/{domain}/{service}", func(w http.ResponseWriter, r *http.Request) {
		s.call(w, r.PathValue("domain"), r.PathValue("service"), r)
	})

	// Not behind authed(): Home Assistant authenticates a socket with the token
	// in the auth message, not with a header, so demanding the header here would
	// refuse a client that is doing exactly the right thing.
	m.HandleFunc("GET /api/websocket", s.websocket)

	return m
}

// all returns every state in the order the house declares them.
func (s *server) all() []State {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]State, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, *s.states[id])
	}
	return out
}

// flip is the one writer of state: it changes it, stamps it, and tells every
// subscriber. Both a service call and the idle ticker go through here, so the
// demo can never show a change the socket did not report.
func (s *server) flip(id, want string) (*State, bool) {
	s.mu.Lock()
	st, ok := s.states[id]
	if !ok {
		s.mu.Unlock()
		return nil, false
	}
	old := *st
	if want == "toggle" {
		if st.State == "on" {
			want = "off"
		} else {
			want = "on"
		}
	}
	// A cover takes open_cover/close_cover, a lock takes lock/unlock; the state
	// word they leave behind is not the one they were called with.
	st.State = want
	if strings.HasPrefix(id, "cover.") {
		st.Attributes["current_position"] = map[bool]int{true: 100, false: 0}[want == "open"]
	}
	if strings.HasPrefix(id, "light.") {
		// A light carries a brightness only while it is on, which is what makes
		// the details panel worth opening in the demo.
		if want == "on" {
			st.Attributes["brightness"] = 180
		} else {
			delete(st.Attributes, "brightness")
		}
	}
	now := time.Now().UTC()
	st.LastChanged, st.LastUpdated, st.LastReported = now, now, now
	st.Attributes["friendly_name"] = old.Attributes["friendly_name"]
	out := *st
	subs := make([]*subscriber, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()

	for _, sub := range subs {
		s.send(sub, map[string]any{
			"id":   1,
			"type": "event",
			"event": map[string]any{
				"event_type": "state_changed",
				"data": map[string]any{
					"entity_id": id,
					"old_state": old,
					"new_state": out,
				},
			},
		})
	}
	return &out, true
}

func (s *server) send(sub *subscriber, msg any) {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = wsjson.Write(ctx, sub.conn, msg)
}

// call answers a service call with the states it changed, which is the only
// confirmation a write gets in has.
func (s *server) call(w http.ResponseWriter, domain, service string, r *http.Request) {
	// Home Assistant takes the target under either key and in either shape — one
	// id or a list — so the demo does too. Decoding it as a typed field would
	// reject half of what the client legitimately sends.
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	var ids []string
	for _, key := range []string{"entity_id", "entity_ids"} {
		switch v := body[key].(type) {
		case string:
			ids = append(ids, v)
		case []any:
			for _, e := range v {
				if s, ok := e.(string); ok {
					ids = append(ids, s)
				}
			}
		}
	}

	want := "on"
	switch service {
	case "turn_on", "open", "lock", "open_cover":
		want = "on"
	case "turn_off", "unlock", "close", "close_cover":
		want = "off"
	case "toggle":
		want = "toggle"
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": fmt.Sprintf("Service %s.%s is not implemented by fakeha", domain, service),
		})
		return
	}

	changed := []State{}
	for _, id := range ids {
		if st, ok := s.flip(id, want); ok {
			changed = append(changed, *st)
		}
	}
	writeJSON(w, http.StatusOK, changed)
}

// idleFlips is what the demo's "live" claim rests on: a house that is quietly
// doing something while you watch it. Only read-only and long-lived entities
// move, so nothing on screen contradicts itself and no action is undone.
func (s *server) idleFlips() {
	ids := []string{
		"sensor.hall_temperature", "sensor.kitchen_humidity", "sensor.power_meter",
		"sensor.co2", "binary_sensor.hallway_motion", "sensor.outside_temperature",
	}
	tick := time.NewTicker(2500 * time.Millisecond)
	defer tick.Stop()
	for i := 0; ; i++ {
		<-tick.C
		id := ids[i%len(ids)]
		s.mu.Lock()
		st := s.states[id]
		next := st.State
		if strings.HasPrefix(id, "binary_sensor.") {
			if next == "on" {
				next = "off"
			} else {
				next = "on"
			}
		} else {
			v, _ := strconv.ParseFloat(st.State, 64)
			next = fmt.Sprintf("%.1f", v+0.2)
		}
		s.mu.Unlock()
		s.flip(id, next)
	}
}

// logbook is canned but shaped like the real thing: oldest first, some entries
// naming an entity and some not.
func (s *server) logbook(filter []string) []map[string]any {
	day := time.Now().AddDate(0, 0, -1)
	rows := []struct {
		id, name, what string
		offset         time.Duration
	}{
		{"binary_sensor.hallway_motion", "Hallway Motion", "on", 9 * time.Hour},
		{"light.kitchen_light", "Kitchen Light", "on", 8 * time.Hour},
		{"", "Good Night", "Triggered by automation.good_night", 7 * time.Hour},
		{"light.kitchen_light", "Kitchen Light", "off", 6 * time.Hour},
		{"media_player.living_room_tv", "Living Room TV", "off", 5 * time.Hour},
		{"cover.living_room_blinds", "Living Room Blinds", "open", 3 * time.Hour},
		{"lock.front_door", "Front Door Lock", "unlocked", 2 * time.Hour},
		{"", "Morning Lights", "Triggered by automation.morning_lights", time.Hour},
	}
	want := map[string]bool{}
	for _, f := range filter {
		if f != "" {
			want[f] = true
		}
	}
	out := []map[string]any{}
	for _, r := range rows {
		if len(want) > 0 && !want[r.id] {
			continue
		}
		e := map[string]any{
			"when":  day.Add(r.offset).Format("2006-01-02T15:04:05Z"),
			"name":  r.name,
			"state": r.what,
		}
		if r.id != "" {
			e["entity_id"] = r.id
			e["domain"] = strings.SplitN(r.id, ".", 2)[0]
		} else {
			e["message"] = r.what
			e["domain"] = "automation"
		}
		out = append(out, e)
	}
	return out
}

// history returns one list per entity, which is what /api/history/period gives:
// a list of lists, not a flat one.
func (s *server) history(ids []string) [][]State {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := [][]State{}
	for _, id := range ids {
		st, ok := s.states[id]
		if !ok {
			continue
		}
		old := *st
		old.Attributes = nil
		// A history is a sequence of changes, so it alternates: whatever the
		// entity is not now, then now, then now again.
		if old.State == "on" {
			old.State = "off"
		} else {
			old.State = "on"
		}
		older := old
		older.LastChanged = old.LastChanged.Add(-3 * time.Hour)
		older.LastUpdated = older.LastChanged
		newest := old
		newest.State = st.State
		newest.LastChanged = old.LastChanged.Add(time.Hour)
		newest.LastUpdated = newest.LastChanged
		out = append(out, []State{older, old, newest})
	}
	return out
}

// services is the registry shape: grouped by domain, then by service name. The
// TUI derives its action menus from this, so it has to be grouped the way HA
// groups it rather than flattened.
func services() []map[string]any {
	on := []string{"turn_on", "turn_off", "toggle"}
	group := func(domain string, fields map[string]any, names ...string) map[string]any {
		inner := map[string]any{}
		for _, n := range names {
			svc := map[string]any{
				"name":        n,
				"description": n,
				"fields":      fields,
				"target": map[string]any{
					"entity": []any{map[string]any{"domain": []string{domain}}},
				},
			}
			inner[n] = svc
		}
		return map[string]any{"domain": domain, "services": inner}
	}
	out := []map[string]any{
		group("light", map[string]any{
			"brightness": map[string]any{"description": "Brightness 0-255"},
			"transition": map[string]any{"description": "Transition time in seconds"},
		}, on...),
		group("switch", nil, on...),
		group("fan", nil, on...),
		group("media_player", nil, on...),
		group("climate", nil, on...),
		group("automation", nil, on...),
		group("script", nil, on...),
		group("cover", nil, "open_cover", "close_cover", "stop_cover"),
		group("lock", nil, "lock", "unlock", "open"),
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["domain"].(string) < out[j]["domain"].(string) })
	return out
}

// websocket performs the five-step exchange the client expects and then keeps
// the connection: auth_required, auth, auth_ok, subscribe_events, result. A
// client that skipped or reordered any of it would hang here rather than
// quietly working.
func (s *server) websocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	sub := &subscriber{conn: conn}
	defer func() {
		s.mu.Lock()
		delete(s.subs, sub)
		s.mu.Unlock()
		conn.CloseNow()
	}()

	ctx := r.Context()
	if err := wsjson.Write(ctx, conn, map[string]any{
		"ha_version": "2026.9.4",
		"type":       "auth_required",
		"message":    "password or token",
	}); err != nil {
		return
	}
	var auth struct {
		Type  string `json:"type"`
		Token string `json:"access_token"`
	}
	if err := wsjson.Read(ctx, conn, &auth); err != nil {
		return
	}
	if auth.Type != "auth" || auth.Token != *token {
		wsjson.Write(ctx, conn, map[string]any{
			"type": "auth_invalid", "message": "Invalid access token or password",
		})
		return
	}
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "auth_ok", "ha_version": "2026.9.4"}); err != nil {
		return
	}
	// One reader for the life of the connection: it is what processes the
	// client's pings and notices a close.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			var in map[string]any
			if err := wsjson.Read(ctx, conn, &in); err != nil {
				return
			}
			if id, ok := in["id"].(float64); ok {
				s.send(sub, map[string]any{"id": int(id), "type": "result", "success": true, "result": nil})
			}
		}
	}()

	// An authenticated client is a subscriber, whether or not it subscribed: the
	// demo has one event type and sends it either way.
	s.mu.Lock()
	s.subs[sub] = true
	s.mu.Unlock()

	select {
	case <-closed:
	case <-r.Context().Done():
	}
}
