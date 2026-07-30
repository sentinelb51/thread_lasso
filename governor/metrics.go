//go:build windows && amd64

package governor

import (
	"ThreadOrchestra/process"
	"ThreadOrchestra/thread"
	"math"
	"time"
)

const (
	// EMA smoothing: short reacts within ~3 ticks (phase shifts), long
	// averages ~30 ticks (stable classification input).
	shortAlpha = 0.5
	longAlpha  = 0.065

	// Ring length for coefficient-of-variation and correlation windows.
	historyLen = 32

	// Wait-reason histogram decay per tick (exponential forgetting).
	waitDecay = 0.97

	waitReasonCount = 40

	// Threads created within 5s of the process count as founders.
	createdAtStartWindow = 5 * time.Second
)

// Series is the accumulated behavioral state of one thread. All values are
// derived purely from Samples — no syscalls — so the whole metrics layer is
// unit-testable with synthetic streams.
type Series struct {
	Key               thread.Key
	Win32StartAddress uintptr

	CyclesRateShort float64 // cycles/sec
	CyclesRateLong  float64
	SwitchRateShort float64 // context switches/sec
	SwitchRateLong  float64
	CyclesPerSwitch float64 // avg work quantum, EMA
	UserRatio       float64 // user/(user+kernel) time, EMA
	ReadyRatio      float64 // EMA of "observed in Ready state" = CPU starvation
	RunningRatio    float64
	Lifetime        time.Duration
	CreatedAtStart  bool
	Samples         int

	// BaselineRelative is the thread's priority relative to the process base,
	// captured the first time we ever saw the thread — before the governor
	// could have changed anything. Classification must use this, never the
	// live value: promoting a thread to THREAD_PRIORITY_HIGHEST moves its base
	// priority to exactly where a game-set TIME_CRITICAL thread sits, so
	// reading the live value makes the governor mistake its own work for the
	// game's intent and lock itself out of the thread.
	BaselineRelative int
	hasBaseline      bool

	waitHistogram [waitReasonCount]float64
	waitTotal     float64
	observeTotal  float64 // decayed sample count, same decay as waitTotal

	rateHist   [historyLen]float64
	switchHist [historyLen]float64
	histLen    int
	histPos    int

	hasPrev       bool
	prevCycles    uint64
	hasPrevCycles bool
	prevSwitches  uint32
	prevUser      int64
	prevKernel    int64
	prevAt        time.Time
}

func (s *Series) update(sample *ThreadSample, at time.Time, processCreateTime int64) {
	s.Win32StartAddress = sample.Win32StartAddress
	s.Lifetime = time.Duration(nowFiletimeDelta(at, sample.CreateTime))
	s.CreatedAtStart = sample.CreateTime-processCreateTime < int64(createdAtStartWindow/100)

	if !s.hasPrev {
		s.rebase(sample, at)
		return
	}

	dt := at.Sub(s.prevAt).Seconds()
	if dt <= 0 {
		return
	}
	s.Samples++

	// uint32 subtraction handles the cumulative counter wrapping at 2^32.
	switchDelta := float64(sample.ContextSwitches - s.prevSwitches)
	switchRate := switchDelta / dt
	s.SwitchRateShort = ema(s.SwitchRateShort, switchRate, shortAlpha)
	s.SwitchRateLong = ema(s.SwitchRateLong, switchRate, longAlpha)

	var cyclesRate float64
	if sample.HasCycles && s.hasPrevCycles && sample.Cycles >= s.prevCycles {
		cyclesDelta := float64(sample.Cycles - s.prevCycles)
		cyclesRate = cyclesDelta / dt
		s.CyclesRateShort = ema(s.CyclesRateShort, cyclesRate, shortAlpha)
		s.CyclesRateLong = ema(s.CyclesRateLong, cyclesRate, longAlpha)

		if switchDelta > 0 {
			s.CyclesPerSwitch = ema(s.CyclesPerSwitch, cyclesDelta/switchDelta, longAlpha)
		}
	}

	userDelta := float64(sample.UserTime - s.prevUser)
	kernelDelta := float64(sample.KernelTime - s.prevKernel)
	if total := userDelta + kernelDelta; total > 0 {
		s.UserRatio = ema(s.UserRatio, userDelta/total, longAlpha)
	}

	s.ReadyRatio = ema(s.ReadyRatio, boolTo1(sample.ThreadState == process.StateReady), longAlpha)
	s.RunningRatio = ema(s.RunningRatio, boolTo1(sample.ThreadState == process.StateRunning), longAlpha)

	for i := range s.waitHistogram {
		s.waitHistogram[i] *= waitDecay
	}
	s.waitTotal *= waitDecay
	s.observeTotal = s.observeTotal*waitDecay + 1
	if sample.ThreadState == process.StateWaiting && int(sample.WaitReason) < waitReasonCount {
		s.waitHistogram[sample.WaitReason]++
		s.waitTotal++
	}

	s.rateHist[s.histPos] = cyclesRate
	s.switchHist[s.histPos] = switchRate
	s.histPos = (s.histPos + 1) % historyLen
	if s.histLen < historyLen {
		s.histLen++
	}

	s.prevCycles = sample.Cycles
	s.hasPrevCycles = sample.HasCycles
	s.prevSwitches = sample.ContextSwitches
	s.prevUser = sample.UserTime
	s.prevKernel = sample.KernelTime
	s.prevAt = at
}

