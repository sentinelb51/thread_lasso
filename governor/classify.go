//go:build windows && amd64

package governor

import (
	"ThreadOrchestra/process"
	"ThreadOrchestra/thread"
	"math"
	"strings"
)

type Role int

const (
	RoleUnknown Role = iota
	RoleMainSim
	RoleRenderSubmit
	RoleGPUWorker
	RoleAudio
	RoleInput
	RoleNetwork // includes voice chat
	RoleJobWorker
	RoleLoader
	RolePoolIdle
	RoleTelemetry
)

var roleNames = map[Role]string{
	RoleUnknown:      "unknown",
	RoleMainSim:      "main/sim",
	RoleRenderSubmit: "render",
	RoleGPUWorker:    "gpu-driver",
	RoleAudio:        "audio",
	RoleInput:        "input",
	RoleNetwork:      "network/voice",
	RoleJobWorker:    "job-worker",
	RoleLoader:       "loader",
	RolePoolIdle:     "pool-idle",
	RoleTelemetry:    "telemetry",
}

func (r Role) String() string { return roleNames[r] }

// allRoles is the fixed scoring/ranking order. Ranking used to walk a map,
// so two roles tied at the top swapped winner at random every window; the
// hysteresis streak then reset every tick and the thread could never reach a
// stable verdict, meaning it was never actuated. Iterating a slice makes ties
// resolve to the earlier (more specific) role, deterministically.
var allRoles = []Role{
	RoleMainSim, RoleRenderSubmit, RoleGPUWorker, RoleAudio, RoleInput,
	RoleNetwork, RoleJobWorker, RoleLoader, RolePoolIdle, RoleTelemetry,
}

type Bucket int

const (
	BucketNone Bucket = iota // unknown → leave alone
	BucketUntouchable
	BucketCritical
	BucketInteractive
	BucketBackground
)

var bucketNames = map[Bucket]string{
	BucketNone:        "-",
	BucketUntouchable: "untouchable",
	BucketCritical:    "critical",
	BucketInteractive: "interactive",
	BucketBackground:  "background",
}

func (b Bucket) String() string { return bucketNames[b] }

// ModuleResolver maps a start address to a lowercase module base name
// ("amdxx64.dll"). nil / empty result in limited mode.
type ModuleResolver func(addr uintptr) string

// Facts is the complete evidence about one thread for a single window.
type Facts struct {
	Series            *Series
	Description       string
	Module            string // lowercase base name; "" when unknown
	TimeCritical      bool   // the *game* elevated this thread (see Series.BaselineRelative)
	PriorityBoosted   bool   // the game nudged it above the process base, but not to the top
	IsForegroundInput bool
	CohortSize        int     // threads that look identical to this one (see buildCohorts)
	CyclesShare       float64 // CyclesRateLong / hottest thread's rate
	FrameCorrelation  float64 // vs the hottest game thread; 0 for the hottest itself

	// Config overrides, resolved by the governor from the auto.exclude and
	// auto.force rules before classification is bucketed.
	Excluded     bool   // matched an exclude glob — never touch this thread
	ForcedBucket Bucket // a force rule pins the bucket; BucketNone = not forced
}

