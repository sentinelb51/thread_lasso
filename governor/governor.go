//go:build windows && amd64

package governor

import (
	"ThreadOrchestra/config"
	"ThreadOrchestra/process"
	"ThreadOrchestra/thread"
	"ThreadOrchestra/util"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"
)

// Governor runs the sample → metrics → classify → actuate loop for one game
// session.
type Governor struct {
	gameName string
	game     config.Game
	auto     config.Auto
	pid      uint32

	sampler    *Sampler
	tracker    *Tracker
	classifier *Classifier
	phases     *PhaseDetector
	actuator   *Actuator
	manual     *manualApplier
	resolver   ModuleResolver
	topology   *process.Topology

	// UI-driven controls. Both are read at the top of each tick so the actual
	// thread work always happens on the loop goroutine, never the UI's.
	paused          atomic.Bool
	revertRequested atomic.Bool

	warnings []string

	// Views receives one ViewModel per tick; sends are non-blocking (a slow
	// or absent UI never stalls the loop).
	Views chan ViewModel
}

func New(gameName string, game config.Game, pid uint32) *Governor {
	auto := config.Auto{}
	if game.Auto != nil {
		auto = *game.Auto
	}

	mode := process.AccessMode(auto.Mode)
	if mode != process.AccessFull {
		mode = process.AccessLimited
	}

	g := &Governor{
		gameName:   gameName,
		game:       game,
		auto:       auto,
		pid:        pid,
		sampler:    NewSampler(pid, mode),
		tracker:    NewTracker(),
		classifier: NewClassifier(auto.StableWindows),
		phases:     NewPhaseDetector(),
		manual:     newManualApplier(game.Threads),
		Views:      make(chan ViewModel, 1),
	}
	g.actuator = NewActuator(g)

	// Topology is process-wide and needs no handle, so it is loaded in every
	// mode: it validates the assumed CPU-set base and enables SMT-aware
	// ideal-processor placement.
	if topology, err := process.LoadTopology(); err != nil {
		g.warn(fmt.Sprintf("cpu topology unavailable (%v); ideal-processor placement disabled", err))
	} else {
		g.topology = topology
		if topology.Base != util.CpuSetBase() {
			g.warn(fmt.Sprintf("cpu-set base is 0x%x, not the assumed 0x%x; corrected", topology.Base, util.CpuSetBase()))
			util.SetCpuSetBase(topology.Base)
		}
		g.auto.CriticalCores = g.validCores("critical_cores", g.auto.CriticalCores)
		g.auto.BackgroundCores = g.validCores("background_cores", g.auto.BackgroundCores)
	}

	if mode == process.AccessFull {
		resolver, err := NewModuleResolver(pid)
		if err != nil {
			g.warn(fmt.Sprintf("full mode: module resolution unavailable (%v), falling back to behavioral classification", err))
		} else {
			g.resolver = resolver
		}
	}

	return g
}

// validCores drops config core indices outside the machine's logical CPU range
// so an out-of-bounds value can never reach util.CoreArrayToBitmask (which
// panics) or produce a bogus CPU Set ID mid-loop.
func (g *Governor) validCores(name string, cores []int) []int {
	var valid []int
	for _, core := range cores {
		if core >= 0 && core < g.topology.Logical {
			valid = append(valid, core)
		} else {
			g.warn(fmt.Sprintf("%s: core %d out of range [0,%d); ignoring", name, core, g.topology.Logical))
		}
	}
	return valid
}

func (g *Governor) warn(message string) {
	g.warnings = append(g.warnings, message)
}

// TogglePause flips whether the governor applies changes and returns the new
// paused state. Safe to call from the UI goroutine.
func (g *Governor) TogglePause() bool {
	paused := !g.paused.Load()
	g.paused.Store(paused)
	return paused
}

// Paused reports whether actuation is currently suspended.
func (g *Governor) Paused() bool { return g.paused.Load() }

// RevertAll requests that every tuned thread be restored on the next tick. The
// restore runs on the loop goroutine, so it never races an in-flight Apply.
func (g *Governor) RevertAll() { g.revertRequested.Store(true) }

