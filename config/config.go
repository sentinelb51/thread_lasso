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

	err = json.Unmarshal(data, &config)
	return
}
