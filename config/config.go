package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const filename = "config.json"

// Path is where the config is read from and written back to: the working
// directory, which for a normal launch is the directory holding the binary.
func Path() string { return filename }

// Load reads config.json, resolves defaults, and returns any complaints about
// the file's contents alongside the config. Problems are non-fatal by design —
// a typo in one threshold should not stop the tool running — but they are
// surfaced rather than swallowed.
func Load() (config Config, problems []string, err error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return
	}

	if err = json.Unmarshal(data, &config); err != nil {
		return
	}

	for name, game := range config.Games {
		if game.Auto == nil {
			continue
		}
		for _, problem := range game.Auto.applyDefaults() {
			problems = append(problems, name+": "+problem)
		}
		config.Games[name] = game
	}

	return
}

// Save writes the config back, preserving everything Load read. The file is
// written to a sibling temp file and renamed, so an interrupted save cannot
// leave a half-written config behind.
//
// JSON has no comments to lose, but key order and formatting are normalised —
// worth knowing before hand-editing a file the settings panel also writes.
func Save(config Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	data = append(data, '\n')

	temp := filename + ".tmp"
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", temp, err)
	}
	if err := os.Rename(temp, filename); err != nil {
		os.Remove(temp)
		return fmt.Errorf("replacing %s: %w", filename, err)
	}

	return nil
}

// EnsureFile writes a starter config when none exists and reports whether it
// created one. Every tuning key is written out at its default rather than
// omitted: a config you can read top to bottom is the point, and the settings
// panel is easier to trust when the file already shows what it will write.
func EnsureFile() (bool, error) {
	if _, err := os.Stat(filename); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("checking %s: %w", filename, err)
	}

	if err := Save(starterConfig()); err != nil {
		return false, err
	}

	return true, nil
}

// starterConfig is a complete, valid config for a game that does not exist, so
// a first run does nothing until the placeholder is renamed. Better than an
// empty file: every key is present at its default, which is the fastest way to
// learn what there is to change.
func starterConfig() Config {
	auto := DefaultAuto()
	auto.Mode = "full"
	auto.Optimisation = "observe"

	return Config{
		Readme: []string{
			"Rename \"YourGame.exe\" to the executable you want tuned; the tool watches for it and attaches when it appears.",
			"optimisation: observe classifies without changing anything, manual applies only the threads[] rules, auto runs the governor.",
			"aggression selects the preset the tuning block below starts from: conservative never lowers anything, standard demotes only pool-idle and telemetry, aggressive enacts the whole table.",
			"Every key under tuning is described in the app's Settings tab, and by " + selfName() + " -settings.",
		},
		Games: map[string]Game{
			"YourGame.exe": {
				Priority:   "high",
				IOPriority: "high",
				Auto:       &auto,
			},
		},
	}
}

// selfName is the executable's own name, so generated help quotes a command the
// user can actually type.
func selfName() string {
	path, err := os.Executable()
	if err != nil {
		return "ThreadOrchestra"
	}

	return filepath.Base(path)
}