// Run blocks until the game exits or ctx is cancelled. All modifications are
// reverted on the way out — including on panic.
func (g *Governor) Run(ctx context.Context) error {
	defer func() {
		// Revert must run even if a tick panics; handles are still open at
		// this point. Must be fast: CTRL_CLOSE grants ~5s.
		restored, errs := thread.RestoreAll(g.sampler.Cache())
		if restored > 0 || len(errs) > 0 {
			fmt.Printf("reverted %d threads (%d errors)\n", restored, len(errs))
		}
		g.sampler.Cache().Close()
	}()

	interval := time.Duration(g.auto.PollIntervalMS) * time.Millisecond
	if interval < 250*time.Millisecond {
		g.warn("poll_interval_ms below 250 is wasteful: each tick walks a full system snapshot")
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := g.tick(); err != nil {
			if errors.Is(err, process.ErrProcessNotFound) {
				return nil // game exited; deferred revert has nothing live to undo
			}
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (g *Governor) tick() error {
	sample, err := g.sampler.Sample()
	if err != nil {
		return err
	}

	// A UI-requested revert is serviced here, on the loop goroutine, before any
	// new actuation this tick.
	if g.revertRequested.CompareAndSwap(true, false) {
		thread.RestoreAll(g.sampler.Cache())
		g.actuator.Reset()
		g.manual.Reset()
	}

	// Only fold metrics while the game holds the foreground. Unfocused, Windows
	// throttles it, so its thread behavior is unrepresentative; Rebase advances
	// the counter baselines so the series simply hold their last focused values
	// (and the first focused tick after still measures a clean delta).
	if sample.Focused {
		g.tracker.Update(&sample)
	} else {
		g.tracker.Rebase(&sample)
	}

	phase := g.phases.Update(g.tracker)
	g.classifier.Prune(g.tracker.Series)
	g.actuator.Prune(g.tracker.Series)

	facts := g.buildFacts(&sample)

	verdicts := make(map[thread.Key]Verdict, len(facts))
	for key, f := range facts {
		verdicts[key] = g.classifier.Observe(key, f)
	}

	// Actuation is gated on focus too: with metrics frozen there is nothing new
	// to act on, and we avoid fighting the game's own background-state priority
	// juggling. Manual rules are user-authored and deterministic (applied in both
	// manual and auto modes); in auto mode they take precedence over the actuator.
	// A paused governor makes no changes at all.
	if sample.Focused && !g.paused.Load() {
		if g.auto.Optimisation == "manual" || g.auto.Optimisation == "auto" {
			g.manual.Apply(&sample, facts)
		}
		if g.auto.Optimisation == "auto" && phase == PhaseStable {
			g.actuator.Apply(&sample, facts, verdicts)
		}
	}
	g.actuator.Watchdog(facts)

	g.publish(&sample, phase, facts, verdicts)

	return nil
}

// buildFacts derives the per-thread evidence set for this window.
func (g *Governor) buildFacts(sample *Sample) map[thread.Key]*Facts {
	hottest := g.tracker.HottestGameThread()
	cohorts := buildCohorts(sample, g.tracker)

	facts := make(map[thread.Key]*Facts, len(sample.Threads))
	for i := range sample.Threads {
		threadSample := &sample.Threads[i]
		key := thread.Key{TID: threadSample.TID, CreateTime: threadSample.CreateTime}

		series, ok := g.tracker.Series[key]
		if !ok {
			continue
		}

		f := &Facts{
			Series:      series,
			Description: threadSample.Description,
			// Both read the baseline captured at the thread's first sighting,
			// never the live priority — see Series.BaselineRelative.
			TimeCritical:      series.BaselineRelative >= gameElevatedPriority,
			PriorityBoosted:   series.BaselineRelative > 0 && series.BaselineRelative < gameElevatedPriority,
			IsForegroundInput: sample.InputTID != 0 && threadSample.TID == sample.InputTID,
			CohortWeight:      cohorts[key],
		}

		if g.resolver != nil {
			f.Module = g.resolver(threadSample.Win32StartAddress)
		}

		if hottest != nil && hottest.CyclesRateLong > 0 {
			f.CyclesShare = series.CyclesRateLong / hottest.CyclesRateLong
			// The hottest thread is the correlation reference; correlating it
			// with itself is a tautology, so it stays at 0 rather than
			// collecting a free render score for being the yardstick.
			if series != hottest {
				f.FrameCorrelation = Correlation(series, hottest)
			}
		}

		g.applyOverrides(f)
		facts[key] = f
	}

	return facts
}

// Ratio band over which two threads' rates stop counting as the same: fully
// alike within cohortTolerance, unrelated at cohortCutoff. Worker pools run the
// same code on the same work items, so their members track each other closely;
// unrelated threads rarely do.
const (
	cohortTolerance = 1.25
	cohortCutoff    = 2.5
)

// buildCohorts weighs each thread's cohort — the group of threads it is
// indistinguishable from. Two sources are combined:
//
//   - a shared Win32StartAddress, which is exact but unavailable under
//     anti-tamper (Overwatch zeroes it, which also means the address must be
//     skipped rather than counted: every thread shares "0", and one cohort of
//     eighty is worse than no cohort at all);
//   - matching behaviour, which survives that erasure.
//
// The behavioural half is a weighted count rather than a tally of exact
// matches: a hard ratio cut made the weight — and with it the main/sim versus
// job-worker call on the hottest thread in the process — flip on a cohort
// member drifting a few percent between windows. A thread is always fully in
// its own cohort, so the minimum weight is 1.
func buildCohorts(sample *Sample, tracker *Tracker) map[thread.Key]float64 {
	byStart := make(map[uintptr]int)
	for i := range sample.Threads {
		if address := sample.Threads[i].Win32StartAddress; address != 0 {
			byStart[address]++
		}
	}

	type profile struct {
		key      thread.Key
		wait     process.WaitReason
		cycles   float64
		switches float64
	}

	profiles := make([]profile, 0, len(tracker.Series))
	for key, series := range tracker.Series {
		if series.Samples < 3 {
			continue // no reliable behaviour to match on yet
		}
		wait, _ := series.DominantWait()
		profiles = append(profiles, profile{key, wait, series.CyclesRateLong, series.SwitchRateLong})
	}

	// Still O(n²) over a few dozen threads per tick, but each pair is now
	// scored once and credited to both members, which is half the work of the
	// full square and makes the result exactly symmetric by construction.
	weights := make([]float64, len(profiles))
	for i := range profiles {
		weights[i] = 1 // a thread is always its own cohort member
	}
	for i := range profiles {
		for j := i + 1; j < len(profiles); j++ {
			if profiles[i].wait != profiles[j].wait {
				continue
			}
			similarity := rateSimilarity(profiles[i].cycles, profiles[j].cycles) *
				rateSimilarity(profiles[i].switches, profiles[j].switches)
			if similarity <= 0 {
				continue
			}
			weights[i] += similarity
			weights[j] += similarity
		}
	}

	behavioural := make(map[thread.Key]float64, len(profiles))
	for i := range profiles {
		behavioural[profiles[i].key] = weights[i]
	}

	cohorts := make(map[thread.Key]float64, len(sample.Threads))
	for i := range sample.Threads {
		key := thread.Key{TID: sample.Threads[i].TID, CreateTime: sample.Threads[i].CreateTime}
		weight := behavioural[key]
		// A shared entry point is proof of a pool, so it is a floor on the
		// weight rather than another graded term.
		if start := float64(byStart[sample.Threads[i].Win32StartAddress]); start > weight {
			weight = start
		}
		if weight < 1 {
			weight = 1
		}
		cohorts[key] = weight
	}

	return cohorts
}

// rateSimilarity grades how alike two rates are: 1 within cohortTolerance of
// each other, decaying to 0 at cohortCutoff. Two idle threads (both zero) count
// as identical.
func rateSimilarity(a, b float64) float64 {
	if a <= 0 || b <= 0 {
		return boolTo1(a <= 0 && b <= 0)
	}
	if a > b {
		a, b = b, a
	}

	return 1 - ramp(b/a, cohortTolerance, cohortCutoff)
}

// applyOverrides resolves the exclude/force config rules for one thread. An
// exclude glob (matched against the thread name or its start module) marks the
// thread untouchable; the first matching force rule pins its bucket.
func (g *Governor) applyOverrides(f *Facts) {
	for _, pattern := range g.auto.Exclude {
		if util.Match(pattern, f.Description) || (f.Module != "" && util.Match(pattern, f.Module)) {
			f.Excluded = true
			return // excluded threads are never forced or tuned
		}
	}

	for _, rule := range g.auto.Force {
		if rule.Name == "" && rule.Module == "" {
			continue
		}
		if rule.Name != "" && !util.Match(rule.Name, f.Description) {
			continue
		}
		if rule.Module != "" && (f.Module == "" || !util.Match(rule.Module, f.Module)) {
			continue
		}
		f.ForcedBucket = parseBucket(rule.Bucket)
		return
	}
}

func (g *Governor) publish(sample *Sample, phase Phase, facts map[thread.Key]*Facts, verdicts map[thread.Key]Verdict) {
	view := ViewModel{
		At:           sample.At,
		GameName:     g.gameName,
		PID:          g.pid,
		Phase:        phase.String(),
		Optimisation: g.auto.Optimisation,
		Aggression:   g.auto.Aggression,
		AccessMode:   g.auto.Mode,
		Focused:      sample.Focused,
		Warnings:     g.warnings,
		TotalCycles:  g.tracker.TotalCyclesRate,
		ReadRate:     g.tracker.ReadRate,
		ThreadCount:  len(sample.Threads),
		Rows:         make([]ThreadRow, 0, len(sample.Threads)),
	}

	for i := range sample.Threads {
		threadSample := &sample.Threads[i]
		key := thread.Key{TID: threadSample.TID, CreateTime: threadSample.CreateTime}

		f, ok := facts[key]
		if !ok {
			continue
		}
		verdict := verdicts[key]

		// Name is the game-set thread description (often empty). We deliberately
		// do not fall back to the Win32 start address: Overwatch's anti-tamper
		// zeroes it (rendering a useless "0x0"), and a raw hex entry point is
		// noise in the identity column anyway. Unnamed threads are identified by
		// their module (full mode) or classified role instead.
		name := threadSample.Description

		waitProfile := ""
		if reason, share := f.Series.DominantWait(); share > 0 {
			waitProfile = fmt.Sprintf("%s %.0f%%", reason, share*100)
		}

		applied := g.actuator.AppliedLabel(key)
		if applied == "" {
			applied = g.manual.AppliedLabel(key)
		}

		view.Rows = append(view.Rows, ThreadRow{
			TID:          threadSample.TID,
			Name:         name,
			Module:       f.Module,
			CyclesRate:   f.Series.CyclesRateLong,
			SwitchRate:   f.Series.SwitchRateLong,
			Quantum:      f.Series.CyclesPerSwitch,
			Priority:     threadSample.Priority,
			BasePriority: threadSample.BasePriority,
			WaitProfile:  waitProfile,
			Role:         verdict.Role.String(),
			Confidence:   verdict.Confidence,
			Bucket:       verdict.Bucket.String(),
			Stable:       verdict.Stable,
			Applied:      applied,
			Starved:      f.Series.ReadyRatio > 0.2,
		})
	}

	sort.Slice(view.Rows, func(i, j int) bool { return view.Rows[i].CyclesRate > view.Rows[j].CyclesRate })

	// Non-blocking publish: drop the frame if the consumer is behind.
	select {
	case g.Views <- view:
	default:
		select {
		case <-g.Views:
		default:
		}
		select {
		case g.Views <- view:
		default:
		}
	}
}
