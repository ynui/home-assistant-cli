package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Home Assistant has no push channel over REST, so the WebSocket API at
// /api/websocket is the only way to be told about a change instead of asking.
// We use one feature of it — a subscription to state_changed — and that is
// enough to retire the hover poll as the normal path: the row you act on
// updates the moment Home Assistant applies the action, and a group count is as
// fresh as the server, not as fresh as the last time you looked at that group.
//
// The exchange is fixed and short:
//
//	<- {"type":"auth_required"}
//	-> {"type":"auth","access_token":…}
//	<- {"type":"auth_ok"}
//	-> {"id":1,"type":"subscribe_events","event_type":"state_changed"}
//	<- {"id":1,"type":"result","success":true}
//	<- {"id":1,"type":"event","event":{"data":{"entity_id":…,"new_state":{…}}}}
//
// Nothing here decides policy. Every failure comes back as a message and
// uiModel decides: a refused connection falls back to polling, a dropped one is
// reconnected, a rejected token is reported and given up on.

// wsPingInterval is how often we prove the socket is still alive. A TCP
// connection through NAT or a proxy can die without either end noticing, and a
// silently stale list is worse than a reconnecting one.
const wsPingInterval = 30 * time.Second

// wsPingTimeout bounds the pong we wait for. A ping that never answers is the
// only evidence we get of a half-open connection.
const wsPingTimeout = 10 * time.Second

// wsReadLimit caps one message. The library's 32 KiB default is close to what a
// chatty entity (media player, calendar) can push in a single state_changed.
const wsReadLimit = 1 << 20

// wsMaxReconnect caps the backoff between dropped-connection retries.
const wsMaxReconnect = 15 * time.Second

// wsConnectTries is how many times a refused handshake is retried before the
// program settles for polling. Enough to cover a Home Assistant that is still
// starting; few enough that a proxy which blocks the upgrade is not asked
// forever.
const wsConnectTries = 3

// wsSend is what we write, and it must stay exactly this small. Home Assistant
// validates every message against a schema and rejects unknown keys, so a
// struct shared with the read path fails the handshake with "Auth message
// incorrectly formatted" the moment a receive-only field is added to it — which
// is what happened: `success` and `event` rode along in the auth message.
type wsSend struct {
	ID          int    `json:"id,omitempty"`
	Type        string `json:"type"`
	AccessToken string `json:"access_token,omitempty"`
	EventType   string `json:"event_type,omitempty"`
}

