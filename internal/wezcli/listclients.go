package wezcli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ClientInfo mirrors one row of `wezterm cli list-clients --format json`.
// Field naming follows wezterm's snake_case schema; tags below map them.
//
// The two timestamp fields are the only non-trivial decode: wezterm emits
// `last_input` and `idle_time` as RFC3339 / Go-style `Duration.String()`
// formatted strings respectively in current builds, but historical builds
// have used unitless seconds. clientInfoRaw + the (un)marshallers below
// accept both shapes; a parse failure leaves the field zero and is logged
// as a TODO upstream-format check rather than a hard error (the §13.3
// poller cares about LastInput as a relative ordering, not an absolute
// instant).
type ClientInfo struct {
	ClientID      string
	Username      string
	Hostname      string
	PID           int
	FocusedPaneID int
	LastInput     time.Time
	IdleTime      time.Duration
}

// clientInfoRaw is the on-the-wire intermediate. The two flexible fields
// are kept as json.RawMessage so the unmarshaller below can probe both
// shapes (string vs number) per upstream version.
type clientInfoRaw struct {
	ClientID      string          `json:"client_id"`
	Username      string          `json:"username"`
	Hostname      string          `json:"hostname"`
	PID           int             `json:"pid"`
	FocusedPaneID int             `json:"focused_pane_id"`
	LastInput     json.RawMessage `json:"last_input"`
	IdleTime      json.RawMessage `json:"idle_time"`
}

// ListClients runs `cli list-clients --format json`. Per-call ceiling and
// failure mapping match List (§14.1).
func (c *Client) ListClients(ctx context.Context) ([]ClientInfo, error) {
	out, err := c.run(ctx, "cli", "list-clients", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("%w: cli list-clients: %v", ErrMuxUnreachable, err)
	}
	out = []byte(strings.TrimSpace(string(out)))
	var raw []clientInfoRaw
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("%w: cli list-clients json: %v", ErrMuxUnreachable, err)
	}
	clients := make([]ClientInfo, len(raw))
	for i, r := range raw {
		clients[i] = ClientInfo{
			ClientID:      r.ClientID,
			Username:      r.Username,
			Hostname:      r.Hostname,
			PID:           r.PID,
			FocusedPaneID: r.FocusedPaneID,
			LastInput:     parseFlexibleTime(r.LastInput),
			IdleTime:      parseFlexibleDuration(r.IdleTime),
		}
	}
	return clients, nil
}

// parseFlexibleTime accepts either an RFC3339 / RFC3339Nano string or a
// JSON number of seconds-since-epoch; on failure returns the zero Time.
// The §13.3 poller only uses LastInput as a relative ordering input via
// pickMostRecentClient, so a zero Time falls to the lexicographic
// tie-break and the algorithm remains stable.
func parseFlexibleTime(raw json.RawMessage) time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}
	}
	// Try JSON string first (RFC3339).
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, time.RFC1123} {
			if t, err := time.Parse(layout, s); err == nil {
				return t
			}
		}
		// Fall back to Go-Duration-style "1h2m3s" treated as elapsed-from-epoch?
		// Do not — too ambiguous; leave zero.
		return time.Time{}
	}
	// Fall back to JSON number (seconds since epoch).
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		whole, frac := int64(f), f-float64(int64(f))
		return time.Unix(whole, int64(frac*float64(time.Second))).UTC()
	}
	return time.Time{}
}

// parseFlexibleDuration accepts either a string ("1h2m3s") or a JSON
// number (interpreted as seconds, possibly fractional). On failure
// returns 0; same fail-soft policy as parseFlexibleTime.
func parseFlexibleDuration(raw json.RawMessage) time.Duration {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
		// "{secs:..,nanos:..}" upstream variant — try parsing as number.
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return time.Duration(n * float64(time.Second))
		}
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return time.Duration(f * float64(time.Second))
	}
	return 0
}
