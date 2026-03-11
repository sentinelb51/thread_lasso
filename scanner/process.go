package scanner

import (
	"ThreadOrchestra/config"
	"fmt"
	"time"

	"github.com/mitchellh/go-ps"
)

const pollingRate = 1 * time.Second

// Process scans all processes and returns the first game found from the config.
// This is a blocking function
func Process(cfg config.Config) (config.Game, ps.Process, error) {
	if len(cfg.Games) == 0 {
		return config.Game{}, nil, fmt.Errorf("no games configured")
	}

	for {
		processes, err := ps.Processes()
		if err != nil {
			return config.Game{}, nil, err
		}

		for _, process := range processes {
			name := process.Executable()

			game, found := cfg.Games[name]
			if found {
				return game, process, nil
			}
		}

		time.Sleep(pollingRate)
	}
}
