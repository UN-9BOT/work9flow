// Package worker drives engine.Step for active workflow runs.
//
// work9flowd runs a Worker that periodically scans storage for runs
// in a non-terminal state and calls engine.Step on each one. The
// worker is the runtime half of the engine; without it HTTP calls
// create runs but nothing executes them.
//
// When cfg.DSHEndpoint is empty (no provider wired) the worker is
// not started — the runtime remains a pure CRUD service which is
// what unit tests and the smoke script rely on.
package worker

import (
	"context"
	"time"

	"github.com/charmbracelet/log"

	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/engine"
	"github.com/unbot/work9flow/internal/storage"
)

// Options configures a Worker.
type Options struct {
	Engine  *engine.Engine
	Repo    storage.Repo
	Logger  *log.Logger
	// Interval between scans; default 2s.
	Interval time.Duration
	// Concurrency caps how many runs are stepped in parallel.
	// Default 1 (sequential) — DSH sessions are heavy and serial
	// stepping keeps event ordering predictable.
	Concurrency int
}

// Worker periodically drives engine.Step for active runs.
type Worker struct {
	opts Options
	tick time.Duration
}

// New returns a Worker configured from opts.
func New(opts Options) *Worker {
	interval := opts.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &Worker{opts: opts, tick: interval}
}

// Run blocks until ctx is cancelled, ticking every Interval. Each
// tick lists non-terminal runs and calls engine.Step on each.
func (w *Worker) Run(ctx context.Context) {
	if w.opts.Engine == nil {
		w.opts.Logger.Warn("worker disabled: nil engine")
		return
	}
	if w.opts.Repo == nil {
		w.opts.Logger.Warn("worker disabled: nil repo")
		return
	}
	w.opts.Logger.Info("worker started", "interval", w.tick)
	t := time.NewTicker(w.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.opts.Logger.Info("worker stopped")
			return
		case <-t.C:
			w.tickOnce(ctx)
		}
	}
}

// TickOnceForTest performs one scan + step cycle. Tests use this
// instead of Run so they don't depend on ticker scheduling.
func (w *Worker) TickOnceForTest(ctx context.Context, repo storage.Repo) {
	if repo == nil {
		repo = w.opts.Repo
	}
	runs, err := repo.ListRuns(ctx)
	if err != nil {
		if w.opts.Logger != nil {
			w.opts.Logger.Error("worker list runs", "err", err)
		}
		return
	}
	for i := range runs {
		run := runs[i]
		if domain.IsTerminal(run.State) {
			continue
		}
		if err := w.opts.Engine.Step(ctx, run.ID); err != nil {
			if err == engine.ErrTerminalRun {
				continue
			}
			if w.opts.Logger != nil {
				w.opts.Logger.Warn("worker step", "run_id", run.ID, "err", err)
			}
		}
	}
}

func (w *Worker) tickOnce(ctx context.Context) {
	w.TickOnceForTest(ctx, w.opts.Repo)
}
