package watcher_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// simulation drives Watcher.Run — the loop, not single ticks — in simulated
// time. The watcher's clock and its Sleep are the simulation's: a pause the
// watcher takes makes that much time pass at once, second by second, with a
// block mined every blockTime and whatever the test has scheduled happening
// at its moment. Nothing runs concurrently and nothing waits for the wall
// clock, so a test can let half an hour pass, count what it cost, and say at
// which second a tick ran.
//
// A pause ends early when its context does: that is how Watcher.Wake ends an
// idle pause, and an event that calls Wake in the middle of a pause sees the
// next tick run at that same simulated moment.
//
// Every tick is checked by the harness's watchdog (through Config.OnTick).
type simulation struct {
	h *harness
	// now is the simulated time since the simulation was created; end is
	// when the current run stops.
	now, end time.Duration
	// blockTime is the simulated time between blocks; owed what has passed
	// since the last one.
	blockTime, owed time.Duration
	// noBlocks stops the mining (the chain stands still).
	noBlocks bool
	events   []*simEvent
	pauses   []simPause
	ticks    []simTick
	// onPause, if set, is called before each pause the watcher takes, with
	// the status its last tick left; onTick after each tick.
	onPause func(d time.Duration, st watcher.Status)
	onTick  func(st watcher.Status)
	cancel  context.CancelFunc
}

type simEvent struct {
	at   time.Duration
	do   func()
	done bool
}

// simPause is one pause of the watcher: when it began, how long the watcher
// meant to pause, how long it did, and whether it was cut short.
type simPause struct {
	at, want, took time.Duration
	cut            bool
	// idle is Status.Idle as the tick before the pause left it.
	idle bool
}

// simTick is one tick: when it ran, and the status it left.
type simTick struct {
	at time.Duration
	st watcher.Status
}

// newSimulation prepares h for simulated runs: 3 s between ticks, the
// default idle interval unless the harness was configured with one, a block
// a second. The watcher is rebuilt with the simulation's clock.
func newSimulation(h *harness) *simulation {
	s := &simulation{h: h, blockTime: time.Second}
	h.cfg.PollInterval = 3 * time.Second
	h.cfg.Sleep = s.sleep
	h.cfg.OnTick = s.ticked
	h.restart()
	return s
}

// at schedules do at simulated time t (from the simulation's start).
func (s *simulation) at(t time.Duration, do func()) {
	s.events = append(s.events, &simEvent{at: t, do: do})
}

// in schedules do d from now.
func (s *simulation) in(d time.Duration, do func()) { s.at(s.now+d, do) }

// run lets d of simulated time pass with the watcher running, and returns
// when it has: Run is cancelled in the pause in which the time is up.
//
// Every run begins with a tick, as Run does. So after run has returned, the
// next run starts a fresh pause at its first simulated moment: an event
// scheduled five seconds in falls five seconds into that pause.
func (s *simulation) run(d time.Duration) {
	s.h.t.Helper()
	s.end = s.now + d
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	defer cancel()
	s.h.beforeTick()
	if err := s.h.w.Watcher.Run(ctx); !errors.Is(err, context.Canceled) {
		s.h.t.Fatalf("Run returned %v\n%s", err, tail(s.h.logs.String(), 4000))
	}
}

// stop ends the current run at the next opportunity (from within an event
// or a hook).
func (s *simulation) stop() { s.end = s.now }

// ticked is the watcher's Config.OnTick.
func (s *simulation) ticked() {
	st := s.h.w.Watcher.Status()
	s.h.afterTick(st.LastError == "")
	s.h.beforeTick()
	s.ticks = append(s.ticks, simTick{at: s.now, st: st})
	if s.onTick != nil {
		s.onTick(st)
	}
}

// sleep is the watcher's Config.Sleep.
func (s *simulation) sleep(ctx context.Context, d time.Duration) error {
	if d < time.Second {
		return ctx.Err() // the pause between two scan steps of one tick
	}
	st := s.h.w.Watcher.Status()
	if s.onPause != nil {
		s.onPause(d, st)
	}
	p := simPause{at: s.now, want: d, idle: st.Idle}
	for p.took < d && ctx.Err() == nil && s.now < s.end {
		step := min(time.Second, d-p.took, s.end-s.now)
		s.advance(step)
		p.took += step
	}
	if s.now >= s.end && p.took < d {
		s.cancel()
	} else if ctx.Err() != nil && p.took < d {
		p.cut = true
	}
	s.pauses = append(s.pauses, p)
	if s.now >= s.end {
		s.cancel()
	}
	return ctx.Err()
}

// advance lets d pass: the clock moves, blocks are mined, events happen.
func (s *simulation) advance(d time.Duration) {
	s.now += d
	s.h.clock.Advance(d)
	if !s.noBlocks {
		for s.owed += d; s.owed >= s.blockTime; s.owed -= s.blockTime {
			s.h.chain.Mine(1)
		}
	}
	for _, e := range s.events {
		if !e.done && e.at <= s.now {
			e.done = true
			e.do()
		}
	}
}

// tickAt returns the first tick that ran at or after t, and whether there
// was one.
func (s *simulation) tickAt(t time.Duration) (simTick, bool) {
	for _, tk := range s.ticks {
		if tk.at >= t {
			return tk, true
		}
	}
	return simTick{}, false
}

// reset forgets the recorded pauses and ticks.
func (s *simulation) reset() { s.pauses, s.ticks = nil, nil }

// idleSince asserts that the watcher has been idle in every tick since
// simulated time t, and that there was at least one.
func (s *simulation) idleSince(t testing.TB, since time.Duration) {
	t.Helper()
	n := 0
	for _, tk := range s.ticks {
		if tk.at < since {
			continue
		}
		n++
		if !tk.st.Idle {
			t.Fatalf("tick at %s: not idle (%s)", tk.at, tk.st.Work)
		}
	}
	if n == 0 {
		t.Fatalf("no tick since %s", since)
	}
}
