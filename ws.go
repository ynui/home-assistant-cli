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

// Home Assistant has no push channel over REST, so the WebSocket API is how
// the TUI gets told about changes instead of polling. One feature is used: a
// subscription to state_changed.
//
// The exchange:
//
//	<- {"type":"auth_required"}
//	-> {"type":"auth","access_token":…}
//	<- {"type":"auth_ok"}
//	-> {"id":1,"type":"subscribe_events","event_type":"state_changed"}
//	<- {"id":1,"type":"result","success":true}
//	<- {"id":1,"type":"event","event":{"data":{"entity_id":…,"new_state":{…}}}}
//
// Nothing here decides policy. Every failure comes back as a message and the
// uiModel decides: refused means fall back to polling, dropped means
// reconnect, a rejected token is final.

// wsPingInterval is how often we prove the socket is still alive; a connection
// through a proxy can die without either end noticing.
const wsPingInterval = 30 * time.Second

// wsPingTimeout bounds the pong we wait for.
const wsPingTimeout = 10 * time.Second

// wsReadLimit caps one message; the library's 32 KiB default is too small for
// chatty entities.
const wsReadLimit = 1 << 20

// wsMaxReconnect caps the backoff between dropped-connection retries.
const wsMaxReconnect = 15 * time.Second

// wsConnectTries is how many times a refused handshake is retried before the
// program settles for polling: enough for a HA that is still starting, few
// enough that a proxy blocking the upgrade is not asked forever.
const wsConnectTries = 3

// wsSend is what we write, and it must stay this small: HA rejects unknown
// keys, so a struct shared with the read path breaks the handshake. Two
// readers would need its fields anyway.
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

// Messages from the socket to the model, one tea.Msg each so everything is
// driven from Update. The socket messages carry the connection they arrived
// on, and the model drops any that is not the one it currently holds - a read
// armed on an old socket would otherwise report that socket's death as the
// live one's.

type wsConnMsg struct{ conn *websocket.Conn }

// wsFailedMsg is a handshake that never completed; the model falls back to
// polling.
type wsFailedMsg struct{ err error }

// wsGoneMsg is a connection that dropped; the model reconnects.
type wsGoneMsg struct {
	conn *websocket.Conn
	err  error
}

// wsRejectedMsg is a token HA refused; final, no retry.
type wsRejectedMsg struct{ err error }

// wsStateMsg is one state_changed event. NewState is nil when the entity was
// removed.
type wsStateMsg struct {
	conn     *websocket.Conn
	entityID string
	state    *State
}

// wsIdleMsg is a frame that is not an event (a pong, a stray result).
type wsIdleMsg struct{ conn *websocket.Conn }

// wsAliveMsg says a ping was answered.
type wsAliveMsg struct{ conn *websocket.Conn }

// wsURL turns a base URL into the WebSocket endpoint at /api/websocket.
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
	// The context covers the handshake only, not the connection's lifetime.
	ctx, cancel := context.WithTimeout(ctx, timeout)
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
	// The subscription is answered with a result carrying the same id; waiting
	// for it means "connected" really means subscribed.
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

// wsReadCmd blocks for one message; the model re-arms it after every message.
func wsReadCmd(conn *websocket.Conn) tea.Cmd {
	return func() tea.Msg {
		_, data, err := conn.Read(ctx)
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

// wsPingCmd proves the socket is alive.
func wsPingCmd(conn *websocket.Conn) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, wsPingTimeout)
		defer cancel()
		if err := conn.Ping(ctx); err != nil {
			return wsGoneMsg{conn: conn, err: err}
		}
		return wsAliveMsg{conn: conn}
	}
}

// wsBackoff spaces out reconnect attempts, capped.
func wsBackoff(tries int) time.Duration {
	return min(time.Second<<min(tries, 4), wsMaxReconnect)
}

// wsEvent is one frame of a general event stream, kept apart from wsRecv: the
// TUI wants a state_changed decoded into typed fields, `has watch` wants the
// raw bytes, and merging them would cost the TUI an unmarshal per event for
// data it throws away.
type wsEvent struct {
	Type  string `json:"type"`
	Event struct {
		EventType string          `json:"event_type"`
		Data      json.RawMessage `json:"data"`
	} `json:"event"`
}

// wsStream subscribes to one event type and hands every event to fn until the
// socket dies or ctx is done. Not a tea.Cmd: `has watch` owns the terminal.
// The bool reports whether the subscription ever succeeded, so the caller can
// tell an unreachable server (give up after a few tries) from one that went
// away mid-tail (keep going; a house that restarts should not need this
// program restarted too).
func wsStream(ctx context.Context, c *Client, timeout time.Duration, eventType string,
	fn func(eventType string, data json.RawMessage)) (bool, error) {

	conn, err := wsConnect(c, timeout, eventType)
	if err != nil {
		// wsConnect hands back its socket even on failure; close it.
		if conn != nil {
			conn.CloseNow()
		}
		return false, err
	}
	defer conn.CloseNow()
	go wsKeepAlive(ctx, conn)

	for {
		// A fresh frame each time: unmarshalling into a reused one leaves
		// fields holding the previous frame's values.
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

// wsKeepAlive proves the socket is alive from outside the read loop (`has
// watch`'s version of the TUI's ping tick). An unanswered ping closes the
// connection: a tail that has gone quiet must fail loudly.
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