// Classification thresholds. Cycle-rate numbers assume multi-GHz cores;
// they separate "parked" from "doing real work", not exact CPU percentages.
const (
	// Absolute cycle-rate landmarks. On a ~4.5 GHz core these are roughly
	// 0.02% and 1% of one core: below parked a thread is doing nothing at all,
	// above active it is working continuously.
	parkedCyclesRate = 1e6
	activeCyclesRate = 5e7

	// A pool member below this wake rate is idling between jobs rather than
	// servicing a queue.
	poolIdleSwitchRate = 100

	pumpSwitchRate = 300 // wakes/sec typical of audio/net/input pumps
	pumpMaxQuantum = 3e4 // cycles per wake for a pump
	regularWakeCV  = 0.5
	burstyWakeCV   = 1.5
	minScore       = 2.0 // below this the thread stays Unknown/untouched

	// Share of waits spent on a parking primitive above which that primitive
	// is the dominant story for the thread.
	poolWaitShare = 0.5

	// Cohorts of at least this many statistically identical threads are a
	// worker pool. Roles a process only has one of are scored down for members.
	poolCohortSize = 3

	// Thread priority relative to the process base at or above which the game
	// is deliberately prioritising the thread. Note this is inherently
	// ambiguous on Windows: in a dynamic priority class,
	// THREAD_PRIORITY_TIME_CRITICAL and THREAD_PRIORITY_HIGHEST both land a
	// HIGH-class thread on base priority 15. Both mean "the game wants this
	// thread scheduled", which is all the bucket policy needs.
	gameElevatedPriority = 2
)

var gpuDriverModules = []string{
	"amdxx64", "atidxx64", "nvwgf2umx", "nvd3dumx", "nvoglv64", "igd10iumd64", "igd12umd64",
}

var audioModules = []string{"bink", "xaudio", "mmdevapi", "audioses", "wasapi"}

var networkModules = []string{"vivoxsdk", "mswsock", "ws2_32", "winhttp", "wininet"}

// nameHints maps substrings of thread names (SetThreadDescription) to roles.
var nameHints = []struct {
	substr string
	role   Role
	weight float64
}{
	{"binkasy", RoleAudio, 3},
	{"audio", RoleAudio, 4},
	{"sound", RoleAudio, 4},
	{"mixer", RoleAudio, 3},
	{"render", RoleRenderSubmit, 4},
	{"rhi", RoleRenderSubmit, 3},
	{"gfx", RoleRenderSubmit, 3},
	{"present", RoleRenderSubmit, 3},
	{"input", RoleInput, 4},
	{"net", RoleNetwork, 3},
	{"sock", RoleNetwork, 3},
	{"voice", RoleNetwork, 3},
	{"job", RoleJobWorker, 2},
	{"worker", RoleJobWorker, 2},
	{"task", RoleJobWorker, 1.5},
	{"pool", RoleJobWorker, 1.5},
	{"loader", RoleLoader, 3},
	{"stream", RoleLoader, 2},
	{"telemetry", RoleTelemetry, 4},
	{"log", RoleTelemetry, 1.5},
}

// scoreSheet accumulates per-role evidence for one window. hits counts how many
// independent rules backed each role, which is what separates "three signals
// agree" from "one signal fired and nothing contradicted it" — the latter used
// to be reported as 100% confidence.
type scoreSheet struct {
	score map[Role]float64
	hits  map[Role]int
}

func newScoreSheet() *scoreSheet {
	return &scoreSheet{score: make(map[Role]float64, len(allRoles)), hits: make(map[Role]int, len(allRoles))}
}

// add records points for a role. Graded rules routinely evaluate to zero, and
// a zero-weight rule is not evidence, so it counts as neither score nor hit.
func (sheet *scoreSheet) add(role Role, points float64) {
	if points <= 0 {
		return
	}

	sheet.score[role] += points
	sheet.hits[role]++
}

// rank returns the top two roles in deterministic order. See allRoles.
func (sheet *scoreSheet) rank() (best, second Role) {
	best, second = RoleUnknown, RoleUnknown
	for _, role := range allRoles {
		switch {
		case best == RoleUnknown || sheet.score[role] > sheet.score[best]:
			second, best = best, role
		case second == RoleUnknown || sheet.score[role] > sheet.score[second]:
			second = role
		}
	}

	return best, second
}

// winner is the top-ranked role once it clears minScore.
func (sheet *scoreSheet) winner() Role {
	best, _ := sheet.rank()
	if best == RoleUnknown || sheet.score[best] < minScore {
		return RoleUnknown
	}

	return best
}

