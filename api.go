package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Client struct {
	base  string
	token string
	http  *http.Client
}

type State struct {
	EntityID    string         `json:"entity_id"`
	State       string         `json:"state"`
	Attributes  map[string]any `json:"attributes"`
	LastChanged time.Time      `json:"last_changed"`
	LastUpdated time.Time      `json:"last_updated"`
}

type ServiceField struct {
	Description string `json:"description"`
	Example     any    `json:"example"`
	// Required marks arguments a service cannot be called without. We never
	// offer an action that needs one, because we have nothing to pass.
	Required bool `json:"required"`
}

// ServiceTarget lists the entity domains a service can be aimed at. It is nil
// for services that take no entity, which is how we tell "script.mute_stereo"
// (global) apart from "script.turn_on" (targets script entities).
type ServiceTarget struct {
	Entity []struct {
		Domain []string `json:"domain"`
	} `json:"entity"`
}

// Service is one callable service, flattened out of Home Assistant's grouped
// /api/services response.
type Service struct {
	Domain      string                  `json:"domain"`
	Name        string                  `json:"name"`
	Description string                  `json:"description"`
	Fields      map[string]ServiceField `json:"fields"`
	Target      *ServiceTarget          `json:"target"`
}

// entityDomains is the set of entity domains this service accepts.
func (s Service) entityDomains() []string {
	if s.Target == nil {
		return nil
	}
	var out []string
	for _, e := range s.Target.Entity {
		out = append(out, e.Domain...)
	}
	return out
}

// ServerConfig is the part of /api/config that is worth reading. The rest of
// the response - time zone, components, unit system - is not used by anything
// here, so it is not decoded.
type ServerConfig struct {
	Version      string `json:"version"`
	LocationName string `json:"location_name"`
}

var errNotConfigured = errors.New("no server configured: run `has login`, or pass --url and --token")

func newClient(cfg Config, timeout time.Duration) (*Client, error) {
	if cfg.URL == "" || cfg.Token == "" {
		return nil, errNotConfigured
	}
	return &Client{
		base:  strings.TrimRight(cfg.URL, "/"),
		token: cfg.Token,
		http:  &http.Client{Timeout: timeout},
	}, nil
}

func (c *Client) do(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, newAPIError(res.StatusCode, raw)
	}
	return raw, nil
}

// newAPIError keeps Home Assistant's own message, so callers can print it
// verbatim. There is nothing to branch on beyond the status, so it is a plain
// error rather than a type.
func newAPIError(status int, raw []byte) error {
	var e struct {
		Message string `json:"message"`
	}
	if status == http.StatusUnauthorized {
		// coded so main can give the 401 its own exit code without the
		// message changing. ws.go has errBadToken for the same condition on
		// the socket path; that one is shaped for the TUI's status line, so
		// the two stay separate.
		return coded{exitAuth, errors.New("token rejected (401); run `has login` to enter a new one")}
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return errors.New(e.Message)
	}
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		msg = http.StatusText(status)
	}
	return fmt.Errorf("HTTP %d: %s", status, msg)
}

// ping verifies the token. Home Assistant returns 401 for a bad one.
func (c *Client) ping() error {
	_, err := c.do(http.MethodGet, "/api/", nil)
	return err
}

func (c *Client) serverConfig() (ServerConfig, error) {
	var out ServerConfig
	b, err := c.do(http.MethodGet, "/api/config", nil)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(b, &out)
}

func (c *Client) states() ([]State, error) {
	b, err := c.do(http.MethodGet, "/api/states", nil)
	if err != nil {
		return nil, err
	}
	return unmarshalStates(b)
}

