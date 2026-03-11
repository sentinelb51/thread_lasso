package main

import (
	"ThreadOrchestra/config"
	"ThreadOrchestra/process"
	"ThreadOrchestra/scanner"
	"fmt"
)

func main() {

	configuration, err := config.Load()
	if err != nil {
		panic(err)
	}

	fmt.Printf("Waiting for a gameConfig...\n")
	gameConfig, gameProcess, err := scanner.Process(configuration)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Found game: %s\nConfig: %+v\n", gameProcess.Executable(), gameConfig)
	err = process.SetAffinity(uint32(gameProcess.Pid()), []int{0, 1, 2, 3, 4, 5, 6, 7})
	fmt.Println(err)
	fmt.Println("Set affinity to cores 0-7")
}
