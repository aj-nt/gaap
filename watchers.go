package gaap

import (
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/aj-nt/gaap/internal/status"
	pb "github.com/aj-nt/vassago-sdk/proto"
)

// Watching the daemon for task completions.
//
// None of this touches the DAG. The watchers own their own set of task IDs, fed
// by dispatchFeed, and hand what they see to the loop as events. That is the
// single-writer rule: a watcher that mutated a status is a race waiting for a
// test to drive both goroutines at once.

// startWatcher begins observing the daemon, once per orchestrator.
//
// Subscription when enabled, polling otherwise. A subscription that cannot open
// falls back immediately; one that closes without ever delivering an event is
// not a working subscription and also falls back, because waiting on silence is
// how a run hangs instead of failing.
func (o *Orchestrator) startWatcher() {
	o.watchOnce.Do(func() {
		o.seedWatcher()
		if o.subscribeFallbackToPoll {
			err := o.watchSubscription()
			if err == nil {
				return
			}
			slog.Info("subscription unavailable, falling back to polling", "error", err)
		}
		o.pollFallbacks.Add(1)
		go o.watchPoll()
	})
}

// seedWatcher tells the watcher which tasks are already in play. A resumed run
// re-dispatches nothing, so without this the watcher would watch an empty set and
// the run would wait out the clock.
func (o *Orchestrator) seedWatcher() {
	for _, node := range o.dag.nodes {
		if node.Status != status.TaskReady && node.Status != status.TaskClaimed {
			continue
		}
		select {
		case o.dispatchFeed <- node.ID:
		default:
			slog.Warn("dispatch feed full while seeding the watcher", "task_id", node.ID)
		}
	}
}

// watchPoll queries the daemon for the tasks this run dispatched and posts what
// it finds. It keeps its own set of IDs, fed by the loop through dispatchFeed,
// and never reads or writes the DAG.
func (o *Orchestrator) watchPoll() {
	// Detection: if the daemon is not connected at all, simulate completions.
	// The simulation mutates the DAG, so it runs on the loop, not here.
	if _, err := o.daemon.GetTask(o.ctx, "probe-detection"); err != nil && err.Error() == "vassago not connected" {
		slog.Info("polling: daemon unavailable, using simulation fallback")
		o.post(Event{Type: EventSimulate})
		return
	}

	pollInterval := time.Duration(o.cfg.PollIntervalSec) * time.Second
	maxWait := time.Duration(o.cfg.MaxWaitSec) * time.Second
	slog.Info("polling: querying daemon for task completions",
		"poll_interval_sec", o.cfg.PollIntervalSec,
		"max_wait_sec", o.cfg.MaxWaitSec)

	ids := make(map[string]struct{})
	poll := func() {
		for id := range ids {
			entry, err := o.daemon.GetTask(o.ctx, id)
			if err != nil || entry == nil {
				continue
			}
			evt := taskEventFromClient(entry)
			o.post(Event{Type: EventTaskUpdate, Task: &evt})
			if evt.Terminal() {
				delete(ids, id)
			}
		}
	}

	// Drain the IDs the loop already handed over, and look once immediately:
	// waiting a whole interval to make the first query is latency no caller asked
	// for, and it hid dispatch bugs behind a tick.
	for {
		select {
		case id := <-o.dispatchFeed:
			ids[id] = struct{}{}
			continue
		default:
		}
		break
	}
	poll()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	start := time.Now()

	for {
		select {
		case <-o.ctx.Done():
			return
		case <-o.stopCh:
			return
		case id := <-o.dispatchFeed:
			ids[id] = struct{}{}
		case <-ticker.C:
			poll()
			if time.Since(start) > maxWait {
				slog.Warn("polling: timeout waiting for workers",
					"elapsed_sec", int(time.Since(start).Seconds()))
				o.post(Event{Type: EventTimeout})
				return
			}
		}
	}
}

// watchSubscription opens a gRPC subscription for task status changes and posts
// each one. Returns an error when the stream cannot be opened, so the caller can
// fall back to polling; once open, the reader goroutine owns the stream and only
// posts — every DAG write stays on the loop.
func (o *Orchestrator) watchSubscription() error {
	stream, err := o.daemon.Subscribe(o.ctx, &pb.SubscribeRequest{
		AgentId: "gaap-orchestrator",
		Targets: []string{"task"},
	})
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	slog.Info("subscribed to task events via gRPC stream")

	go func() {
		received := 0
		for {
			event, err := stream.Recv()
			if err != nil {
				if err != io.EOF {
					slog.Warn("subscription stream error", "error", err)
				} else {
					slog.Info("subscription stream closed (io.EOF)")
				}
				if received == 0 {
					// A stream that delivered nothing is not a working
					// subscription: fall back rather than wait on silence.
					o.pollFallbacks.Add(1)
					slog.Info("subscription delivered no events, falling back to polling")
					go o.watchPoll()
				}
				return
			}
			if event.Task == nil {
				continue
			}
			received++
			evt := taskEventFromProto(event.Task)
			o.post(Event{Type: EventTaskUpdate, Task: &evt})
		}
	}()

	// The stream can be silent for reasons that are not errors, so the wait
	// still needs a clock. This is the only timer in the subscription path.
	go func() {
		maxWait := time.Duration(o.cfg.MaxWaitSec) * time.Second
		select {
		case <-o.ctx.Done():
		case <-o.stopCh:
		case <-time.After(maxWait):
			o.post(Event{Type: EventTimeout})
		}
	}()

	return nil
}

// stopWatcher ends the observation goroutines. Safe to call more than once.
func (o *Orchestrator) stopWatcher() {
	o.stopOnce.Do(func() {
		close(o.stopCh)
		if o.workerStop != nil {
			close(o.workerStop)
			slog.Info("auto-workers stopped")
		}
	})
}