// rebase resets the counter baselines to sample without folding a rate. It is
// used for ticks the governor deliberately ignores (the game is unfocused):
// the cumulative counters keep climbing in the background, so re-baselining
// every ignored tick means the first folded tick after focus returns measures
// a clean single-interval delta instead of one spanning the whole gap.
func (s *Series) rebase(sample *ThreadSample, at time.Time) {
	s.hasPrev = true
	s.prevCycles = sample.Cycles
	s.hasPrevCycles = sample.HasCycles
	s.prevSwitches = sample.ContextSwitches
	s.prevUser = sample.UserTime
	s.prevKernel = sample.KernelTime
	s.prevAt = at
}

// DominantWait returns the most frequent wait reason and its share of all
// observed waits (0 when the thread was never seen waiting).
func (s *Series) DominantWait() (process.WaitReason, float64) {
	if s.waitTotal <= 0 {
		return 0, 0
	}

	best := 0
	for i, count := range s.waitHistogram {
		if count > s.waitHistogram[best] {
			best = i
		}
	}

	return process.WaitReason(best), s.waitHistogram[best] / s.waitTotal
}

// WaitShare returns the share of observed waits spent in the given reason.
func (s *Series) WaitShare(reason process.WaitReason) float64 {
	if s.waitTotal <= 0 || int(reason) >= waitReasonCount {
		return 0
	}

	return s.waitHistogram[reason] / s.waitTotal
}

// WaitShareAny is WaitShare summed over several reasons. Several kernel wait
// reasons come in pairs that mean the same thing to us — DelayExecution and
// WrDelayExecution are both "the thread called Sleep" — and keying a rule on
// only one half silently misses every thread reported as the other.
func (s *Series) WaitShareAny(reasons ...process.WaitReason) float64 {
	var share float64
	for _, reason := range reasons {
		share += s.WaitShare(reason)
	}

	return math.Min(1, share)
}

// WaitCoverage is the decayed fraction of samples in which the thread was seen
// waiting at all. WaitShare says which reason dominates the waits; coverage
// says how much of the thread's life those waits actually account for, so a
// "100%" reason resting on two observations can be discounted rather than
// trusted like one resting on fifty.
func (s *Series) WaitCoverage() float64 {
	if s.observeTotal <= 0 {
		return 0
	}

	return math.Min(1, s.waitTotal/s.observeTotal)
}

// noteBaseline records the thread's priority relative to the process base the
// first time the thread is seen, and never again. See Series.BaselineRelative.
func (s *Series) noteBaseline(threadBase, processBase int32) {
	if s.hasBaseline {
		return
	}

	s.hasBaseline = true
	s.BaselineRelative = int(threadBase - processBase)
}

// WakeRegularity is the coefficient of variation of the recent switch rate:
// low values indicate a fixed-tick thread (audio mixer, network tick), high
// values a bursty one (loader). Returns +Inf when there is no signal.
func (s *Series) WakeRegularity() float64 {
	mean, stddev := meanStddev(s.switchHist[:s.histLen])
	if mean <= 0 {
		return math.Inf(1)
	}

	return stddev / mean
}

// Correlation is the Pearson correlation of two threads' cycle-rate series.
// All live threads advance their rings in lockstep (one entry per tick), so
// the last n entries of each ring are time-aligned.
func Correlation(a, b *Series) float64 {
	n := a.histLen
	if b.histLen < n {
		n = b.histLen
	}
	if n < 8 {
		return 0 // not enough aligned history to be meaningful
	}

	var sa, sb []float64
	for i := n; i > 0; i-- {
		sa = append(sa, a.rateHist[(a.histPos-i+historyLen*2)%historyLen])
		sb = append(sb, b.rateHist[(b.histPos-i+historyLen*2)%historyLen])
	}

	meanA, stdA := meanStddev(sa)
	meanB, stdB := meanStddev(sb)
	if stdA == 0 || stdB == 0 {
		return 0
	}

	var cov float64
	for i := 0; i < n; i++ {
		cov += (sa[i] - meanA) * (sb[i] - meanB)
	}

	return cov / (float64(n) * stdA * stdB)
}

