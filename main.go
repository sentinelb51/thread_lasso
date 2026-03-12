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

	fmt.Printf("Waiting for a game...\n")
	gameConfig, gameProcess, err := scanner.Process(configuration)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Found game: %s\nConfig: %+v\n", gameProcess.Executable(), gameConfig)

	affinities, err := process.CpuSets(uint32(gameProcess.Pid()))
	if err != nil {
		panic(err)
	}

	fmt.Println("CPU sets before:", affinities)
	err = process.SetCpuSets(uint32(gameProcess.Pid()), []int{0, 1, 2, 3, 4, 5, 6, 7})

	affinities, err = process.CpuSets(uint32(gameProcess.Pid()))
	if err != nil {
		panic(err)
	}
	fmt.Println("CPU sets after:", affinities)

	fmt.Println(err)
	fmt.Println("Set affinity to cores 0-7")
}
