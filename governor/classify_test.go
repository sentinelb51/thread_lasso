//go:build windows && amd64

package governor

import (
	"ThreadOrchestra/process"
	"ThreadOrchestra/thread"
	"math"
	"testing"
	"time"
)

// synthTick describes a constant per-tick behavior; buildSeries feeds it
// through the real metrics pipeline so tests exercise update()/EMA/histogram
// exactly as the governor does at runtime.
type synthTick struct {
	state           process.ThreadState
	wait            process.WaitReason
	cyclesPerTick   uint64
	switchesPerTick uint32
	createTime      int64
}

const filetimeEpochOffset = 116444736000000000 // 1601→1970 in 100ns ticks

var synthBase = time.Unix(1_000_000, 0)

// filetimeBefore returns the FILETIME stamp d before t.
func filetimeBefore(t time.Time, d time.Duration) int64 {
	return t.UnixNano()/100 + filetimeEpochOffset - d.Nanoseconds()/100
}

func buildSeries(spec synthTick, ticks int) *Series {
	if spec.createTime == 0 {
		spec.createTime = filetimeBefore(synthBase, 300*time.Second) // old, not a founder
	}

	s := &Series{Key: thread.Key{TID: 1}}
	at := synthBase
	var cycles uint64
	var switches uint32

	for i := 0; i < ticks; i++ {
		cycles += spec.cyclesPerTick
		switches += spec.switchesPerTick
		sample := ThreadSample{
			ThreadSnapshot: process.ThreadSnapshot{
				TID:             1,
				CreateTime:      spec.createTime,
				ContextSwitches: switches,
				ThreadState:     spec.state,
				WaitReason:      spec.wait,
				UserTime:        int64(cycles / 2),
				KernelTime:      int64(cycles / 2),
			},
			Cycles:    cycles,
			HasCycles: true,
		}
		s.update(&sample, at, 0)
		at = at.Add(time.Second)
	}

	return s
}

func TestClassifyJobWorker(t *testing.T) {
	// WrQueue-dominant + high cycles = an active thread-pool worker.
	s := buildSeries(synthTick{
		state:           process.StateWaiting,
		wait:            process.WrQueue,
		cyclesPerTick:   2e8,
		switchesPerTick: 500, // quantum 4e5 > pump threshold, so not a pump
	}, 40)

	role, conf := ClassifyRole(&Facts{Series: s, CyclesShare: 0.2})
	if role != RoleJobWorker {
		t.Fatalf("role = %v (conf %.2f), want job-worker", role, conf)
	}
}

// The engine this tool targets parks its job system on WrAlertByThreadId
// (NtWaitForAlertByThreadId — the futex behind WaitOnAddress/SRWLOCK), not on
// a KQUEUE. Keying the pool rules on WrQueue alone left every one of those
// threads unclassified and therefore untuned.
func TestClassifyFutexPool(t *testing.T) {
	busy := buildSeries(synthTick{
		state:           process.StateWaiting,
		wait:            process.WrAlertByThreadId,
		cyclesPerTick:   1e9,
		switchesPerTick: 15000,
	}, 40)

	if role, conf := ClassifyRole(&Facts{Series: busy, CyclesShare: 0.23}); role != RoleJobWorker {
		t.Fatalf("busy futex thread: role = %v (conf %.2f), want job-worker", role, conf)
	}

	// The four ~9.5M cyc/s, ~35 sw/s threads in the live capture: a pool
	// waiting between jobs, not a pool doing work.
	idle := buildSeries(synthTick{
		state:           process.StateWaiting,
		wait:            process.WrAlertByThreadId,
		cyclesPerTick:   95e5,
		switchesPerTick: 35,
	}, 40)

	if role, conf := ClassifyRole(&Facts{Series: idle, CyclesShare: 0.002}); role != RolePoolIdle {
		t.Fatalf("idle futex thread: role = %v (conf %.2f), want pool-idle", role, conf)
	}
}

