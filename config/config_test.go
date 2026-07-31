package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The settings panel writes config.json back through Save. Anything Load read
// and Save did not write is silently destroyed the first time someone changes a
// number in the UI, which makes round-tripping the single most important
// property of this package.

func TestSaveKeepsEverythingLoadRead(t *testing.T) {
	t.Chdir(t.TempDir())

	original := `{
	  "_readme": ["keep me"],
	  "games": {
	    "Overwatch.exe": {
	      "priority": "high",
	      "io_priority": "high",
	      "threads": [
	        {"name": "binkasy*", "priority": 0, "io_priority": 0},
	        {"name": "vivoxsdk*", "priority": 0, "io_priority": 0}
	      ],
	      "auto": {
	        "mode": "full",
	        "optimisation": "auto",
	        "aggression": "aggressive",
	        "exclude": ["Bink*"],
	        "force": [{"name": "Render*", "bucket": "critical"}],
	        "role_buckets": {"network/voice": "interactive"},
	        "critical_cores": [0, 2, 4]
	      }
	    },
	    "Other.exe": {"priority": "normal"}
	  }
	}`
	if err := os.WriteFile(filename, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, problems, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}

	// Change one setting, the way the settings panel does.
	game := loaded.Games["Overwatch.exe"]
	auto := *game.Auto
	auto.Tuning.Buckets.Interactive.Priority = -1
	auto.Tuning.Buckets.Interactive.PriorityMode = PriorityLower
	game.Auto = &auto
	loaded.Games["Overwatch.exe"] = game

	if err := Save(loaded); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, problems, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("reload problems: %v", problems)
	}

	back := reloaded.Games["Overwatch.exe"]
	switch {
	case len(reloaded.Readme) != 1 || reloaded.Readme[0] != "keep me":
		t.Errorf("readme lost: %v", reloaded.Readme)
	case back.Priority != "high" || back.IOPriority != "high":
		t.Errorf("process-level settings lost: %+v", back)
	case len(back.Threads) != 2 || back.Threads[0].Name != "binkasy*":
		t.Errorf("manual thread rules lost: %+v", back.Threads)
	case back.Threads[0].Priority == nil || *back.Threads[0].Priority != 0:
		t.Error("a manual priority of 0 was dropped as if it were unset")
	case len(back.Auto.Exclude) != 1 || len(back.Auto.Force) != 1:
		t.Errorf("exclude/force rules lost: %+v", back.Auto)
	case back.Auto.RoleBuckets["network/voice"] != "interactive":
		t.Errorf("role_buckets lost: %v", back.Auto.RoleBuckets)
	case len(back.Auto.CriticalCores) != 3:
		t.Errorf("critical_cores lost: %v", back.Auto.CriticalCores)
	case len(reloaded.Games) != 2:
		t.Errorf("a game went missing: %v", reloaded.Games)
	}

	// And the edit itself survived.
	if back.Auto.Tuning.Buckets.Interactive.Priority != -1 ||
		back.Auto.Tuning.Buckets.Interactive.PriorityMode != PriorityLower {
		t.Errorf("the saved edit did not come back: %+v", back.Auto.Tuning.Buckets.Interactive)
	}
	// While an untouched key still tracks the preset.
	if back.Auto.Tuning.Buckets.Critical.IOPriority != IoHigh {
		t.Error("an unedited key lost its preset on the round trip")
	}
}

// A saved file must be self-describing: every tuning key written out, so the
// file can be read top to bottom instead of guessed at.
func TestSavedTuningIsWrittenInFull(t *testing.T) {
	t.Chdir(t.TempDir())

	auto := DefaultAuto()
	if err := Save(Config{Games: map[string]Game{"g.exe": {Auto: &auto}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)

	for _, key := range []string{
		"starvation_ready_ratio", "priority_mode", "demote_roles",
		"cadence_cv", "stack_scans_per_tick", "safer_bucket_factor",
	} {
		if !strings.Contains(text, key) {
			t.Errorf("%q is missing from the saved file", key)
		}
	}

	// Role overrides are the exception: they are sparse by design, and writing
	// ten empty objects would bury the settings that are actually set.
	var probe struct {
		Games map[string]struct {
			Auto struct {
				Tuning struct {
					Roles map[string]map[string]any `json:"roles"`
				} `json:"tuning"`
			} `json:"auto"`
		} `json:"games"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Games["g.exe"].Auto.Tuning.Roles["audio"]) != 0 {
		t.Error("an empty role override should not be written out")
	}
}

func TestEnsureFileWritesAStarterOnlyWhenMissing(t *testing.T) {
	t.Chdir(t.TempDir())

	created, err := EnsureFile()
	if err != nil || !created {
		t.Fatalf("EnsureFile() = %v, %v; want a file to be created", created, err)
	}

	loaded, problems, err := Load()
	if err != nil {
		t.Fatalf("the generated config does not load: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("the generated config complains about itself: %v", problems)
	}
	if len(loaded.Readme) == 0 {
		t.Error("the starter config should explain itself")
	}

	game, ok := loaded.Games["YourGame.exe"]
	if !ok {
		t.Fatalf("no placeholder game: %v", loaded.Games)
	}
	// A placeholder must not tune anything if it is run as-is.
	if game.Auto.Optimisation != "observe" {
		t.Errorf("optimisation = %q; a config nobody has edited must not change threads", game.Auto.Optimisation)
	}

	// A second call must leave the file alone.
	if err := os.WriteFile(filename, []byte(`{"games":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	created, err = EnsureFile()
	if err != nil || created {
		t.Fatalf("EnsureFile() = %v, %v; want it to leave an existing file alone", created, err)
	}
	data, _ := os.ReadFile(filename)
	if string(data) != `{"games":{}}` {
		t.Error("EnsureFile overwrote an existing config")
	}
}

// The config shipped in the repo has to keep working, unchanged, across this
// whole schema move — that is the compatibility claim.
func TestTheRepositoryConfigStillLoads(t *testing.T) {
	data, err := os.ReadFile("../config.json")
	if err != nil {
		t.Skip("no config.json beside the module")
	}

	t.Chdir(t.TempDir())
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, problems, err := Load()
	if err != nil {
		t.Fatalf("the repository config no longer loads: %v", err)
	}
	for _, problem := range problems {
		t.Errorf("the repository config now complains: %s", problem)
	}
	for name, game := range loaded.Games {
		if game.Auto == nil {
			continue
		}
		if game.Auto.Tuning.Gates.PollIntervalMS == 0 {
			t.Errorf("%s: tuning was not populated", name)
		}
	}
}