// confidence rates how well the evidence supports a specific role — the role
// actually being reported, which is not always this window's winner.
func (sheet *scoreSheet) confidence(role Role) float64 {
	if role == RoleUnknown {
		return 0
	}

	best := sheet.score[role]
	if best < minScore {
		return 0
	}

	// The runner-up is floored: an unopposed signal is not certainty, it is
	// just the only thing that fired. Without the floor, the *least* informed
	// classifications were the ones reporting 100%.
	rival := minScore / 2
	for _, other := range allRoles {
		if other != role && sheet.score[other] > rival {
			rival = sheet.score[other]
		}
	}

	margin := clamp01((best - rival) / best)
	support := clamp01(float64(sheet.hits[role]) / 2)

	return margin * (0.5 + 0.5*support)
}

// scoreRoles grades every role against the evidence. Rules are graded rather
// than gated: a thread at 0.23 cycles-share is not categorically different from
// one at 0.26, so evidence ramps in over a band instead of switching on at a
// threshold. Pure function.
func scoreRoles(f *Facts) *scoreSheet {
	sheet := newScoreSheet()
	s := f.Series

	// EMAs and the wait histogram carry no signal in the first windows; a
	// fresh thread would otherwise match the "parked" rule spuriously.
	if s == nil || s.Samples < 3 {
		return sheet
	}

	// Hard evidence first: the thread pumping the foreground window's
	// message queue is the input thread.
	if f.IsForegroundInput {
		sheet.add(RoleInput, 10)
	}

	// A pool member cannot be the main thread — a process has exactly one.
	pooled := f.CohortSize >= poolCohortSize

	// Hot crunchers: the top cycle consumers are the frame-critical set.
	hot := ramp(f.CyclesShare, 0.30, 0.60)
	if pooled {
		sheet.add(RoleJobWorker, 2*hot)
	} else {
		sheet.add(RoleMainSim, 3*hot)
		if s.CreatedAtStart {
			sheet.add(RoleMainSim, 1) // a founder thread, created with the process
		}
	}

	// Long uninterrupted work quanta at a high absolute rate: simulation or
	// heavy worker code, as opposed to a pump doing a sliver of work per wake.
	cruncher := math.Min(ramp(s.CyclesRateLong, 2e8, 1e9), ramp(s.CyclesPerSwitch, 2e5, 1e6))
	if pooled {
		sheet.add(RoleJobWorker, 1.5*cruncher)
	} else {
		sheet.add(RoleMainSim, 1.5*cruncher)
	}

	// Render submit: a large share of the cycles delivered in many small wakes.
	// Both conditions must hold, so the weaker of the two ramps governs.
	sheet.add(RoleRenderSubmit, 2.5*math.Min(
		ramp(f.CyclesShare, 0.15, 0.35),
		ramp(s.SwitchRateLong, 1000, 3000),
	))

	// Frame-coupled activity marks render/sim helpers even without modules.
	// At poll-interval granularity this is a load-envelope correlation, not
	// true frame coupling, so it nudges rather than decides.
	coupled := ramp(f.FrameCorrelation, 0.6, 0.9)
	sheet.add(RoleRenderSubmit, 2*coupled)
	sheet.add(RoleMainSim, 1*coupled)

	// Pumps: many tiny wakes. The game marking a pump Time Critical is a
	// strong audio signal (see TID 11068 in the captures).
	isPump := s.SwitchRateLong > pumpSwitchRate && s.CyclesPerSwitch > 0 && s.CyclesPerSwitch < pumpMaxQuantum
	if isPump {
		sheet.add(RoleAudio, 2)
		sheet.add(RoleNetwork, 1.5)
		if s.WakeRegularity() < regularWakeCV {
			sheet.add(RoleAudio, 1)
		}
		if f.TimeCritical {
			sheet.add(RoleAudio, 2)
		}
	}
	if f.PriorityBoosted && !f.TimeCritical {
		sheet.add(RoleAudio, 1)
		sheet.add(RoleNetwork, 1)
	}

	// Parking primitives. WrQueue is the classic thread-pool KQUEUE wait, but
	// no modern engine is limited to it: WrAlertByThreadId is
	// NtWaitForAlertByThreadId, the futex behind WaitOnAddress / SRWLOCK /
	// condition variables, and it is where Overwatch's job system parks. Keying
	// only on WrQueue made RoleJobWorker and RolePoolIdle unreachable on such a
	// process. The futex is weighted lower because a busy thread blocked on a
	// contended lock waits there too, and coverage discounts a dominant reason
	// that rests on very few observations.
	parked := (s.WaitShare(process.WrQueue) + 0.75*s.WaitShare(process.WrAlertByThreadId)) * s.WaitCoverage()
	if parked > poolWaitShare {
		switch {
		case s.CyclesRateLong < parkedCyclesRate:
			sheet.add(RolePoolIdle, 4*parked)
		case s.CyclesRateLong < activeCyclesRate && s.SwitchRateLong < poolIdleSwitchRate:
			sheet.add(RolePoolIdle, 3*parked)
		default:
			sheet.add(RoleJobWorker, 4*parked)
		}
	}

	// A cohort of behaviourally identical threads is a pool of some kind; which
	// kind is left to the rules above.
	if pooled {
		sheet.add(RoleJobWorker, 1)
		sheet.add(RolePoolIdle, 1)
	}

	// Sleep loops that never do real work. The kernel reports plain
	// DelayExecution for a user-mode Sleep and WrDelayExecution for the waiting
	// variant; checking only the latter missed every Sleep-looping thread.
	sleeping := s.WaitShareAny(process.DelayExecution, process.WrDelayExecution) * s.WaitCoverage()
	if sleeping > poolWaitShare && s.Lifetime.Seconds() > 60 {
		sheet.add(RoleTelemetry, 3*sleeping*(1-ramp(f.CyclesShare, 0.005, 0.05)))
	}

	// Parked regardless of wait reason.
	if s.SwitchRateLong < 1 && s.CyclesRateLong < parkedCyclesRate {
		sheet.add(RolePoolIdle, 2.5)
	}

	// Bursty, kernel-heavy, mid-activity: streaming/asset loading.
	if s.WakeRegularity() > burstyWakeCV && s.UserRatio < 0.5 &&
		f.CyclesShare > 0.02 && f.CyclesShare < 0.3 {
		sheet.add(RoleLoader, 1.5)
	}

	// Module evidence (full mode only).
	if f.Module != "" {
		switch {
		case matchesAny(f.Module, gpuDriverModules):
			if f.CyclesShare > 0.15 || s.SwitchRateLong > 5000 {
				sheet.add(RoleGPUWorker, 4)
			} else {
				sheet.add(RolePoolIdle, 2) // parked driver pool (shader compilers)
			}
		case matchesAny(f.Module, audioModules):
			sheet.add(RoleAudio, 4)
		case matchesAny(f.Module, networkModules):
			sheet.add(RoleNetwork, 4)
		case strings.HasPrefix(f.Module, "ntdll"):
			sheet.add(RolePoolIdle, 1.5)
		}
	}

	// Thread-name evidence.
	if f.Description != "" {
		name := strings.ToLower(f.Description)
		for _, hint := range nameHints {
			if strings.Contains(name, hint.substr) {
				sheet.add(hint.role, hint.weight)
			}
		}
	}

	return sheet
}

