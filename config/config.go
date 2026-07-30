package config

import (
	"encoding/json"
	"os"
)

const filename = "config.json"

func Load() (config Config, err error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return
	}

	if err = json.Unmarshal(data, &config); err != nil {
		return
	}

	for name, game := range config.Games {
		if game.Auto != nil {
			game.Auto.applyDefaults()
			config.Games[name] = game
		}
	}

	return
}