// services flattens the grouped response into one entry per service, sorted by
// domain then name so repeated calls give identical output.
func (c *Client) services() ([]Service, error) {
	b, err := c.do(http.MethodGet, "/api/services", nil)
	if err != nil {
		return nil, err
	}
	var grouped []struct {
		Domain   string `json:"domain"`
		Services map[string]struct {
			// No Name: the map key is the service name, and it is the one the
			// call has to send back.
			Description string                  `json:"description"`
			Fields      map[string]ServiceField `json:"fields"`
			Target      *ServiceTarget          `json:"target"`
		} `json:"services"`
	}
	if err := json.Unmarshal(b, &grouped); err != nil {
		return nil, err
	}
	out := []Service{}
	for _, g := range grouped {
		for name, body := range g.Services {
			out = append(out, Service{
				Domain:      g.Domain,
				Name:        name,
				Description: body.Description,
				Fields:      body.Fields,
				Target:      body.Target,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Domain != out[j].Domain {
			return out[i].Domain < out[j].Domain
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// state fetches a single entity. The UI uses it to refresh only the row under
// the cursor, instead of pulling every state on each tick.
func (c *Client) state(entityID string) (State, error) {
	var s State
	b, err := c.do(http.MethodGet, "/api/states/"+url.PathEscape(entityID), nil)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// call fires a service and returns the states it changed: Home Assistant
// answers a service call with the new state of every entity it touched, so the
// caller can report what happened instead of assuming it worked. data may be
// nil for services that take no arguments.
func (c *Client) call(domain, service string, data map[string]any) ([]State, error) {
	if data == nil {
		data = map[string]any{}
	}
	b, err := c.do(http.MethodPost,
		"/api/services/"+url.PathEscape(domain)+"/"+url.PathEscape(service), data)
	if err != nil {
		return nil, err
	}
	// A service that changes nothing answers with an empty list. Anything we
	// cannot read is treated the same: the call itself succeeded, and the
	// error that matters is the one do() already returned.
	var out []State
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, nil
	}
	return out, nil
}

// history returns state changes since t, newest last. minimal_response and
// no_attributes keep the payload small.
//
// The period must be RFC3339, not a Unix epoch: Home Assistant parses this
// path segment as a datetime string and answers 400 "Invalid datetime" for a
// bare integer.
func (c *Client) history(entities []string, since time.Time) ([]State, error) {
	if len(entities) == 0 {
		return nil, errors.New("no entity given")
	}
	q := url.Values{}
	q.Set("filter_entity_id", strings.Join(entities, ","))
	q.Set("minimal_response", "")
	q.Set("no_attributes", "")
	path := "/api/history/period/" + since.UTC().Format(time.RFC3339) + "?" + q.Encode()
	b, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	groups, err := unmarshalStates(b)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return groups[i].LastChanged.Before(groups[j].LastChanged)
	})
	return groups, nil
}

// unmarshalStates decodes both a flat []State (from /api/states) and the
// list-of-lists returned by the history endpoint.
func unmarshalStates(b []byte) ([]State, error) {
	var flat []State
	if err := json.Unmarshal(b, &flat); err == nil {
		return flat, nil
	}
	var groups [][]State
	if err := json.Unmarshal(b, &groups); err != nil {
		return nil, err
	}
	var out []State
	for _, g := range groups {
		out = append(out, g...)
	}
	return out, nil
}

// LogEntry is one row of Home Assistant's logbook: something that happened, and
// what it was to. The shape is not fixed - a state change records `state`, a
// script or automation run records `message`, and an entry about something with
// no entity at all records neither and has no entity_id - so only `when` and
// `name` are always there. Copied from a real /api/logbook response.
type LogEntry struct {
	When     time.Time `json:"when"`
	Name     string    `json:"name"`
	EntityID string    `json:"entity_id"`
	State    string    `json:"state"`
	Message  string    `json:"message"`
	Domain   string    `json:"domain"`
}

// what is what happened, in the logbook's own words.
func (e LogEntry) what() string {
	if e.State != "" {
		return e.State
	}
	return e.Message
}

// logbook returns what happened during a day, oldest first, which is the order
// the endpoint answers in and the order `history` prints in too.
//
// The filter is ?entity= and takes a comma-separated list; it is the only way
// to narrow the day down. The endpoint answers POST with 405, so this is a GET
// with no request body.
func (c *Client) logbook(day time.Time, entities []string) ([]LogEntry, error) {
	path := "/api/logbook/" + day.Format("2006-01-02")
	if len(entities) > 0 {
		path += "?" + url.Values{"entity": {strings.Join(entities, ",")}}.Encode()
	}
	b, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var out []LogEntry
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// areasTemplate renders one line per entity that is in an area. Home Assistant
// does not include area data in /api/states, so a template render is the only
// way to ask; it accounts for both entity-level and device-level assignments.
const areasTemplate = "{% for s in states %}{% set a = area_name(s.entity_id) %}" +
	"{% if a %}{{ s.entity_id }}\t{{ a }}\n{% endif %}{% endfor %}"

// template renders a Jinja template on the server and returns the result
// verbatim. The answer is plain text, not JSON: a template can render a number,
// a yaml document or a word, and the caller asked for exactly that.
func (c *Client) template(tpl string) (string, error) {
	b, err := c.do(http.MethodPost, "/api/template", map[string]string{"template": tpl})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// entityAreas fetches the mapping from entity_id to friendly area name.
func (c *Client) entityAreas() (map[string]string, error) {
	rendered, err := c.template(areasTemplate)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, line := range strings.Split(rendered, "\n") {
		line = strings.TrimRight(line, "\r")
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			out[parts[0]] = parts[1]
		}
	}
	return out, nil
}