// ClassifyRole scores every role against the evidence and returns the winner
// with a 0..1 confidence. Pure function.
func ClassifyRole(f *Facts) (Role, float64) {
	sheet := scoreRoles(f)
	role := sheet.winner()

	return role, sheet.confidence(role)
}

// ramp is a clamped linear interpolation: 0 at or below lo, 1 at or above hi.
// It replaces the classifier's step thresholds so a thread just short of a cut
// still contributes proportional evidence instead of scoring nothing.
func ramp(value, lo, hi float64) float64 {
	if hi <= lo {
		return boolTo1(value >= hi)
	}

	return clamp01((value - lo) / (hi - lo))
}

func clamp01(v float64) float64 { return math.Min(1, math.Max(0, v)) }

// parseBucket maps a config bucket name to its Bucket; unknown names yield
// BucketNone, which force rules treat as "no override".
func parseBucket(name string) Bucket {
	switch name {
	case "critical":
		return BucketCritical
	case "interactive":
		return BucketInteractive
	case "background":
		return BucketBackground
	case "untouchable":
		return BucketUntouchable
	default:
		return BucketNone
	}
}

// BucketFor maps a role to its tuning bucket. Config overrides win first: an
// exclude match makes a thread untouchable, a force rule pins its bucket
// outright. Otherwise the game's own elevated threads are untouchable — except
// vivoxsdk voice threads (user decision: voice chat may be tuned).
//
// "The game's own" is load-bearing: f.TimeCritical is derived from the
// priority baseline captured before the governor touched anything. Reading the
// live priority instead made every thread the governor promoted to
// THREAD_PRIORITY_HIGHEST look game-elevated on the next tick, so the governor
// promoted threads straight into its own untouchable bucket and never tuned
// them again.
func BucketFor(role Role, f *Facts) Bucket {
	if f.Excluded {
		return BucketUntouchable
	}
	if f.ForcedBucket != BucketNone {
		return f.ForcedBucket
	}
	if f.TimeCritical && !matchesAny(f.Module, []string{"vivoxsdk"}) {
		return BucketUntouchable
	}

	switch role {
	case RoleMainSim, RoleRenderSubmit, RoleGPUWorker, RoleAudio, RoleInput:
		return BucketCritical
	case RoleNetwork, RoleJobWorker, RoleLoader:
		return BucketInteractive
	case RolePoolIdle, RoleTelemetry:
		return BucketBackground
	default:
		return BucketNone
	}
}

