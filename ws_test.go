package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/coder/websocket"
)

// wsAcceptedKeys are the only keys a real Home Assistant accepts in a message
// we send. It validates every message against a schema and rejects anything
// else, which is how a receive-only field leaking into the auth message turns
// into "Auth message incorrectly formatted".
var wsAcceptedKeys = map[string]bool{
	"id": true, "type": true, "access_token": true, "event_type": true,
}

// fakeWS serves the WebSocket half of Home Assistant's API: the same four-step
// handshake, the same strictness, then whatever the test pushes. The payloads
// are written as raw JSON on purpose — copying the wire format is the point,
// not this file's structs.
func fakeWS(t *testing.T, after func(conn *websocket.Conn)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := context.Background()
		send := func(v string) bool {
			return conn.Write(ctx, websocket.MessageText, []byte(v)) == nil
		}
		recv := func() string {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return ""
			}
			msg := strings.TrimSpace(string(data))
			if bad := wsUnknownKey(msg); bad != "" {
				send(`{"type":"auth_invalid","message":"not a valid option at '` + bad + `'"}`)
				return ""
			}
			return msg
		}
		if !send(`{"type":"auth_required"}`) {
			return
		}
		auth := recv()
		if !strings.Contains(auth, `"access_token":"secret"`) {
			send(`{"type":"auth_invalid","message":"Invalid access token"}`)
			return
		}
		if !send(`{"type":"auth_ok"}`) {
			return
		}
		sub := recv()
		if !strings.Contains(sub, `"subscribe_events"`) {
			return
		}
		if !send(`{"id":1,"type":"result","success":true,"result":null}`) {
			return
		}
		after(conn)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// wsUnknownKey names the first key in a message the real server would refuse,
// so the fake fails the way reality does instead of accepting anything.
func wsUnknownKey(msg string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(msg), &m); err != nil {
		return ""
	}
	for k := range m {
		if !wsAcceptedKeys[k] {
			return k
		}
	}
	return ""
}

func wsClient(t *testing.T, url string) *Client {
	t.Helper()
	return &Client{base: url, token: "secret"}
}

