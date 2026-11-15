// Package cdp is a zero-dependency Chrome DevTools Protocol client over
// Chromium's --remote-debugging-pipe transport.
//
// Pipe framing (measured with a probe against this machine's Chromium,
// 2026-11-14): messages are JSON objects terminated by a NUL byte (0x00).
// A newline-terminated message is never answered; a NUL-terminated one is
// answered immediately. The parent writes commands to the child's fd 3 and
// reads events/responses from the child's fd 4.
package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

type rawMessage struct {
	ID        *int            `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *rpcError       `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message) }

// Event is a CDP event pushed by the browser (method + params + session).
type Event struct {
	Method    string
	Params    json.RawMessage
	SessionID string
}

// Client speaks CDP over the debugging pipe.
type Client struct {
	w   io.Writer
	r   io.Reader
	mu  sync.Mutex
	seq int

	pendingMu sync.Mutex
	pending   map[int]chan rawMessage

	events chan Event
	done   chan struct{}
}

// New wraps the write end (child fd 3) and read end (child fd 4).
func New(w io.Writer, r io.Reader) *Client {
	c := &Client{
		w:       w,
		r:       r,
		pending: make(map[int]chan rawMessage),
		events:  make(chan Event, 128),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Events returns the channel on which browser-pushed events arrive.
func (c *Client) Events() <-chan Event { return c.events }

// Close stops the read loop.
func (c *Client) Close() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

// Call sends a method and waits for its response (or ctx timeout).
// sessionID is "" for browser-level (no session) commands.
func (c *Client) Call(ctx context.Context, sessionID, method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	c.seq++
	id := c.seq
	c.mu.Unlock()

	msg := map[string]interface{}{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	body = append(body, 0x00)

	ch := make(chan rawMessage, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	if _, err := c.w.Write(body); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case m := <-ch:
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("cdp: timeout waiting for %s", method)
	}
}

// readLoop splits the incoming byte stream on NUL and routes messages.
func (c *Client) readLoop() {
	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	for {
		select {
		case <-c.done:
			return
		default:
		}
		n, err := c.r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			for {
				i := bytes.IndexByte(buf.Bytes(), 0x00)
				if i < 0 {
					break
				}
				raw := buf.Next(i)
				buf.Next(1) // consume the NUL
				var m rawMessage
				if err := json.Unmarshal(raw, &m); err != nil {
					continue
				}
				c.route(m)
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *Client) route(m rawMessage) {
	if m.ID != nil {
		c.pendingMu.Lock()
		ch, ok := c.pending[*m.ID]
		c.pendingMu.Unlock()
		if ok {
			ch <- m
		}
		return
	}
	if m.Method != "" {
		select {
		case c.events <- Event{Method: m.Method, Params: m.Params, SessionID: m.SessionID}:
		default:
		}
	}
}

// WaitForEvent blocks until an event with the given method arrives for the
// given session (or ctx expires). Events for other methods are re-queued
// only if they arrive later; unmatched events are dropped.
func (c *Client) WaitForEvent(ctx context.Context, sessionID, method string) (json.RawMessage, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev := <-c.events:
			if ev.Method == method && (sessionID == "" || ev.SessionID == sessionID) {
				return ev.Params, nil
			}
			// Not the event we wait for; drop it (callers that need
			// multiple event kinds should drain Events() themselves).
		case <-time.After(60 * time.Second):
			return nil, fmt.Errorf("cdp: timeout waiting for %s", method)
		}
	}
}