// Tracker maintains Series for every live thread plus process-level rates.
type Tracker struct {
	Series map[thread.Key]*Series

	TotalCyclesRate float64 // process-wide cycles/sec, EMA
	ReadRate        float64 // disk read bytes/sec, EMA — loading indicator

	hasPrev        bool
	prevProcCycles uint64
	prevRead       int64
	prevAt         time.Time
}

func NewTracker() *Tracker {
	return &Tracker{Series: make(map[thread.Key]*Series)}
}

// Update folds one Sample into the per-thread series and process-level rates,
// and drops series for threads that no longer exist.
func (t *Tracker) Update(sample *Sample) {
	if t.hasPrev {
		dt := sample.At.Sub(t.prevAt).Seconds()
		if dt > 0 {
			if sample.Process.CycleTime >= t.prevProcCycles {
				t.TotalCyclesRate = ema(t.TotalCyclesRate,
					float64(sample.Process.CycleTime-t.prevProcCycles)/dt, shortAlpha)
			}
			if sample.Process.ReadTransferCount >= t.prevRead {
				t.ReadRate = ema(t.ReadRate,
					float64(sample.Process.ReadTransferCount-t.prevRead)/dt, shortAlpha)
			}
		}
	}
	t.rebaseProcess(sample)
	t.syncSeries(sample, true)
}

// Rebase advances only the counter baselines — process-level and per-thread —
// without folding any rate. The governor calls it on ticks it must ignore (the
// game is unfocused and therefore throttled by Windows): metrics hold their
// last focused values, and the next Update after focus returns still measures a
// clean single-interval delta. See Series.rebase.
func (t *Tracker) Rebase(sample *Sample) {
	t.rebaseProcess(sample)
	t.syncSeries(sample, false)
}

// rebaseProcess snapshots the process-level counters as the new baseline.
func (t *Tracker) rebaseProcess(sample *Sample) {
	t.hasPrev = true
	t.prevProcCycles = sample.Process.CycleTime
	t.prevRead = sample.Process.ReadTransferCount
	t.prevAt = sample.At
}

// syncSeries creates/prunes per-thread series to match the sample. When fold is
// true each live series folds this window into its metrics; when false it only
// rebaselines its counters (an ignored tick).
func (t *Tracker) syncSeries(sample *Sample, fold bool) {
	seen := make(map[thread.Key]bool, len(sample.Threads))
	for i := range sample.Threads {
		threadSample := &sample.Threads[i]
		key := thread.Key{TID: threadSample.TID, CreateTime: threadSample.CreateTime}
		seen[key] = true

		series, ok := t.Series[key]
		if !ok {
			series = &Series{Key: key}
			t.Series[key] = series
		}

		// Captured on both paths: a thread first seen on an ignored (unfocused)
		// tick must still get its pre-tuning baseline recorded.
		series.noteBaseline(threadSample.BasePriority, sample.Process.BasePriority)

		if fold {
			series.update(threadSample, sample.At, sample.Process.CreateTime)
		} else {
			series.rebase(threadSample, sample.At)
		}
	}

	for key := range t.Series {
		if !seen[key] {
			delete(t.Series, key)
		}
	}
}

// HottestGameThread returns the series with the highest long-term cycle rate
// — the reference for frame correlation. Returns nil until data exists.
func (t *Tracker) HottestGameThread() *Series {
	var best *Series
	for _, series := range t.Series {
		if best == nil || series.CyclesRateLong > best.CyclesRateLong {
			best = series
		}
	}

	return best
}

func ema(current, value, alpha float64) float64 {
	return current + alpha*(value-current)
}

func meanStddev(values []float64) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}

	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(len(values))

	var variance float64
	for _, v := range values {
		variance += (v - mean) * (v - mean)
	}

	return mean, math.Sqrt(variance / float64(len(values)))
}

func boolTo1(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// nowFiletimeDelta converts "now minus a FILETIME creation stamp" to
// nanoseconds.
func nowFiletimeDelta(now time.Time, createFiletime int64) int64 {
	const filetimeEpochOffset = 116444736000000000 // 1601→1970 in 100ns ticks
	nowFiletime := now.UnixNano()/100 + filetimeEpochOffset
	return (nowFiletime - createFiletime) * 100
}
