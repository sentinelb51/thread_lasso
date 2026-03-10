package main

import (
	"ThreadOrchestra/config"
	"fmt"
	"time"

	"github.com/mitchellh/go-ps"
)

func main() {

	// Read config
	configuration, err := config.Load()
	if err != nil {
		panic(err)
	}

	game := findGame(configuration)
	fmt.Printf("Config: %+v\n", game)
}

func findGame(cfg config.Config) config.Game {
	if len(cfg.Games) == 0 {
		panic("No games defined in config")
	}

	for {
		processes, err := ps.Processes()
		if err != nil {
			panic(err)
		}

		for _, process := range processes {
			fmt.Println(process.Executable())
			game, found := cfg.Games[process.Executable()]
			if found {
				fmt.Println("Found game:", process.Executable())
				return game
			}
		}

		time.Sleep(1 * time.Second)
	}

}
