package tui

import (
	"context"
	"math"
	"math/rand"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	introDuration = 1800 * time.Millisecond
	introPeriod   = (time.Second + 29) / 30 // round up; never more than 30 frames/s
	introCooldown = 30 * time.Minute
	introFade     = 120 * time.Millisecond // data is readable from the first step, with no cascade
	waveColumns   = 64
	waveRows      = 5
)

type introClock struct {
	now  func() time.Time
	wait func(context.Context, time.Duration) bool
}

// Like tea.Tick, but cancellable when a key, resize or navigation ends the
// animation. At most one command waits, and it never sees loop-owned state.
func waitIntro(ctx context.Context, delay time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

type introWave struct {
	random      func() float64
	base, final [waveColumns]float64
	height      [waveColumns]float64
}

func newIntroWave(random func() float64) introWave {
	w := introWave{random: random}
	for j := range w.base {
		i := float64(j)
		w.base[j] = (0.4 + 0.6*random()) * (0.6 + 0.4*math.Abs(math.Sin(0.5*i+1)))
		w.final[j] = 40 * math.Exp(-i/14) * w.base[j]
	}
	return w
}

func (w *introWave) step(age time.Duration) {
	p := min(1, max(0, float64(age)/float64(1700*time.Millisecond)))
	left := (1 - p) * (1 - p) * (1 - p) // 1 - easeOutCubic(p)
	length := 14 + (120-14)*left
	for j := range w.height {
		target := 40 * math.Exp(-float64(j)/length) * (w.base[j]*(1-left) + w.random()*left)
		target = min(40, target*(1+0.3*left))
		w.height[j] += (target - w.height[j]) * 0.5
	}
}

type introMotion struct {
	clock   introClock
	wave    introWave
	started time.Time
	at      time.Time
	fades   [introFieldCount]time.Time
	frame   uint64

	enabled, pending, playing bool
	tick                      uint64
	stop                      context.CancelFunc
	renderID                  uint64
	renderAt                  time.Time
	slow                      int
}

func newIntroMotion() introMotion {
	rng := rand.New(rand.NewSource(7))
	a := introMotion{clock: introClock{time.Now, waitIntro}, wave: newIntroWave(rng.Float64)}
	a.wave.height = a.wave.final // static until Init has the actual startup policy
	return a
}

func (a *introMotion) start(at time.Time, animate, checkStamp bool) {
	a.started, a.at = at, at
	a.enabled = animate
	a.pending = animate && checkStamp
	a.playing = animate && !checkStamp
	a.frame++
	if animate {
		a.wave.height = [waveColumns]float64{}
		if a.playing {
			a.wave.step(0)
		}
	}
}

func (a *introMotion) stopTick() {
	if a.stop != nil {
		a.stop()
		a.stop, a.tick = nil, 0
	}
}

func (a *introMotion) snap() {
	a.stopTick()
	a.enabled, a.pending, a.playing = false, false, false
	a.fades = [introFieldCount]time.Time{}
	a.wave.height = a.wave.final
	a.frame++
}

func (a *introMotion) advance(at time.Time) {
	if at.After(a.at) {
		a.at = at
	} else {
		at = a.at
	}
	a.frame++
	if a.pending && at.Sub(a.started) >= introDuration {
		a.snap()
	}
	if a.playing {
		age := at.Sub(a.started)
		// Finish at the last frame before the deadline rather than schedule
		// a short extra tick or let startup command overhead extend the show.
		if age >= introDuration-introPeriod {
			a.playing = false
			a.wave.height = a.wave.final
		} else {
			a.wave.step(age)
		}
	}
}

func (a *introMotion) fade(k int, at time.Time) {
	if a.enabled && !a.pending {
		a.at, a.fades[k] = at, at
		a.frame++
	}
}

func (a *introMotion) fadeProgress(k int) float64 {
	if a.fades[k].IsZero() {
		return 1
	}
	return min(1, max(0, float64(a.at.Sub(a.fades[k]))/float64(introFade)))
}

func (a *introMotion) fading() bool {
	for k := range a.fades {
		if a.fadeProgress(k) < 1 {
			return true
		}
	}
	return false
}

func (a *introMotion) opacity() float64 {
	if !a.playing && !a.pending {
		return 1
	}
	return min(1, max(0, float64(a.at.Sub(a.started)-250*time.Millisecond)/float64(800*time.Millisecond)))
}

func (m *Model) receiveIntroStamp(s introStamp) tea.Cmd {
	i := m.intro
	a := &i.motion
	if a.pending {
		at := a.clock.now()
		a.stopTick()
		if !s.ok || !s.last.IsZero() && at.Sub(s.last) < introCooldown {
			a.snap()
		} else {
			a.pending, a.playing = false, true
			a.advance(at)
		}
	}
	if s.ok {
		return recordIntroStamp(i.options.StatePath, a.started)
	}
	return nil
}

type introTick struct {
	id uint64
	at time.Time
}

func (m *Model) syncIntroClock() tea.Cmd {
	i := m.intro
	if i == nil || i.frozen {
		return nil
	}
	a := &i.motion
	if !a.enabled {
		return nil
	}
	t := m.threads[m.active]
	if m.height < 24 || m.width < 40 || t != i.owner || !t.following && t.anchor.id != introEntry ||
		m.focusedDialog() != nil || m.selection.dragging || m.selection.selected {
		a.snap()
		return nil
	}
	if a.tick != 0 || !a.pending && !a.playing && !a.fading() {
		return nil
	}
	delay := introPeriod
	if a.pending {
		delay = max(0, introDuration-a.clock.now().Sub(a.started))
	}
	clock := a.clock
	id := m.token()
	ctx, cancel := context.WithCancel(m.ctx)
	a.tick, a.stop = id, cancel
	return func() tea.Msg {
		if !clock.wait(ctx, delay) {
			return nil
		}
		return introTick{id, clock.now()}
	}
}

func (m *Model) tickIntro(v introTick) tea.Cmd {
	i := m.intro
	if i == nil || v.id == 0 || v.id != i.motion.tick {
		return nil
	}
	a := &i.motion
	a.stopTick()
	if i.memo.sampleID == a.renderID && i.memo.cost > 16*time.Millisecond {
		a.slow++
	} else {
		a.slow = 0
	}
	a.renderID, a.renderAt = v.id, a.clock.now()
	if a.slow >= 2 {
		a.snap()
	} else {
		a.advance(v.at)
	}
	m.introOnly = true
	return m.syncIntroClock()
}

func (m *Model) snapIntroKey() {
	if i := m.intro; i != nil && i.motion.enabled {
		i.motion.snap()
	}
}
