//go:build windows && amd64

package governor

import (
	"strings"
	"testing"

	"ThreadOrchestra/process"
)

// quietSeries is a thread doing nothing interesting: no cycles, a handful of
// wakes, no parking primitive. Every behavioural rule scores zero on it, so
// whatever a test adds on top is the only thing being measured.
func quietSeries() *Series {
	return buildSeries(synthTick{
		state:           process.StateWaiting,
		wait:            process.WrUserRequest,
		switchesPerTick: 5,
	}, 40)
}

// Stack evidence is the only identification that survives a process scrubbing
// its start addresses, and it answers a better question than the entry point
// did: not what created the thread, but what the thread is doing.
func TestStackIdentifiesWork(t *testing.T) {
	tests := []struct {
		name  string
		stack []string
		want  Role
	}{
		{
			name:  "the vendor driver outranks the runtime above it",
			stack: []string{"nvwgf2umx.dll", "dxgi.dll", "ntdll.dll"},
			want:  RoleGPUWorker,
		},
		{
			name:  "the D3D runtime alone is a submitting thread",
			stack: []string{"dxgi.dll", "d3d12.dll", "ntdll.dll"},
			want:  RoleRenderSubmit,
		},
		{
			name:  "the runtime above the graphics syscall stubs is the flip path",
			stack: []string{"dxgi.dll", "win32u.dll", "ntdll.dll"},
			want:  RoleRenderSubmit,
		},
		{
			name:  "winsock frames are a network thread",
			stack: []string{"mswsock.dll", "ws2_32.dll", "ntdll.dll"},
			want:  RoleNetwork,
		},
		{
			name:  "the audio session API is an audio thread",
			stack: []string{"audioses.dll", "mmdevapi.dll"},
			want:  RoleAudio,
		},
		{
			name:  "raw input is an input thread",
			stack: []string{"gameinput.dll", "user32.dll"},
			want:  RoleInput,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := &Facts{Series: quietSeries(), CyclesShare: 0.2, Stack: test.stack}
			if role, confidence := ClassifyRole(f); role != test.want {
				t.Errorf("role = %v (conf %.2f), want %v", role, confidence, test.want)
			}
		})
	}
}

// A thread with no stack evidence must not be pushed anywhere by its absence.
func TestNoStackEvidenceScoresNothing(t *testing.T) {
	var sheet scoreSheet
	scoreStack(&sheet, &Facts{Series: quietSeries()})

	for role := Role(1); int(role) < roleCount; role++ {
		if sheet.score[role] != 0 {
			t.Errorf("%v scored %.1f from an empty stack", role, sheet.score[role])
		}
	}
}

// Windows has no per-thread network counters. Pending I/O says the thread is
// waiting on a device; the stack says which one.
func TestPendingIoIsAttributedByTheStack(t *testing.T) {
	socket := quietSeries()
	socket.IoPendingRatio = 0.9
	network := scoreRoles(&Facts{Series: socket, CyclesShare: 0.02, Stack: []string{"mswsock.dll"}})

	file := quietSeries()
	file.IoPendingRatio = 0.9
	loader := scoreRoles(&Facts{Series: file, CyclesShare: 0.02, Stack: []string{"kernelbase.dll"}})

	if network.score[RoleNetwork] <= 4 {
		t.Errorf("network score = %.1f, want the stack match plus the pending-I/O bonus",
			network.score[RoleNetwork])
	}
	if loader.score[RoleLoader] == 0 {
		t.Error("pending I/O without socket frames scored nothing towards loader")
	}
	if loader.score[RoleNetwork] != 0 {
		t.Errorf("network scored %.1f on a thread with no socket frames", loader.score[RoleNetwork])
	}
}

// Idle threads with no pending I/O must not collect either score.
func TestNoPendingIoScoresNothing(t *testing.T) {
	sheet := scoreRoles(&Facts{Series: quietSeries(), CyclesShare: 0.02, Stack: []string{"mswsock.dll"}})
	if sheet.score[RoleNetwork] != 4 {
		t.Errorf("network score = %.1f, want exactly the stack match (4)", sheet.score[RoleNetwork])
	}
}

func TestDefaultRoleBucketsKeepsPumpsOutOfTheCriticalSet(t *testing.T) {
	buckets := DefaultRoleBuckets()

	if got := buckets.Of(RoleAudio); got != BucketInteractive {
		t.Errorf("audio bucket = %v, want interactive", got)
	}
	if got := buckets.Of(RoleNetwork); got != BucketBackground {
		t.Errorf("network bucket = %v, want background", got)
	}
	// The frame-critical set is what they were moved out of the way of.
	for _, role := range []Role{RoleMainSim, RoleRenderSubmit, RoleGPUWorker, RoleInput} {
		if got := buckets.Of(role); got != BucketCritical {
			t.Errorf("%v bucket = %v, want critical", role, got)
		}
	}
	if got := buckets.Of(RoleUnknown); got != BucketNone {
		t.Errorf("unknown bucket = %v, want none: an unclassified thread is left alone", got)
	}
}

func TestParseRoleBuckets(t *testing.T) {
	buckets, problems := ParseRoleBuckets(map[string]string{"network/voice": "interactive"})
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if got := buckets.Of(RoleNetwork); got != BucketInteractive {
		t.Errorf("network bucket = %v, want the override (interactive)", got)
	}
	if got := buckets.Of(RoleAudio); got != BucketInteractive {
		t.Errorf("audio bucket = %v, want the default; an override must not disturb other roles", got)
	}
}

// A typo in a policy override is silent misconfiguration otherwise: the role
// keeps its default and nothing says the config line did nothing.
func TestParseRoleBucketsReportsUnusableEntries(t *testing.T) {
	buckets, problems := ParseRoleBuckets(map[string]string{
		"netwrok":       "interactive",
		"audio":         "urgent",
		"network/voice": "critical",
	})

	if len(problems) != 2 {
		t.Fatalf("problems = %v, want 2 (an unknown role and an unknown bucket)", problems)
	}
	if !strings.Contains(strings.Join(problems, " "), "netwrok") {
		t.Errorf("problems = %v, want the misspelled role named", problems)
	}
	if got := buckets.Of(RoleAudio); got != BucketInteractive {
		t.Errorf("audio bucket = %v, want the default: an unusable bucket name must not apply", got)
	}
	// The valid entry in the same map still takes effect.
	if got := buckets.Of(RoleNetwork); got != BucketCritical {
		t.Errorf("network bucket = %v, want critical", got)
	}
}

func TestDescribeSources(t *testing.T) {
	tests := []struct {
		name   string
		counts map[EntrySource]int
		total  int
		want   string
	}{
		{
			name:   "an unprotected process",
			counts: map[EntrySource]int{EntryWin32: 40},
			total:  40,
			want:   "40 via Win32StartAddress",
		},
		{
			name:   "a process that scrubbed both snapshot fields",
			counts: map[EntrySource]int{EntryStack: 74, EntryNone: 2},
			total:  76,
			want:   "74 via stack scan, 2 unrecovered",
		},
		{
			name:   "nothing worked",
			counts: map[EntrySource]int{EntryNone: 76},
			total:  76,
			want:   "76 unrecovered",
		},
		{
			name:   "limited mode has no identifier at all",
			counts: nil,
			total:  76,
			want:   "unavailable (limited mode)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := describeSources(test.counts, test.total); got != test.want {
				t.Errorf("describeSources() = %q, want %q", got, test.want)
			}
		})
	}
}
