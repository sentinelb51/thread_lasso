package scanner

import (
	"ThreadOrchestra/config"
	"context"
	"fmt"
	"time"

	"github.com/mitchellh/go-ps"
)

const pollingRate = 1 * time.Second

// Process scans all processes and returns the first game found from the
// config. It blocks until a game appears or ctx is cancelled — the caller owns
// the timeout, because the UI must be able to close the window while nothing
// has been found yet.
func Process(ctx context.Context, cfg config.Config) (config.Game, ps.Process, error) {
	if len(cfg.Games) == 0 {
		return config.Game{}, nil, fmt.Errorf("no games configured")
	}

	ticker := time.NewTicker(pollingRate)
	defer ticker.Stop()

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

		select {
		case <-ctx.Done():
			return config.Game{}, nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