func TestClassifyPoolIdle(t *testing.T) {
	// WrQueue-dominant + near-zero cycles = a parked pool worker.
	s := buildSeries(synthTick{
		state:           process.StateWaiting,
		wait:            process.WrQueue,
		cyclesPerTick:   0,
		switchesPerTick: 10,
	}, 40)

	role, conf := ClassifyRole(&Facts{Series: s, CyclesShare: 0.001})
	if role != RolePoolIdle {
		t.Fatalf("role = %v (conf %.2f), want pool-idle", role, conf)
	}
}

func TestClassifyTelemetry(t *testing.T) {
	// A long-lived sleep loop that never does real work.
	s := buildSeries(synthTick{
		state:           process.StateWaiting,
		wait:            process.WrDelayExecution,
		cyclesPerTick:   0,
		switchesPerTick: 1,
		createTime:      filetimeBefore(synthBase, 120*time.Second),
	}, 40)

	role, conf := ClassifyRole(&Facts{Series: s, CyclesShare: 0.005})
	if role != RoleTelemetry {
		t.Fatalf("role = %v (conf %.2f), want telemetry", role, conf)
	}
}

// The kernel reports a user-mode Sleep as plain DelayExecution, not
// WrDelayExecution. The rule only checked the Wr variant, so the 44M cyc/s
// sleep-loop thread in the live capture scored nothing at all.
func TestClassifyTelemetryPlainDelayExecution(t *testing.T) {
	s := buildSeries(synthTick{
		state:           process.StateWaiting,
		wait:            process.DelayExecution,
		cyclesPerTick:   44e6,
		switchesPerTick: 99,
		createTime:      filetimeBefore(synthBase, 300*time.Second),
	}, 40)

	// Share 0.0105 — just over the old hard cut of 0.01, which alone would
	// have disqualified it even with the right wait reason.
	role, conf := ClassifyRole(&Facts{Series: s, CyclesShare: 0.0105})
	if role != RoleTelemetry {
		t.Fatalf("role = %v (conf %.2f), want telemetry", role, conf)
	}
}

// The second-hottest thread in the live capture scored nothing and stayed
// unknown: share 0.4455 against an old main/sim cut of 0.5, and 1066 sw/s
// against an old render cut of 2000. It missed both by a hair.
func TestGradedScoringCatchesNearMiss(t *testing.T) {
	s := buildSeries(synthTick{
		state:           process.StateWaiting,
		wait:            process.UserRequest,
		cyclesPerTick:   1_859_600_000,
		switchesPerTick: 1066,
	}, 40)
	s.CreatedAtStart = true

	if role, conf := ClassifyRole(&Facts{Series: s, CyclesShare: 0.4455}); role != RoleMainSim {
		t.Fatalf("near-miss cruncher: role = %v (conf %.2f), want main/sim", role, conf)
	}
}

// Evidence must degrade smoothly across a band rather than switching off at a
// threshold. Two threads a hair either side of the old 0.25 render cut should
// score almost the same, not 2.5 versus nothing.
func TestGradedScoringIsContinuous(t *testing.T) {
	s := buildSeries(synthTick{
		state:           process.StateWaiting,
		wait:            process.UserRequest,
		cyclesPerTick:   971_600_000,
		switchesPerTick: 15822,
	}, 40)

	below := scoreRoles(&Facts{Series: s, CyclesShare: 0.2328}).score[RoleRenderSubmit]
	above := scoreRoles(&Facts{Series: s, CyclesShare: 0.2672}).score[RoleRenderSubmit]

	if below <= 0 {
		t.Fatalf("thread just below the old cut scored nothing at all")
	}
	if gap := math.Abs(above - below); gap > 0.5 {
		t.Fatalf("render score jumped by %.2f across the old cut; grading is not continuous", gap)
	}
}

// A cohort of statistically identical threads is a worker pool, and a process
// has exactly one main thread — so cohort membership must veto main/sim.
func TestCohortVetoesMainSim(t *testing.T) {
	s := buildSeries(synthTick{
		state:           process.StateWaiting,
		wait:            process.UserRequest,
		cyclesPerTick:   1_270_000_000,
		switchesPerTick: 200,
	}, 40)
	s.CreatedAtStart = true

	alone := &Facts{Series: s, CyclesShare: 0.9, CohortSize: 1}
	if role, _ := ClassifyRole(alone); role != RoleMainSim {
		t.Fatalf("solo hot thread: role = %v, want main/sim", role)
	}

	pooled := &Facts{Series: s, CyclesShare: 0.9, CohortSize: 4}
	if role, _ := ClassifyRole(pooled); role == RoleMainSim {
		t.Fatalf("cohort member classified main/sim; a process has one main thread")
	}
}

