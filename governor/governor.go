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
	"strings"
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
	identifier *Identifier
	topology   *process.Topology

	// UI-driven controls. All are read at the top of each tick so the actual
	// thread work always happens on the loop goroutine, never the UI's.
	paused          atomic.Bool
	revertRequested atomic.Bool

	// tuning is swapped wholesale rather than edited in place, so a tick always
	// sees one coherent table. generation increments with every swap, which is
	// what tells the actuator that a thread's current settings were applied
	// under rules that no longer exist and must be undone before it re-tunes.
	tuning     atomic.Pointer[config.Tuning]
	generation atomic.Uint64
	saveTuning func(config.Tuning) error

	warnings          []string
	entryPointChecked bool

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

	buckets, problems := ParseRoleBuckets(auto.RoleBuckets)

	g := &Governor{
		gameName:   gameName,
		game:       game,
		auto:       auto,
		pid:        pid,
		sampler:    NewSampler(pid, mode),
		tracker:    NewTracker(),
		classifier: NewClassifier(auto.Tuning.Gates, buckets),
		phases:     NewPhaseDetector(),
		manual:     newManualApplier(game.Threads),
		Views:      make(chan ViewModel, 1),
	}
	g.tuning.Store(&auto.Tuning)
	g.actuator = NewActuator(g)

	for _, problem := range problems {
		g.warn(problem)
	}
	preset := config.DefaultTuning(auto.Aggression)
	for _, problem := range checkPolicy(&auto.Tuning, &preset, buckets) {
		g.warn(problem)
	}

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
		identifier, err := NewIdentifier(pid, auto.Tuning.Scan)
		if err != nil {
			g.warn(fmt.Sprintf("full mode: process memory unreadable (%v); identification is behavioural only", err))
		} else {
			g.identifier = identifier
		}
	}

	return g
}