func matchesAny(value string, substrings []string) bool {
	for _, substr := range substrings {
		if strings.Contains(value, substr) {
			return true
		}
	}
	return false
}

// Verdict is the hysteresis-filtered classification of one thread.
type Verdict struct {
	Role       Role
	Confidence float64
	Bucket     Bucket
	Stable     bool // held for >= stableWindows consecutive windows
	Streak     int
}

type roleState struct {
	stable    Role
	candidate Role
	streak    int
}

// Classifier applies K-window stability filtering on top of per-window
// scoring so a briefly-spiking thread doesn't flap between buckets.
type Classifier struct {
	stableWindows int
	states        map[thread.Key]*roleState
}

func NewClassifier(stableWindows int) *Classifier {
	return &Classifier{
		stableWindows: stableWindows,
		states:        make(map[thread.Key]*roleState),
	}
}

// Observe folds one window's scored role into the thread's hysteresis state.
func (c *Classifier) Observe(key thread.Key, f *Facts) Verdict {
	sheet := scoreRoles(f)
	role := sheet.winner()

	state, ok := c.states[key]
	if !ok {
		state = &roleState{candidate: role, stable: RoleUnknown}
		c.states[key] = state
	}

	if role == state.candidate {
		state.streak++
	} else {
		state.candidate = role
		state.streak = 1
	}

	if state.streak >= c.stableWindows {
		state.stable = state.candidate
	}

	verdict := Verdict{
		Role: state.stable,
		// Rated for the role being reported, not for this window's winner.
		// The two disagree whenever hysteresis is holding an older verdict,
		// and the row then showed one role's name beside another's confidence.
		Confidence: sheet.confidence(state.stable),
		Bucket:     BucketFor(state.stable, f),
		Stable:     state.stable == state.candidate && state.streak >= c.stableWindows,
		Streak:     state.streak,
	}

	return verdict
}

// Prune drops hysteresis state for threads no longer tracked.
func (c *Classifier) Prune(live map[thread.Key]*Series) {
	for key := range c.states {
		if _, ok := live[key]; !ok {
			delete(c.states, key)
		}
	}
}
