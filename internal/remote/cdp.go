package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// cdp speaks the Chrome DevTools Protocol over --remote-debugging-pipe:
// NUL-terminated JSON messages, with flattened sessions (sessionId on each message).
type cdp struct {
	w      io.Writer
	wmu    sync.Mutex
	nextID atomic.Int64

	pmu     sync.Mutex
	pending map[int64]chan cdpMessage

	events chan cdpMessage
	done   chan struct{}
	err    error
}

type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return fmt.Sprintf("cdp: %s (%d)", e.Message, e.Code) }

var errClosed = errors.New("browser closed")

func newCDP(r io.Reader, w io.Writer) *cdp {
	c := &cdp{w: w, pending: map[int64]chan cdpMessage{}, events: make(chan cdpMessage, 512), done: make(chan struct{})}
	go c.readLoop(r)
	return c
}

func (c *cdp) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<20)
	defer func() {
		c.pmu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.pmu.Unlock()
		close(c.events)
		close(c.done)
	}()
	for {
		raw, err := br.ReadBytes(0)
		if err != nil {
			c.err = err
			return
		}
		var m cdpMessage
		if err := json.Unmarshal(raw[:len(raw)-1], &m); err != nil {
			continue
		}
		if m.ID != 0 {
			c.pmu.Lock()
			ch := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.pmu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		// Events are handled in order by one goroutine; if it falls far behind we drop
		// screencast frames rather than stall the pipe.
		if m.Method == "Page.screencastFrame" {
			select {
			case c.events <- m:
			default:
				var p struct {
					SessionID int `json:"sessionId"`
				}
				_ = json.Unmarshal(m.Params, &p)
				c.Send(m.SessionID, "Page.screencastFrameAck", map[string]int{"sessionId": p.SessionID})
			}
			continue
		}
		c.events <- m
	}
}

func (c *cdp) write(m cdpMessage, params any) error {
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		m.Params = b
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, 0)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	select {
	case <-c.done:
		return errClosed
	default:
	}
	_, err = c.w.Write(b)
	return err
}

// Call sends a command and waits for its result, decoded into out when non-nil.
func (c *cdp) Call(ctx context.Context, session, method string, params, out any) error {
	id := c.nextID.Add(1)
	ch := make(chan cdpMessage, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()
	if err := c.write(cdpMessage{ID: id, Method: method, SessionID: session}, params); err != nil {
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
		return err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return errClosed
		}
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-ctx.Done():
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
		return ctx.Err()
	}
}

// Send issues a command without waiting for the answer (input events, frame acks).
func (c *cdp) Send(session, method string, params any) {
	id := c.nextID.Add(1)
	_ = c.write(cdpMessage{ID: id, Method: method, SessionID: session}, params)
}