func TestClassifyAudioPump(t *testing.T) {
	// Many tiny, regular wakes + game-set Time Critical = an audio pump.
	s := buildSeries(synthTick{
		state:           process.StateWaiting,
		wait:            process.WrUserRequest,
		cyclesPerTick:   1e6,
		switchesPerTick: 1000, // quantum 1000 << pump threshold
	}, 40)

	f := &Facts{Series: s, CyclesShare: 0.01, TimeCritical: true}
	role, conf := ClassifyRole(f)
	if role != RoleAudio {
		t.Fatalf("role = %v (conf %.2f), want audio", role, conf)
	}

	// The game marked it Time Critical, so it must be untouchable.
	if b := BucketFor(role, f); b != BucketUntouchable {
		t.Fatalf("bucket = %v, want untouchable for game-set Time Critical", b)
	}
}

func TestClassifyInput(t *testing.T) {
	// Owning the foreground window's message queue is hard evidence.
	s := buildSeries(synthTick{state: process.StateWaiting, wait: process.WrUserRequest, switchesPerTick: 5}, 40)

	role, _ := ClassifyRole(&Facts{Series: s, IsForegroundInput: true})
	if role != RoleInput {
		t.Fatalf("role = %v, want input", role)
	}
}

func TestVivoxOverridesTimeCritical(t *testing.T) {
	// vivoxsdk voice threads may be tuned despite being Time Critical
	// (user decision recorded in the plan).
	s := buildSeries(synthTick{state: process.StateWaiting, wait: process.WrUserRequest, switchesPerTick: 100}, 10)
	f := &Facts{Series: s, TimeCritical: true, Module: "vivoxsdk.dll"}

	if b := BucketFor(RoleNetwork, f); b == BucketUntouchable {
		t.Fatalf("vivox voice thread should be overridable, got untouchable")
	} else if b != BucketInteractive {
		t.Fatalf("bucket = %v, want interactive", b)
	}
}

func TestExcludeMakesUntouchable(t *testing.T) {
	// An exclude match overrides everything, even a strong critical role.
	f := &Facts{Excluded: true}
	if b := BucketFor(RoleMainSim, f); b != BucketUntouchable {
		t.Fatalf("bucket = %v, want untouchable for excluded thread", b)
	}
}

func TestForceBucketOverridesTimeCritical(t *testing.T) {
	// A force rule pins the bucket even against a game-set Time Critical thread.
	f := &Facts{TimeCritical: true, ForcedBucket: BucketBackground}
	if b := BucketFor(RoleAudio, f); b != BucketBackground {
		t.Fatalf("bucket = %v, want background (force wins over time-critical)", b)
	}
}

func TestClassifyUnknownBeforeWarmup(t *testing.T) {
	// Fewer than three samples carry no reliable EMA signal.
	s := buildSeries(synthTick{state: process.StateWaiting, wait: process.WrQueue, cyclesPerTick: 5e7, switchesPerTick: 500}, 2)

	if role, _ := ClassifyRole(&Facts{Series: s, CyclesShare: 0.2}); role != RoleUnknown {
		t.Fatalf("role = %v, want unknown before warmup", role)
	}
}

func TestHysteresisStabilizes(t *testing.T) {
	c := NewClassifier(3)
	key := thread.Key{TID: 1}
	f := &Facts{
		Series:      buildSeries(synthTick{state: process.StateWaiting, wait: process.WrQueue, cyclesPerTick: 2e8, switchesPerTick: 500}, 40),
		CyclesShare: 0.2,
	}

	var v Verdict
	for i := 0; i < 3; i++ {
		v = c.Observe(key, f)
	}

	if !v.Stable {
		t.Fatalf("verdict not stable after 3 consistent windows: %+v", v)
	}
	if v.Role != RoleJobWorker {
		t.Fatalf("stable role = %v, want job-worker", v.Role)
	}
	if v.Bucket != BucketInteractive {
		t.Fatalf("bucket = %v, want interactive for job-worker", v.Bucket)
	}
}

