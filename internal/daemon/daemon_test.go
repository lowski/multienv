package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lowski/multienv/internal/docker"
)

func TestRelevant(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ev   docker.Event
		want bool
	}{
		{"container start", docker.Event{Kind: docker.EventKindContainer, Action: "start"}, true},
		{"container die", docker.Event{Kind: docker.EventKindContainer, Action: "die"}, true},
		{"container exec_create", docker.Event{Kind: docker.EventKindContainer, Action: "exec_create"}, false},
		{"container resize", docker.Event{Kind: docker.EventKindContainer, Action: "resize"}, false},
		{"network connect", docker.Event{Kind: docker.EventKindNetwork, Action: "connect"}, true},
		{"network destroy", docker.Event{Kind: docker.EventKindNetwork, Action: "destroy"}, true},
		{"other", docker.Event{Kind: docker.EventKindOther, Action: "anything"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relevant(tc.ev); got != tc.want {
				t.Errorf("relevant(%+v) = %v, want %v", tc.ev, got, tc.want)
			}
		})
	}
}

func TestDescribePrefersName(t *testing.T) {
	t.Parallel()
	ev := docker.Event{
		Kind:       docker.EventKindContainer,
		Action:     "start",
		ActorID:    "abc123def4567890",
		Attributes: map[string]string{"name": "myapp_api_1"},
	}
	got := describe(ev)
	if !strings.Contains(got, "myapp_api_1") {
		t.Errorf("describe should include container name, got %q", got)
	}
	if strings.Contains(got, "abc123def456") {
		t.Errorf("describe should drop id when name is present, got %q", got)
	}
}

func TestDescribeFallsBackToShortID(t *testing.T) {
	t.Parallel()
	ev := docker.Event{
		Kind:    docker.EventKindContainer,
		Action:  "destroy",
		ActorID: "abc123def4567890",
	}
	got := describe(ev)
	if !strings.Contains(got, "abc123def456") {
		t.Errorf("describe should include short id, got %q", got)
	}
}

// fakeSource pumps events on demand and signals an error when its
// internal context is cancelled.
type fakeSource struct {
	events chan docker.Event
	errs   chan error
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		events: make(chan docker.Event, 16),
		errs:   make(chan error, 1),
	}
}

func (f *fakeSource) Events(ctx context.Context) (<-chan docker.Event, <-chan error) {
	return f.events, f.errs
}

func TestDaemonRunsInitialReconcile(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	src := newFakeSource()
	d := &Daemon{
		Events:    src,
		Reconcile: func(ctx context.Context) error { calls.Add(1); return nil },
		Out:       &bytes.Buffer{},
		Debounce:  10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(d, ctx)

	// Give the goroutine a moment to perform the startup reconcile.
	if !waitFor(func() bool { return calls.Load() >= 1 }, 200*time.Millisecond) {
		t.Fatalf("startup reconcile did not run; calls=%d", calls.Load())
	}
	cancel()
	<-done
	if calls.Load() != 1 {
		t.Errorf("expected exactly 1 reconcile (startup), got %d", calls.Load())
	}
}

func TestDaemonDebouncesBurstOfEvents(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var mu sync.Mutex
	var reasons []string
	out := &syncBuffer{}
	src := newFakeSource()
	d := &Daemon{
		Events: src,
		Reconcile: func(ctx context.Context) error {
			calls.Add(1)
			mu.Lock()
			reasons = append(reasons, "fired")
			mu.Unlock()
			return nil
		},
		Out:      out,
		Debounce: 50 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(d, ctx)
	defer func() { cancel(); <-done }()

	waitFor(func() bool { return calls.Load() >= 1 }, 200*time.Millisecond)
	startupCalls := calls.Load()

	// Fire a burst of relevant events within the debounce window.
	for range 5 {
		src.events <- docker.Event{Kind: docker.EventKindContainer, Action: "start", Attributes: map[string]string{"name": "svc"}}
	}

	if !waitFor(func() bool { return calls.Load() > startupCalls }, 500*time.Millisecond) {
		t.Fatalf("expected debounced reconcile after burst; calls=%d", calls.Load())
	}
	// Allow a little extra time to confirm no second reconcile arrives
	// from the same burst.
	time.Sleep(120 * time.Millisecond)
	if got := calls.Load() - startupCalls; got != 1 {
		t.Errorf("expected exactly 1 reconcile from 5-event burst, got %d", got)
	}

	if !strings.Contains(out.String(), "+4 more") {
		t.Errorf("expected log to mention coalesced count, got: %q", out.String())
	}
}

func TestDaemonIgnoresIrrelevantEvents(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	src := newFakeSource()
	d := &Daemon{
		Events:    src,
		Reconcile: func(ctx context.Context) error { calls.Add(1); return nil },
		Out:       &bytes.Buffer{},
		Debounce:  30 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(d, ctx)
	defer func() { cancel(); <-done }()

	waitFor(func() bool { return calls.Load() >= 1 }, 200*time.Millisecond)
	baseline := calls.Load()

	for range 5 {
		src.events <- docker.Event{Kind: docker.EventKindContainer, Action: "exec_start"}
		src.events <- docker.Event{Kind: docker.EventKindContainer, Action: "resize"}
	}
	time.Sleep(150 * time.Millisecond)
	if got := calls.Load(); got != baseline {
		t.Errorf("expected no reconcile after irrelevant events, calls went from %d to %d", baseline, got)
	}
}

func TestDaemonResubscribesOnStreamError(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var subs atomic.Int32

	// Each subscription gets its own channel pair so we can model an
	// error followed by a fresh subscription.
	d := &Daemon{
		Reconcile: func(ctx context.Context) error { calls.Add(1); return nil },
		Out:       &bytes.Buffer{},
		Debounce:  20 * time.Millisecond,
	}

	// Override resubscribeBackoff via a small custom EventSource that
	// emits one error then settles. We can't change the package const,
	// so we just tolerate the 2s wait via a longer test timeout.
	d.Events = eventSourceFunc(func(ctx context.Context) (<-chan docker.Event, <-chan error) {
		n := subs.Add(1)
		ev := make(chan docker.Event, 1)
		er := make(chan error, 1)
		if n == 1 {
			er <- errors.New("boom")
		} else {
			ev <- docker.Event{Kind: docker.EventKindContainer, Action: "start", Attributes: map[string]string{"name": "svc"}}
		}
		return ev, er
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := runAsync(d, ctx)
	defer func() { cancel(); <-done }()

	if !waitFor(func() bool { return subs.Load() >= 2 }, 4*time.Second) {
		t.Fatalf("expected daemon to resubscribe after stream error; subs=%d", subs.Load())
	}
	if !waitFor(func() bool { return calls.Load() >= 2 }, 1*time.Second) {
		t.Errorf("expected reconcile to fire after resubscribe; calls=%d", calls.Load())
	}
}

// --- helpers --------------------------------------------------------------

type eventSourceFunc func(ctx context.Context) (<-chan docker.Event, <-chan error)

func (f eventSourceFunc) Events(ctx context.Context) (<-chan docker.Event, <-chan error) {
	return f(ctx)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func runAsync(d *Daemon, ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()
	return done
}

func waitFor(cond func() bool, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
