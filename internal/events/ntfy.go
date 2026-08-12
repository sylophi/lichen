// Package events is lichen's change-notification layer over ntfy. Each
// daemon holds one long-lived streaming connection, idle until something
// happens. lichen publishes a nudge whenever it pushes the sync repo, and
// the repo's optional webhook POSTs into the same topic for pushes lichen
// did not make. The randomly generated topic is the channel's secret, and
// an event only ever triggers work the hourly pass would do anyway:
// content trust comes from git, never from an event.
package events

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Event struct {
	ID      string `json:"id"`
	Time    int64  `json:"time"`
	Kind    string `json:"event"`
	Message string `json:"message"`
}

// Nudge is the message lichen itself publishes: "the sync repo moved,
// pull it". Origin is the publishing machine's hostname, which lets that
// machine's own daemon skip the event because the push it describes came
// from there. A webhook payload has no origin, so it always reconciles.
type Nudge struct {
	Origin string `json:"origin,omitempty"`
}

type Client struct {
	Server string
	Topic  string
}

// TopicURL is the single definition of the topic's URL: the address
// GitHub webhooks POST to, the one Publish posts to, and the base
// Subscribe streams from must never drift apart.
func (c Client) TopicURL() string {
	return c.Server + "/" + c.Topic
}

func (c Client) Publish(message string) error {
	req, err := http.NewRequest("POST", c.TopicURL(), strings.NewReader(message))
	if err != nil {
		return err
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy publish: %s", resp.Status)
	}
	return nil
}

// Subscribe streams the topic's messages until ctx is cancelled. It
// reconnects with capped backoff, passing since=<last seen time> so events
// missed while disconnected (laptop asleep, network blip) are replayed from
// ntfy's server-side cache, so handlers must be idempotent, and reconciles
// are. Duplicate delivery at the since boundary is filtered by message ID.
func (c Client) Subscribe(ctx context.Context, onConnect func(), onError func(error), handler func(Event)) {
	var since int64
	var lastID string
	backoff := time.Second
	for ctx.Err() == nil {
		url := c.TopicURL() + "/json"
		if since > 0 {
			url += "?since=" + strconv.FormatInt(since, 10)
		}
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			onError(err)
			return
		}
		connectedAt := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("ntfy subscribe: %s", resp.Status)
			resp.Body.Close()
		}
		if err != nil {
			if ctx.Err() == nil {
				onError(err)
			}
		} else {
			onConnect()
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				line := bytes.TrimSpace(sc.Bytes())
				if len(line) == 0 {
					continue
				}
				var ev Event
				if json.Unmarshal(line, &ev) != nil {
					continue
				}
				// Keepalives advance the cursor too, so an idle stream that
				// drops doesn't replay hours of old messages on reconnect.
				if ev.Time > since {
					since = ev.Time
				}
				if ev.Kind == "message" && ev.ID != lastID {
					lastID = ev.ID
					handler(ev)
				}
			}
			resp.Body.Close()
		}
		// Back off before EVERY reconnect, including one after a stream that
		// returned 200 and closed immediately (a captive portal or proxy).
		// Without this that case is a tight, log-flooding, CPU-burning loop.
		// A connection that stayed healthy past ntfy's keepalive interval
		// resets the backoff so normal long-lived drops reconnect promptly.
		if err == nil && time.Since(connectedAt) > 60*time.Second {
			backoff = time.Second
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > time.Minute {
			backoff = time.Minute
		}
	}
}