// While hysteresis holds an older verdict, the reported confidence must rate
// the role being reported. It used to rate the current window's winner, so a
// row could show one role's name beside a different role's confidence.
func TestConfidenceRatesTheReportedRole(t *testing.T) {
	c := NewClassifier(3)
	key := thread.Key{TID: 1}

	worker := &Facts{
		Series:      buildSeries(synthTick{state: process.StateWaiting, wait: process.WrQueue, cyclesPerTick: 2e8, switchesPerTick: 500}, 40),
		CyclesShare: 0.2,
	}
	for i := 0; i < 3; i++ {
		c.Observe(key, worker)
	}

	// Same thread key, evidence now says input. The stable role still reads
	// job-worker for another two windows; the confidence must follow it down,
	// not report the input rule's near-certainty.
	pumping := &Facts{Series: worker.Series, CyclesShare: 0.2, IsForegroundInput: true}
	v := c.Observe(key, pumping)

	if v.Role != RoleJobWorker {
		t.Fatalf("role = %v, want job-worker still held by hysteresis", v.Role)
	}
	if v.Confidence > 0.5 {
		t.Fatalf("confidence %.2f rates the contradicted role too highly", v.Confidence)
	}
}

// A lone unopposed signal is the least informed classification there is; it
// must not be the one reporting perfect confidence.
func TestSingleSignalIsNotFullConfidence(t *testing.T) {
	s := buildSeries(synthTick{
		state:           process.StateWaiting,
		wait:            process.WrQueue,
		cyclesPerTick:   0,
		switchesPerTick: 10,
	}, 40)

	role, conf := ClassifyRole(&Facts{Series: s, CyclesShare: 0.001})
	if role != RolePoolIdle {
		t.Fatalf("role = %v, want pool-idle", role)
	}
	if conf >= 1 {
		t.Fatalf("confidence = %.2f for a single unopposed rule, want < 1", conf)
	}
}

// Ranking used to walk a map, so tied roles swapped winner at random each
// window; the hysteresis streak reset every tick and the thread was never
// actuated. Same evidence must always produce the same winner.
func TestRankIsDeterministic(t *testing.T) {
	sheet := newScoreSheet()
	sheet.add(RoleAudio, 3)
	sheet.add(RoleNetwork, 3)
	sheet.add(RoleLoader, 3)

	first, _ := sheet.rank()
	for i := 0; i < 200; i++ {
		if best, _ := sheet.rank(); best != first {
			t.Fatalf("rank returned %v then %v for identical scores", first, best)
		}
	}
}

// The governor promotes a critical thread to THREAD_PRIORITY_HIGHEST, which in
// a HIGH-class process lands on the same base priority as a game-set
// TIME_CRITICAL thread. Classifying from the live value therefore made the
// governor read its own promotion back as the game's intent and move the
// thread into untouchable — locking itself out of every thread it tuned.
func TestBaselineIgnoresOurOwnPromotion(t *testing.T) {
	const processBase = 13 // HIGH_PRIORITY_CLASS

	var s Series
	s.noteBaseline(processBase, processBase) // first sighting: game left it alone
	s.noteBaseline(15, processBase)          // after we promoted it to HIGHEST

	if s.BaselineRelative != 0 {
		t.Fatalf("BaselineRelative = %d, want 0: the baseline must not follow our own writes", s.BaselineRelative)
	}

	f := &Facts{TimeCritical: s.BaselineRelative >= gameElevatedPriority}
	if b := BucketFor(RoleMainSim, f); b != BucketCritical {
		t.Fatalf("bucket = %v, want critical: a thread we promoted is not game-elevated", b)
	}
}

// The same check must still respect a thread the game really did elevate.
func TestBaselineHonoursGameElevation(t *testing.T) {
	var s Series
	s.noteBaseline(15, 13) // already at HIGHEST the first time we saw it

	f := &Facts{TimeCritical: s.BaselineRelative >= gameElevatedPriority}
	if b := BucketFor(RoleAudio, f); b != BucketUntouchable {
		t.Fatalf("bucket = %v, want untouchable for a game-elevated thread", b)
	}
}
