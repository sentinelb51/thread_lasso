//go:build windows && amd64

package governor

import (
	"sort"
	"strings"
	"time"

	"ThreadOrchestra/config"
	"ThreadOrchestra/thread"
)

// The actuator no longer decides what a bucket does — config.Tuning does. What
// stays here is the part that cannot be expressed as a table: the order threads
// are tuned in, which physical core each one gets, when settings are undone, and
// the watchdog that rolls back a demotion that turned out to be load-bearing.

// actionState tracks what the governor has done to one thread, for cooldown,
// watchdog rollback, and the UI's "applied" column.
type actionState struct {
	lastChange time.Time
	label      string
	bucket     Bucket // what the current settings were applied *for*
	generation uint64 // the tuning revision they were applied under
	idealCPU   int    // sticky: the physical core this thread was steered onto
	idealSlot  int    // index of that core in the topology's lead list
	hasIdeal   bool

	// lowered marks a thread whose priority, memory priority, I/O priority or
	// core set we reduced. It is what the watchdog watches, and it covers every
	// kind of holding-back rather than priority alone: a thread throttled into a
	// stall is starved however it got there, and now that any bucket can be
	// configured to lower things, "background" is no longer the right test.
	lowered     bool
	quarantined bool // rolled back after starvation — never touched again
}

// roleImportance orders the critical set for physical-core placement: the
// threads whose stalls are most visible to the player get first pick of the
// least-contended core. Roles outside the critical set never reach placement.
var roleImportance = map[Role]int{
	RoleMainSim:      5,
	RoleRenderSubmit: 4,
	RoleGPUWorker:    3,
	RoleAudio:        2,
	RoleInput:        1,
}

// bucketImportance ranks buckets for the same ordering. Critical threads are
// placed before anything else claims a core.
func bucketImportance(b Bucket) int {
	switch b {
	case BucketCritical:
		return 2
	case BucketInteractive:
		return 1
	case BucketBackground:
		return 0
	default:
		return -1
	}
}

// Actuator turns per-thread verdicts into thread modifications, filtered by the
// configured gates and the handle's real capabilities, and owns the rollback
// watchdog. It never touches a thread in observe mode (governor never calls
// Apply then); Watchdog and AppliedLabel stay safe no-ops in that case.
type Actuator struct {
	governor *Governor
	coreLoad []int // threads currently steered onto each physical-core lead
	states   map[thread.Key]*actionState
	pending  []pendingAction // reused across ticks to keep Apply allocation-free
}

// pendingAction is one thread's actuation, queued so the tick can be ordered by
// importance before anything is applied.
type pendingAction struct {
	key     thread.Key
	entry   *thread.Entry
	verdict Verdict
	facts   *Facts
}

func NewActuator(g *Governor) *Actuator {
	return &Actuator{
		governor: g,
		states:   make(map[thread.Key]*actionState),
	}
}

// Apply walks this tick's stable verdicts and enacts the bucket policy. Only
// called in auto optimisation during PhaseStable, so it can assume the game is
// in a settled state.
//
// Threads are ordered by importance rather than by snapshot order, because the
// order decides who gets the least-contended physical core: the main thread
// should not lose a dedicated core to whichever job worker happened to appear
// first in the kernel's thread list.
func (a *Actuator) Apply(sample *Sample, facts map[thread.Key]*Facts, verdicts map[thread.Key]Verdict) {
	a.pending = a.pending[:0]

	for i := range sample.Threads {
		threadSample := &sample.Threads[i]
		key := thread.Key{TID: threadSample.TID, CreateTime: threadSample.CreateTime}

		entry := threadSample.Entry
		if entry == nil || entry.Handle == 0 {
			continue
		}

		verdict, ok := verdicts[key]
		if !ok || !verdict.Stable {
			continue // only act on classifications the bucket filter has committed
		}

		f := facts[key]
		if f == nil {
			continue
		}

		a.pending = append(a.pending, pendingAction{key, entry, verdict, f})
	}

	sort.SliceStable(a.pending, func(i, j int) bool {
		left, right := &a.pending[i], &a.pending[j]
		if bl, br := bucketImportance(left.verdict.Bucket), bucketImportance(right.verdict.Bucket); bl != br {
			return bl > br
		}
		if rl, rr := roleImportance[left.verdict.Role], roleImportance[right.verdict.Role]; rl != rr {
			return rl > rr
		}

		return left.facts.CyclesShare > right.facts.CyclesShare
	})

	tuning, generation := a.governor.Tuning()
	for i := range a.pending {
		action := &a.pending[i]
		a.applyOne(action.key, action.entry, action.verdict, tuning, generation)
	}
}

