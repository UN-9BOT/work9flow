package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/protocol"
)

// errWSNoStorage is returned by the WebSocket handler when the
// server has no storage wired.
var errWSNoStorage = errors.New("runtime: no storage wired")

// subscriber is a single WebSocket listener for one run.
type subscriber struct {
	ch chan domain.Event
}

// publishBroker is a per-run pub/sub. The Server owns one.
type publishBroker struct {
	mu          sync.Mutex
	subscribers map[string]map[*subscriber]struct{}
}

func newPublishBroker() *publishBroker {
	return &publishBroker{subscribers: map[string]map[*subscriber]struct{}{}}
}

// subscribe registers a new subscriber for runID. The returned cancel
// removes the subscriber and closes its channel.
func (b *publishBroker) subscribe(runID string) (*subscriber, func()) {
	sub := &subscriber{ch: make(chan domain.Event, 64)}
	b.mu.Lock()
	if b.subscribers[runID] == nil {
		b.subscribers[runID] = map[*subscriber]struct{}{}
	}
	b.subscribers[runID][sub] = struct{}{}
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		delete(b.subscribers[runID], sub)
		if len(b.subscribers[runID]) == 0 {
			delete(b.subscribers, runID)
		}
		b.mu.Unlock()
		close(sub.ch)
	}
	return sub, cancel
}

// publish fans out e to every subscriber of runID. Slow subscribers
// are dropped (they will see a gap on reconnect).
func (b *publishBroker) publish(runID string, e domain.Event) {
	b.mu.Lock()
	subs := make([]*subscriber, 0, len(b.subscribers[runID]))
	for s := range b.subscribers[runID] {
		subs = append(subs, s)
	}
	b.mu.Unlock()
	for _, s := range subs {
		select {
		case s.ch <- e:
		default:
		}
	}
}

// handleRunEventsStream upgrades the request to WebSocket and
// streams events for the run: first the history with seq > after,
// then live events as they are published.
func (s *Server) handleRunEventsStream(w http.ResponseWriter, r *http.Request) {
	if s.opts.Repo == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage", errWSNoStorage)
		return
	}
	id := r.PathValue("id")
	if _, err := s.opts.Repo.GetRun(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "not_found", err)
		return
	}
	var after int64
	if q := r.URL.Query().Get("after"); q != "" {
		if v, err := strconv.ParseInt(q, 10, 64); err == nil {
			after = v
		}
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		// Accept already wrote the error response.
		return
	}
	wsCtx, wsCancel := context.WithCancel(context.Background())
	defer wsCancel()

	// Start a goroutine that continuously reads from the connection.
	// coder/websocket requires a concurrent reader to handle control
	// frames; without it, pings are never answered and the client
	// eventually drops the connection.
	readErr := make(chan error, 1)
	go func() {
		_, _, err := conn.Read(wsCtx)
		readErr <- err
	}()

	sub, unsub := s.broker.subscribe(id)
	defer unsub()

	// 1. Replay history.
	if events, err := s.opts.Repo.EventsAfter(r.Context(), id, after); err == nil {
		for _, e := range events {
			data, err := json.Marshal(protocol.FromEvent(e))
			if err != nil {
				data = []byte(`{}`)
			}
			writeCtx, writeCancel := context.WithTimeout(wsCtx, 5*time.Second)
			werr := conn.Write(writeCtx, websocket.MessageText, data)
			writeCancel()
			if werr != nil {
				_ = conn.Close(websocket.StatusInternalError, "write failed")
				return
			}
		}
	}

	// 2. Forward live events.
	for {
		select {
		case <-wsCtx.Done():
			return
		case e, ok := <-sub.ch:
			if !ok {
				return
			}
			data, err := json.Marshal(protocol.FromEvent(e))
			if err != nil {
				continue
			}
			writeCtx, writeCancel := context.WithTimeout(wsCtx, 5*time.Second)
			werr := conn.Write(writeCtx, websocket.MessageText, data)
			writeCancel()
			if werr != nil {
				return
			}
		case err := <-readErr:
			_ = err
			return
		}
	}
}

// fmt keeps the linter happy in builds without debug prints.
var _ = fmt.Sprintf