// wsRecv is what we read: auth replies, result acks and events as one union,
// which is fine on the way in.
type wsRecv struct {
	ID      int    `json:"id,omitempty"`
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
	Success bool   `json:"success"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Event struct {
		Data struct {
			EntityID string `json:"entity_id"`
			// NewState is null when the entity was removed, which is the only
			// way the server tells us a row should leave the list.
			NewState *State `json:"new_state"`
		} `json:"data"`
	} `json:"event"`
}

// Messages from the socket to the model. Each one is a single tea.Msg so the
// whole connection is driven from Update, with no goroutine of our own.
//
// The socket messages carry the connection they arrived on, and the model drops
// any that is not the one it currently holds. A read armed on a socket we have
// already replaced would otherwise report the old connection's death as the
// current one's, closing a live socket and reconnecting forever.

type wsConnMsg struct{ conn *websocket.Conn }

// wsFailedMsg is a handshake that never completed: no socket, so the model
// falls back to polling and says why.
type wsFailedMsg struct{ err error }

// wsGoneMsg is a connection that dropped. The model reconnects.
type wsGoneMsg struct {
	conn *websocket.Conn
	err  error
}

// wsRejectedMsg is a token Home Assistant refused. Retrying would only spam, so
// this one is final and the reason is worth printing.
type wsRejectedMsg struct{ err error }

// wsStateMsg is one state_changed event.
type wsStateMsg struct {
	conn     *websocket.Conn
	entityID string
	state    *State // nil when the entity was removed
}

// wsIdleMsg is a frame that is not an event (a pong, a stray result). The read
// is re-armed and nothing else happens.
type wsIdleMsg struct{ conn *websocket.Conn }

// wsAliveMsg says a ping was answered.
type wsAliveMsg struct{ conn *websocket.Conn }

// wsURL turns the configured base URL into the WebSocket endpoint. HA serves it
// under the same origin at /api/websocket.
func wsURL(base string) string {
	trimmed := strings.TrimRight(base, "/")
	switch {
	case strings.HasPrefix(trimmed, "https://"):
		return "wss://" + strings.TrimPrefix(trimmed, "https://") + "/api/websocket"
	case strings.HasPrefix(trimmed, "http://"):
		return "ws://" + strings.TrimPrefix(trimmed, "http://") + "/api/websocket"
	default:
		return ""
	}
}

// wsStateChanged is the one event type the TUI subscribes to. An empty
// eventType subscribes to everything, which is what `has watch` does.
const wsStateChanged = "state_changed"

// wsCmd opens the socket, authenticates with the same long-lived token the REST
// calls use, and subscribes. It returns once the subscription is confirmed, so
// wsConnMsg means "you are genuinely subscribed" rather than "connected".
func wsCmd(c *Client, timeout time.Duration, eventType string) tea.Cmd {
	return func() tea.Msg {
		conn, err := wsConnect(c, timeout, eventType)
		switch {
		case errors.Is(err, errBadToken):
			conn.CloseNow()
			return wsRejectedMsg{err}
		case err != nil:
			if conn != nil {
				conn.CloseNow()
			}
			return wsFailedMsg{err}
		}
		return wsConnMsg{conn}
	}
}

// errBadToken marks the one failure worth reporting differently: the token is
// wrong, so nothing will work until the user runs `has login`.
var errBadToken = errors.New("token rejected by Home Assistant")

// wsConnect performs the handshake. It returns a non-nil conn on failure too, so
// the caller can always close whatever was opened.
func wsConnect(c *Client, timeout time.Duration, eventType string) (*websocket.Conn, error) {
	url := wsURL(c.base)
	if url == "" {
		return nil, fmt.Errorf("cannot open a websocket for %q", c.base)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// The context covers the handshake only; it is not the connection's lifetime.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(wsReadLimit)

	var m wsRecv
	if err := wsjson.Read(ctx, conn, &m); err != nil {
		return conn, err
	}
	if m.Type != "auth_required" {
		return conn, fmt.Errorf("expected auth_required, got %q", m.Type)
	}
	if err := wsjson.Write(ctx, conn,
		wsSend{Type: "auth", AccessToken: c.token}); err != nil {
		return conn, err
	}
	if err := wsjson.Read(ctx, conn, &m); err != nil {
		return conn, err
	}
	if m.Type == "auth_invalid" {
		return conn, fmt.Errorf("%w: %s", errBadToken, m.Message)
	}
	if m.Type != "auth_ok" {
		return conn, fmt.Errorf("expected auth_ok, got %q", m.Type)
	}
	if err := wsjson.Write(ctx, conn, wsSend{
		ID:        1,
		Type:      "subscribe_events",
		EventType: eventType,
	}); err != nil {
		return conn, err
	}
	// The subscription is answered with a result message carrying the same id.
	// Waiting for it means "connected" really means subscribed.
	if err := wsjson.Read(ctx, conn, &m); err != nil {
		return conn, err
	}
	if !m.Success {
		msg := "subscribe_events was refused"
		if m.Error != nil {
			msg += ": " + m.Error.Message
		}
		return conn, errors.New(msg)
	}
	return conn, nil
}

// wsReadCmd blocks for one message. The model re-arms it after every message, so
// the socket is only read from where the rest of the program is decided.
func wsReadCmd(conn *websocket.Conn) tea.Cmd {
	return func() tea.Msg {
		_, data, err := conn.Read(context.Background())
		if err != nil {
			return wsGoneMsg{conn: conn, err: err}
		}
		var m wsRecv
		if err := json.Unmarshal(data, &m); err != nil {
			// A frame we cannot read is not a reason to drop a good socket.
			return wsIdleMsg{conn: conn}
		}
		if m.Type != "event" || m.Event.Data.EntityID == "" {
			return wsIdleMsg{conn: conn}
		}
		return wsStateMsg{conn: conn, entityID: m.Event.Data.EntityID, state: m.Event.Data.NewState}
	}
}

// wsPingCmd proves the socket is alive. An unanswered ping is the only way to
// notice a connection that died without closing.
func wsPingCmd(conn *websocket.Conn) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), wsPingTimeout)
		defer cancel()
		if err := conn.Ping(ctx); err != nil {
			return wsGoneMsg{conn: conn, err: err}
		}
		return wsAliveMsg{conn: conn}
	}
}

// wsBackoff spaces out reconnect attempts so a server that is down is not asked
// once a second. Capped: coming back late is worse than a few extra tries.
func wsBackoff(tries int) time.Duration {
	return min(time.Second<<min(tries, 4), wsMaxReconnect)
}

// wsEvent is one frame of the general event stream, kept apart from wsRecv on
// purpose. wsRecv decodes a state_changed straight into typed fields, which is
// what the TUI wants for the event it subscribed to; anything else needs the raw
// bytes of an event the TUI would only throw away, and a second unmarshal per
// state_changed to get them.
type wsEvent struct {
	Type  string `json:"type"`
	Event struct {
		EventType string          `json:"event_type"`
		Data      json.RawMessage `json:"data"`
	} `json:"event"`
}

// wsStream subscribes to one event type and hands every event to fn until the
// socket dies or ctx is done. It is not a tea.Cmd because nothing else in the
// program drives it: `has watch` owns the terminal until it is stopped.
//
// It reports whether the subscription ever succeeded, so the caller can tell a
// server that was never reachable (a few tries, then give up) from one that went
// away mid-tail (keep going: a house that restarts should not need a restart).
func wsStream(ctx context.Context, c *Client, timeout time.Duration, eventType string,
	fn func(eventType string, data json.RawMessage)) (bool, error) {

	conn, err := wsConnect(c, timeout, eventType)
	if err != nil {
		// wsConnect hands back whatever it opened even on failure - a socket that
		// got as far as auth_invalid is still a socket, and Home Assistant holds
		// the session until it times out.
		if conn != nil {
			conn.CloseNow()
		}
		return false, err
	}
	defer conn.CloseNow()
	go wsKeepAlive(ctx, conn)

	for {
		// A fresh frame each time: unmarshalling into a reused one leaves a field
		// the frame did not carry holding the previous frame's value.
		var frame wsEvent
		if _, data, err := conn.Read(ctx); err != nil {
			return true, err
		} else if err := json.Unmarshal(data, &frame); err != nil {
			continue // a frame we cannot read is not a reason to drop a good socket
		}
		if frame.Type != "event" || len(frame.Event.Data) == 0 {
			continue // a pong, a stray result
		}
		fn(frame.Event.EventType, frame.Event.Data)
	}
}

// wsKeepAlive proves the socket is alive from outside the read loop, which is
// the same job the TUI's ping tick does from inside. A ping that is never
// answered closes the connection: a tail that has gone quiet must fail loudly
// rather than sit there implying it is still live.
func wsKeepAlive(ctx context.Context, conn *websocket.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wsPingInterval):
		}
		pctx, cancel := context.WithTimeout(ctx, wsPingTimeout)
		err := conn.Ping(pctx)
		cancel()
		if err != nil {
			conn.CloseNow()
			return
		}
	}
}