// Home Assistant rejects any key it does not expect, so what we send must be
// exactly these fields and nothing else. It shipped with `success` and `event`
// attached from the shared read/write struct, and every handshake failed with
// "Auth message incorrectly formatted" while the fake server passed happily.
func TestWSSentMessagesCarryNoExtraKeys(t *testing.T) {
	auth, err := json.Marshal(wsSend{Type: "auth", AccessToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := json.Marshal(wsSend{ID: 1, Type: "subscribe_events", EventType: "state_changed"})
	if err != nil {
		t.Fatal(err)
	}
	for what, raw := range map[string][]byte{"auth": auth, "subscribe_events": sub} {
		if bad := wsUnknownKey(string(raw)); bad != "" {
			t.Errorf("%s would be rejected for the unknown key %q: %s", what, bad, raw)
		}
	}
	if !strings.Contains(string(auth), `"access_token":"secret"`) {
		t.Errorf("auth message lost its token: %s", auth)
	}
}

func TestWSURL(t *testing.T) {
	cases := map[string]string{
		"http://ha.local:8123":    "ws://ha.local:8123/api/websocket",
		"https://ha.example.com":  "wss://ha.example.com/api/websocket",
		"https://ha.example.com/": "wss://ha.example.com/api/websocket",
		"ha.local:8123":           "",
		"":                        "",
	}
	for in, want := range cases {
		if got := wsURL(in); got != want {
			t.Errorf("wsURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The whole path: dial, authenticate, subscribe, and decode one state_changed
// event exactly as Home Assistant sends it.
func TestWSDeliversStateChanged(t *testing.T) {
	url := fakeWS(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		conn.Write(ctx, websocket.MessageText, []byte(
			`{"id":1,"type":"event","event":{"event_type":"state_changed","data":{`+
				`"entity_id":"light.kitchen_light",`+
				`"old_state":{"entity_id":"light.kitchen_light","state":"off"},`+
				`"new_state":{"entity_id":"light.kitchen_light","state":"on",`+
				`"attributes":{"friendly_name":"Kitchen Light"}}}}}`))
		time.Sleep(50 * time.Millisecond)
	})
	c := wsClient(t, url)

	msg := wsCmd(c, 5*time.Second, wsStateChanged)()
	conn, ok := msg.(wsConnMsg)
	if !ok {
		t.Fatalf("handshake failed: %#v", msg)
	}
	defer conn.conn.CloseNow()

	ev, ok := wsReadCmd(conn.conn)().(wsStateMsg)
	if !ok {
		t.Fatalf("expected an event, got %#v", ev)
	}
	if ev.entityID != "light.kitchen_light" || ev.state == nil || ev.state.State != "on" {
		t.Errorf("event = %+v", ev)
	}
	if ev.state.Attributes["friendly_name"] != "Kitchen Light" {
		t.Errorf("attributes did not survive: %+v", ev.state.Attributes)
	}
}

// A state_changed with a null new_state is how the server says an entity is
// gone, so the row has to leave the list instead of rendering empty.
func TestWSNullStateRemovesTheEntity(t *testing.T) {
	url := fakeWS(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		conn.Write(ctx, websocket.MessageText, []byte(
			`{"id":1,"type":"event","event":{"event_type":"state_changed","data":{`+
				`"entity_id":"light.garage_light","old_state":{"entity_id":"light.garage_light","state":"on"},`+
				`"new_state":null}}}`))
		time.Sleep(50 * time.Millisecond)
	})
	conn, ok := wsCmd(wsClient(t, url), 5*time.Second, wsStateChanged)().(wsConnMsg)
	if !ok {
		t.Fatal("handshake failed")
	}
	defer conn.conn.CloseNow()

	ev, ok := wsReadCmd(conn.conn)().(wsStateMsg)
	if !ok || ev.entityID != "light.garage_light" || ev.state != nil {
		t.Fatalf("event = %#v", ev)
	}

	m := uiFixture()
	m.conn = ev.conn // the model must be subscribed for an event to apply
	if left := len(entityRows(apply(m, ev))); left != 3 {
		t.Errorf("garage_light should be gone, %d rows left", left)
	}
}

// apply sends one message to a model and hands back the model it produced.
func apply(m *uiModel, msg tea.Msg) *uiModel {
	got, _ := m.Update(msg)
	return got.(*uiModel)
}

// A server that is down, or a proxy that refuses the upgrade, must leave the
// program usable rather than blank: the hover poll carries on and the reason is
// on screen.
func TestWSFailureFallsBackToPolling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	msg := wsCmd(wsClient(t, url), time.Second, wsStateChanged)()
	failed, ok := msg.(wsFailedMsg)
	if !ok {
		t.Fatalf("expected wsFailedMsg, got %#v", msg)
	}
	if failed.err == nil {
		t.Error("a failed handshake must say why")
	}

	m := uiFixture()
	m.polling, m.wantWS, m.interval = true, true, pollInterval
	// A refused handshake is retried a few times in case the server is still
	// starting, then the program settles for polling and says so.
	got := m
	for i := 0; i < wsConnectTries; i++ {
		got = apply(got, failed)
		if !got.polling {
			t.Fatal("polling should keep running when there is no socket")
		}
	}
	if !strings.Contains(got.status, "polling instead") {
		t.Errorf("status should explain the fallback, got %q", got.status)
	}
	if got.wantWS {
		t.Error("the retries should be finite")
	}
	if got.liveState() != "polling" {
		t.Errorf("live state = %q", got.liveState())
	}
}

// A rejected token will be rejected forever; say so and stop asking.
func TestWSRejectedTokenIsFinal(t *testing.T) {
	url := fakeWS(t, func(conn *websocket.Conn) {})
	c := wsClient(t, url)
	c.token = "wrong"

	msg, ok := wsCmd(c, 5*time.Second, wsStateChanged)().(wsRejectedMsg)
	if !ok {
		t.Fatalf("expected wsRejectedMsg, got %#v", msg)
	}
	if !strings.Contains(msg.err.Error(), "token rejected") {
		t.Errorf("error = %v", msg.err)
	}

	m := uiFixture()
	m.wantWS = true
	got := apply(m, msg)
	if got.wantWS {
		t.Error("a rejected token should stop the retries")
	}
	if !strings.Contains(got.status, "has login") {
		t.Errorf("status should point at the fix, got %q", got.status)
	}
}

// The live socket in the TUI gets its events as tea.Msg through the model;
// `has watch` has no model, so the same handshake has to serve a plain
// callback. Cancelling the context is what stops the tail.
func TestWSStreamDeliversEventsToAPlainCallback(t *testing.T) {
	url := fakeWS(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		for i, state := range []string{"on", "off"} {
			conn.Write(ctx, websocket.MessageText, []byte(
				`{"id":1,"type":"event","event":{"event_type":"state_changed","data":{`+
					`"entity_id":"light.kitchen_light","old_state":{"state":"off"},`+
					`"new_state":{"entity_id":"light.kitchen_light","state":"`+state+`",`+
					`"last_updated":"2026-10-17T09:00:0`+string(rune('1'+i))+`Z"}}}}`))
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var got []string
	connected, err := wsStream(ctx, wsClient(t, url), 5*time.Second, "", func(eventType string, data json.RawMessage) {
		if eventType != "state_changed" {
			t.Errorf("event type = %q", eventType)
		}
		var ch watchChange
		if json.Unmarshal(data, &ch) != nil {
			t.Errorf("unreadable event: %s", data)
		}
		got = append(got, ch.EntityID+"="+ch.NewState.State)
		if len(got) == 2 {
			cancel()
		}
	})
	if !connected {
		t.Error("a subscription that worked must be reported as connected")
	}
	if err == nil {
		t.Error("a cancelled context ends the tail with an error, not a clean stop")
	}
	if strings.Join(got, " ") != "light.kitchen_light=on light.kitchen_light=off" {
		t.Errorf("events = %v", got)
	}
}

// A server that was never reachable is reported apart from one that dropped, so
// `has watch` can give up instead of retrying a dead host forever.
func TestWSStreamReportsAnUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	connected, err := wsStream(context.Background(), wsClient(t, url), time.Second, "",
		func(string, json.RawMessage) {})
	if connected {
		t.Error("a refused handshake is not a connection")
	}
	if err == nil {
		t.Error("a refused handshake must say why")
	}
}

// The reconnect backoff has to grow, or a server that is down is asked once a
// second forever.
func TestWSBackoffGrowsAndCaps(t *testing.T) {
	m := uiFixture()
	var last time.Duration
	for i := 0; i < 8; i++ {
		d := m.backoff()
		if d < last {
			t.Fatalf("backoff shrank: %v after %v", d, last)
		}
		if d > wsMaxReconnect {
			t.Fatalf("backoff %v exceeds the cap %v", d, wsMaxReconnect)
		}
		last = d
		m.tries++
	}
}

// A read armed on a socket we have already replaced must not close the live
// one. Two in-flight messages from the old connection — a state event and a
// dropped-socket error — both arrive after a reconnect; the model must ignore
// both rather than tear down the new connection and reconnect forever.
func TestWSStaleMessagesDropped(t *testing.T) {
	m := uiFixture()
	m.conn = &websocket.Conn{} // the live connection
	live := m.conn

	// An event from a stale connection is ignored.
	stale := &websocket.Conn{}
	got := apply(m, wsStateMsg{conn: stale, entityID: "light.kitchen_light", state: &State{State: "STALE"}})
	if got.conn != live {
		t.Error("a stale state event must not touch the live connection")
	}
	for _, s := range got.states {
		if s.EntityID == "light.kitchen_light" && s.State == "STALE" {
			t.Error("a stale state event was applied")
		}
	}

	// A dropped-stale-connection error is ignored too.
	got = apply(m, wsGoneMsg{conn: stale, err: errors.New("old socket died")})
	if got.conn != live {
		t.Error("a stale gone message must not close the live connection")
	}
	if got.wantWS {
		t.Error("a stale gone message must not schedule a reconnect")
	}

	// A ping from a stale connection does not re-arm.
	got = apply(m, wsAliveMsg{conn: stale})
	if got.conn != live {
		t.Error("a stale alive message must not touch the live connection")
	}

	// The live connection's own messages still work.
	got = apply(m, wsStateMsg{conn: live, entityID: "light.kitchen_light", state: &State{State: "on"}})
	if got.states[0].State != "on" {
		t.Error("a live state event should be applied")
	}
}