func (a *Actuator) applyOne(key thread.Key, entry *thread.Entry, verdict Verdict, tuning *config.Tuning, generation uint64) {
	// Manual rules win outright: never fight a thread the user has pinned.
	if a.governor.manual.Owns(key) {
		return
	}

	state := a.stateFor(key)
	if state.quarantined {
		return
	}

	// The settings on this thread no longer match what it should have, so undo
	// them before doing anything else. Two things can invalidate them: the
	// classification moved, or the user edited the tuning table underneath us.
	// Leaving a thread tuned for a bucket it is no longer in is the failure this
	// guards against — state.bucket used to be recorded and never read, so a
	// thread misclassified once during loading kept those settings for the whole
	// session and only the starvation watchdog could ever undo anything.
	if state.bucket != BucketNone && (state.bucket != verdict.Bucket || state.generation != generation) {
		retuned := state.generation != generation
		tuned := entry.Touched()
		if err := entry.Restore(); err != nil {
			return // journal intact; try again next tick
		}

		state.bucket = BucketNone
		state.lowered = false
		a.releaseCore(state)

		// A flapping classification must not drive a burst of changes, so a
		// bucket move costs a cooldown before the thread is tuned again. A
		// settings edit is a deliberate act by someone watching the window, and
		// making them wait out a 30-second cooldown to see the effect would read
		// as the control not working.
		if tuned && !retuned {
			state.label = "reverted"
			state.lastChange = time.Now()
			return
		}
	}

	if !state.lastChange.IsZero() && time.Since(state.lastChange) < time.Duration(tuning.Gates.CooldownMS)*time.Millisecond {
		return
	}

	action, ok := tuning.ActionFor(verdict.Bucket.String(), verdict.Role.String())
	if !ok {
		return // BucketNone / BucketUntouchable — leave the thread alone
	}

	// Demotions require more evidence than promotions, at every level: a wrongly
	// held-back thread costs frames for as long as it stays that way, while a
	// wrongly untouched one costs nothing.
	lowers := action.Lowers()
	if lowers {
		if verdict.Confidence < tuning.Gates.DemoteMinConfidence {
			return
		}
		if !tuning.Gates.Demotable(verdict.Role.String()) {
			return
		}
	}

	applied := a.enact(entry, state, action)
	if len(applied) == 0 {
		// Nothing needed changing — the thread already matches its bucket.
		// Record the bucket anyway so the reclassification check above can see
		// it later, and start the cooldown so we stop re-probing every tick.
		if state.bucket == BucketNone {
			state.bucket, state.generation = verdict.Bucket, generation
			state.lastChange = time.Now()
		}
		return
	}

	state.lastChange = time.Now()
	state.label = strings.Join(applied, ",")
	state.bucket, state.generation = verdict.Bucket, generation
	if lowers {
		state.lowered = true
	}
}

// enact writes one resolved action to a thread and returns a short label for
// each field it actually changed. Every field is independent: one that cannot be
// applied — no capability, no configured core list — is skipped without
// affecting the others.
func (a *Actuator) enact(entry *thread.Entry, state *actionState, action config.BucketAction) []string {
	var applied []string

	switch action.PriorityMode {
	case config.PriorityRaise:
		if ok, _ := entry.RaisePriorityTo(action.Priority); ok {
			applied = append(applied, "prio↑")
		}
	case config.PriorityLower:
		if ok, _ := entry.LowerPriorityTo(action.Priority); ok {
			applied = append(applied, "prio↓")
		}
	case config.PrioritySet:
		if ok, _ := entry.SetPriorityTo(action.Priority); ok {
			applied = append(applied, "prio=")
		}
	}

	if cores := a.coresFor(action.CPUSets); len(cores) > 0 {
		if ok, _ := entry.ApplyCpuSets(cores); ok {
			applied = append(applied, "cpuset")
		}
	}

	// Steer the thread onto a distinct physical core so a frame-critical set does
	// not pile onto SMT siblings (full mode only). Assigned once and remembered:
	// the round-robin cursor this replaced advanced on every re-apply, so a
	// thread that stayed critical was moved to a different core every cooldown,
	// throwing away its cache warmth.
	if action.IdealCore {
		if cpu, has := a.idealCoreFor(state); has {
			if ok, _ := entry.ApplyIdealProcessor(cpu); ok {
				applied = append(applied, "ideal")
			}
		}
	}

	if priority, wanted := config.IoPriorityValue(action.IOPriority); wanted {
		if ok, _ := entry.ApplyIoPriority(priority); ok {
			applied = append(applied, "io"+arrow(priority, ioPriorityNormal))
		}
	}

	if action.MemoryPriority > 0 {
		if ok, _ := entry.ApplyMemoryPriority(uint32(action.MemoryPriority)); ok {
			applied = append(applied, "mem"+arrow(action.MemoryPriority, memoryPriorityNormal))
		}
	}

	if action.EcoQoS {
		if ok, _ := entry.ApplyEcoQoS(); ok {
			applied = append(applied, "eco")
		}
	}

	return applied
}