// checkPolicy reports edits that cannot do anything. A fully configurable
// policy has one quiet failure mode: a bucket told to lower a thread does
// nothing at all unless that thread's role is also on the demote list, and the
// two settings live in different sections of the config. Someone who lowers the
// interactive bucket at standard aggression deserves to be told that the
// preset's demote list contains no role that lands there.
//
// Only deviations from the preset are reported. The presets themselves contain
// this combination on purpose — standard puts network threads in a bucket that
// lowers priority while declining to demote them, which is precisely how
// "demote only what I am sure about" is expressed — and warning about the
// defaults would train everyone to ignore the warning.
func checkPolicy(tuning, preset *config.Tuning, buckets RoleBuckets) []string {
	var problems []string

	for role := Role(1); int(role) < roleCount; role++ {
		name := role.String()
		bucket := buckets.Of(role)

		action, ok := tuning.ActionFor(bucket.String(), name)
		if !ok || !action.Lowers() || tuning.Gates.Demotable(name) {
			continue
		}
		if shipped, ok := preset.ActionFor(bucket.String(), name); ok && shipped == action {
			continue // as designed, not as edited
		}

		problems = append(problems, fmt.Sprintf(
			"%s threads land in the %s bucket, which now lowers settings, but %q is not in gates.demote_roles — they will be left alone",
			name, bucket, name))
	}

	return problems
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

// Tuning returns the live tuning table and the revision it belongs to. The
// table must be treated as read-only: it is shared by every goroutine that
// reads it, and edits go through ApplyTuning instead.
func (g *Governor) Tuning() (*config.Tuning, uint64) {
	return g.tuning.Load(), g.generation.Load()
}

// Aggression is the preset name the tuning defaults come from, which is what a
// "reset to default" is measured against.
func (g *Governor) Aggression() string { return g.auto.Aggression }

// GameName is the executable this session is attached to, and the key its
// settings are saved under.
func (g *Governor) GameName() string { return g.gameName }

// Draft returns an editable copy of the live table together with the registry
// bound to it. Editing the copy changes nothing until it is handed back to
// ApplyTuning, so a settings panel can be cancelled.
func (g *Governor) Draft() (*config.Tuning, []config.Setting) {
	live, _ := g.Tuning()
	draft := *live
	defaults := config.DefaultTuning(g.auto.Aggression)

	return &draft, config.Settings(&draft, &defaults)
}

// ApplyTuning swaps in an edited table and bumps the revision, which makes the
// actuator undo settings applied under the old one before re-tuning. Safe to
// call from the UI goroutine; the work lands on the next tick.
func (g *Governor) ApplyTuning(tuning config.Tuning) []string {
	problems := tuning.Validate(g.auto.Aggression)

	g.tuning.Store(&tuning)
	g.generation.Add(1)
	g.classifier.Retune(tuning.Gates)

	return problems
}

// OnSave installs the hook that writes a tuning table back to config.json. Set
// by main, which is the only layer that knows the whole file.
func (g *Governor) OnSave(save func(config.Tuning) error) { g.saveTuning = save }

// SaveTuning applies a table and persists it. Applying happens first: a
// settings change the user can see working is worth more than one that only
// reached the disk.
func (g *Governor) SaveTuning(tuning config.Tuning) ([]string, error) {
	problems := g.ApplyTuning(tuning)
	if g.saveTuning == nil {
		return problems, errors.New("no config file is attached to this session")
	}

	return problems, g.saveTuning(tuning)
}

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
		g.identifier.Close()
	}()

	interval := g.pollInterval()
	if interval < 250*time.Millisecond {
		g.warn("gates.poll_interval_ms below 250 is wasteful: each tick walks a full system snapshot")
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

		// The poll interval is itself a setting, so it can move mid-session.
		if next := g.pollInterval(); next != interval {
			interval = next
			ticker.Reset(interval)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// pollInterval reads the configured sample period, floored so that a bad value
// can never spin the loop.
func (g *Governor) pollInterval() time.Duration {
	tuning, _ := g.Tuning()

	interval := time.Duration(tuning.Gates.PollIntervalMS) * time.Millisecond
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}

	return interval
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
	g.identifier.Prune(g.tracker.Series)

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
	tuning, _ := g.Tuning()
	elevated := tuning.Signals.GameElevatedPriority

	// Identities are resolved first because cohort detection groups on the
	// recovered entry point, which may have come from a stack sweep rather than
	// from the snapshot the cohort builder can see.
	g.identifier.BeginTick(tuning.Scan)
	identities := make(map[thread.Key]identity, len(sample.Threads))
	for i := range sample.Threads {
		threadSample := &sample.Threads[i]
		key := thread.Key{TID: threadSample.TID, CreateTime: threadSample.CreateTime}
		identities[key] = g.identifier.Identify(key, threadSample)
	}

	cohorts := buildCohorts(sample, g.tracker, identities)

	facts := make(map[thread.Key]*Facts, len(sample.Threads))
	for i := range sample.Threads {
		threadSample := &sample.Threads[i]
		key := thread.Key{TID: threadSample.TID, CreateTime: threadSample.CreateTime}

		series, ok := g.tracker.Series[key]
		if !ok {
			continue
		}

		known := identities[key]
		f := &Facts{
			Series:       series,
			Description:  threadSample.Description,
			Module:       known.Module,
			ModuleOffset: known.ModuleOffset,
			Stack:        known.Stack,
			GUIThread:    known.GUI,
			Signals:      &tuning.Signals,
			// Both read the baseline captured at the thread's first sighting,
			// never the live priority — see Series.BaselineRelative.
			TimeCritical:      series.BaselineRelative >= elevated,
			PriorityBoosted:   series.BaselineRelative > 0 && series.BaselineRelative < elevated,
			IsForegroundInput: sample.InputTID != 0 && threadSample.TID == sample.InputTID,
			CohortWeight:      cohorts[key],
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

	// Reload the module table if addresses went unresolved this window; a
	// protected process maps its real modules long after we attach.
	g.identifier.Refresh()
	g.checkEntryPoints(sample, identities)

	return facts
}

// entryCheckTick is when the start-address diagnostic runs. Stack sweeps are
// budgeted per tick, so the recovery routes need several ticks to have been
// tried on every thread; reporting before then would blame the process for work
// we had not done yet.
const entryCheckTick = 10

// checkEntryPoints reports, once, what the process is hiding about where its
// threads started, and which of the four recovery routes still worked.
//
// The distinction the warning draws is the diagnostically useful one. If the
// snapshot's stack and TEB pointers are populated while both address fields are
// zero, the struct is being read correctly and those two fields specifically
// have been cleared — user mode can do that to Win32StartAddress on its own,
// but clearing the kernel's copy as well takes a driver. If the whole struct
// comes back empty, nothing was scrubbed and we are simply not being told, so
// the fix is privilege, not cleverness.
func (g *Governor) checkEntryPoints(sample *Sample, identities map[thread.Key]identity) {
	if g.entryPointChecked || sample.Tick < entryCheckTick || len(sample.Threads) < 4 {
		return
	}
	g.entryPointChecked = true

	total := len(sample.Threads)
	blank, opaque := 0, 0
	for i := range sample.Threads {
		snapshot := &sample.Threads[i].ThreadSnapshot
		if snapshot.EntryPoint() != 0 {
			continue
		}
		blank++
		// Same struct, adjacent fields: if these are empty too, the snapshot
		// itself is being withheld rather than doctored.
		if snapshot.StackBase == 0 && snapshot.TebBase == 0 {
			opaque++
		}
	}

	// A handful of threads without a start address is normal — system worker
	// threads have kernel-space ones. A majority means something is hiding them.
	if blank*2 <= total {
		return
	}

	if opaque*2 > total {
		g.warn(fmt.Sprintf("%d/%d threads report no start address and no stack either: "+
			"the kernel is withholding the whole thread record, not the process hiding it", opaque, total))
	} else {
		g.warn(fmt.Sprintf("%d/%d threads have both start-address fields cleared "+
			"(stack and TEB pointers intact, so the record itself is readable)", blank, total))
	}

	g.warn("thread origins: " + describeSources(g.identifier.Sources(), total))
}

// activityOf picks the module a thread's stack says it is working in: the one
// with the most frames that is not a module every thread has. A row reading
// "[ntdll.dll]" would be true of the whole process and would crowd out the
// synthetic role label, which at least distinguishes one thread from another.
func activityOf(stack []string) string {
	for _, module := range stack {
		if !process.StartupModule(module) {
			return module
		}
	}

	return ""
}

// describeSources renders the recovery-route census, best route first, so the
// warning says what actually worked rather than only what failed.
func describeSources(counts map[EntrySource]int, total int) string {
	if len(counts) == 0 {
		return "unavailable (limited mode)"
	}

	parts := make([]string, 0, len(counts))
	for _, source := range []EntrySource{EntryWin32, EntryKernel, EntryQuery, EntryStack} {
		if counts[source] > 0 {
			parts = append(parts, fmt.Sprintf("%d via %s", counts[source], source))
		}
	}
	if counts[EntryNone] > 0 {
		parts = append(parts, fmt.Sprintf("%d unrecovered", counts[EntryNone]))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("none of %d recovered", total)
	}

	return strings.Join(parts, ", ")
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
//   - a shared entry point, which is exact but not always available. The
//     address is whatever Identifier managed to recover, which on a process
//     that scrubs both snapshot fields means the one read off the thread's own
//     startup frames. It is 0 when every route failed, and a zero address must
//     be skipped rather than counted — every thread would share "0", and one
//     cohort of eighty is worse than none;
//   - matching behaviour, which survives that erasure.
//
// The behavioural half is a weighted count rather than a tally of exact
// matches: a hard ratio cut made the weight — and with it the main/sim versus
// job-worker call on the hottest thread in the process — flip on a cohort
// member drifting a few percent between windows. A thread is always fully in
// its own cohort, so the minimum weight is 1.
func buildCohorts(sample *Sample, tracker *Tracker, identities map[thread.Key]identity) map[thread.Key]float64 {
	byStart := make(map[uintptr]int)
	for _, known := range identities {
		if known.Entry != 0 {
			byStart[known.Entry]++
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
		if entry := identities[key].Entry; entry != 0 {
			if start := float64(byStart[entry]); start > weight {
				weight = start
			}
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

// assignOrdinals numbers the threads within each role so an unnamed thread can
// still be referred to as "render #2" from one tick to the next. The ordering
// is by creation time (then TID), never by cycle rate: the table is sorted by
// rate, and a label that renumbered itself every time two workers swapped
// places would be worse than no label at all.
func assignOrdinals(rows []ThreadRow) {
	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		x, y := &rows[order[a]], &rows[order[b]]
		if x.CreateTime != y.CreateTime {
			return x.CreateTime < y.CreateTime
		}
		return x.TID < y.TID
	})

	counts := make(map[string]int, roleCount)
	for _, i := range order {
		counts[rows[i].Role]++
		rows[i].Ordinal = counts[rows[i].Role]
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

		waitProfile := ""
		if reason, share := f.Series.DominantWait(); share > 0 {
			waitProfile = fmt.Sprintf("%s %.0f%%", reason, share*100)
		}

		applied := g.actuator.AppliedLabel(key)
		if applied == "" {
			applied = g.manual.AppliedLabel(key)
		}

		activity := activityOf(f.Stack)

		view.Rows = append(view.Rows, ThreadRow{
			TID: threadSample.TID,
			// The game-set thread description, usually empty; ThreadRow.Identity
			// decides what to show when it is.
			Name:         threadSample.Description,
			Module:       f.Module,
			ModuleOffset: f.ModuleOffset,
			Activity:     activity,
			CreateTime:   threadSample.CreateTime,
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

	assignOrdinals(view.Rows)
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