// The neutral points the applied labels are drawn against, so a glance at the
// column says which direction a thread was moved.
const (
	ioPriorityNormal     = 2
	memoryPriorityNormal = 5
)

func arrow(value, neutral int) string {
	switch {
	case value > neutral:
		return "↑"
	case value < neutral:
		return "↓"
	default:
		return "="
	}
}

// coresFor resolves a cpu_sets selection to the game's configured core list.
// An unconfigured list yields nothing, which skips the action.
func (a *Actuator) coresFor(selection string) []int {
	switch selection {
	case config.CpuSetsCritical:
		return a.governor.auto.CriticalCores
	case config.CpuSetsBackground:
		return a.governor.auto.BackgroundCores
	}

	return nil
}

// idealCoreFor returns the physical core this thread should be steered onto,
// assigning one on first use and reusing it thereafter. The core is the
// least-occupied lead, which replaces a round-robin cursor that only ever
// advanced: cores were never given back when a thread died or was reverted, so
// after a session's worth of thread churn the surviving critical threads could
// all be pointed at the same physical core while the rest of the machine sat
// idle. Reports false when topology is unavailable (limited mode).
func (a *Actuator) idealCoreFor(state *actionState) (int, bool) {
	if state.hasIdeal {
		return state.idealCPU, true
	}
	if a.governor.topology == nil {
		return 0, false
	}

	leads := a.governor.topology.PhysicalCoreLeads()
	if len(leads) == 0 {
		return 0, false
	}
	if len(a.coreLoad) != len(leads) {
		a.coreLoad = make([]int, len(leads))
	}

	// Ties resolve to the lowest core index, so with the importance ordering in
	// Apply the main thread lands on the first lead every session.
	slot := 0
	for i, load := range a.coreLoad {
		if load < a.coreLoad[slot] {
			slot = i
		}
	}

	a.coreLoad[slot]++
	state.idealSlot = slot
	state.idealCPU = leads[slot]
	state.hasIdeal = true

	return state.idealCPU, true
}

// releaseCore returns a thread's physical-core reservation to the pool.
func (a *Actuator) releaseCore(state *actionState) {
	if !state.hasIdeal {
		return
	}

	state.hasIdeal = false
	if state.idealSlot < len(a.coreLoad) && a.coreLoad[state.idealSlot] > 0 {
		a.coreLoad[state.idealSlot]--
	}
}

// Watchdog rolls back any thread we held back that is now CPU-starved: a
// classification that turned out to be load-bearing. The thread is fully
// reverted, and by default quarantined so the governor stops fighting the
// scheduler over it.
func (a *Actuator) Watchdog(facts map[thread.Key]*Facts) {
	tuning, _ := a.governor.Tuning()

	for key, state := range a.states {
		if !state.lowered || state.quarantined {
			continue
		}

		f := facts[key]
		if f == nil || f.Series.ReadyRatio <= tuning.Gates.StarvationReadyRatio {
			continue
		}

		entry := a.governor.sampler.Cache().Lookup(key)
		if entry == nil {
			continue
		}
		if err := entry.Restore(); err != nil {
			continue // keep trying next tick
		}

		state.lowered = false
		state.bucket = BucketNone
		a.releaseCore(state)

		if tuning.Gates.QuarantineOnRollback {
			state.quarantined = true
			state.label = "rolled-back"
			continue
		}
		// Not quarantined: the thread is eligible again, but only after a
		// cooldown, or the next tick would simply redo what just starved it.
		state.label = "rolled-back*"
		state.lastChange = time.Now()
	}
}

// Prune drops action state for threads that no longer exist. Without it the
// state map grew for the whole session, and — worse — dead threads went on
// holding physical-core reservations that live threads were then steered away
// from.
func (a *Actuator) Prune(live map[thread.Key]*Series) {
	for key, state := range a.states {
		if _, ok := live[key]; !ok {
			a.releaseCore(state)
			delete(a.states, key)
		}
	}
}

// Reset forgets all per-thread action state, letting the governor re-tune from
// scratch after a UI-requested revert. Threads themselves are restored
// separately (by the governor's RestoreAll) before this is called.
func (a *Actuator) Reset() {
	a.states = make(map[thread.Key]*actionState)
	a.coreLoad = nil
}

// AppliedLabel is the UI's "applied" column: the last action taken on a
// thread, or "" when the governor has not touched it.
func (a *Actuator) AppliedLabel(key thread.Key) string {
	if state, ok := a.states[key]; ok {
		return state.label
	}
	return ""
}

func (a *Actuator) stateFor(key thread.Key) *actionState {
	state, ok := a.states[key]
	if !ok {
		state = &actionState{}
		a.states[key] = state
	}
	return state
}
