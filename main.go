package main

import (
	"ThreadOrchestra/config"
	"ThreadOrchestra/governor"
	"ThreadOrchestra/scanner"
	"ThreadOrchestra/ui"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

// A game that has just exited can linger in the process list for a moment.
// Settling before the next scan stops the supervisor from re-attaching to a
// pid that is already tearing down.
const rescanDelay = 2 * time.Second

// The window closes before the governor has reverted its thread changes, so
// the process waits for the supervisor to unwind. CTRL_CLOSE grants ~5s.
const shutdownGrace = 5 * time.Second

func main() {
	// The Fyne GUI is the default. It needs CGO, so a -tags nogui build drops it;
	// in that build, or with the -nogui flag, the text reporter consumes the same
	// event stream.
	noGUI := flag.Bool("nogui", false, "print periodic text reports instead of the GUI")
	probeOnly := flag.Bool("probe", false, "dump raw per-thread identity fields for the running game and exit")
	listSettings := flag.String("settings", "", "print the tuning reference for an aggression preset (conservative, standard, aggressive) and exit")
	flag.Parse()

	if *listSettings != "" {
		printSettings(*listSettings)
		return
	}

	// A first run has no config to read, so write one rather than failing on a
	// missing file. It is a placeholder game with every tuning key at its
	// default: nothing is tuned until the name is changed, and the file doubles
	// as the reference for what there is to change.
	if created, err := config.EnsureFile(); err != nil {
		fmt.Println("config:", err)
		os.Exit(1)
	} else if created {
		fmt.Printf("wrote a starter %s — set the game executable in it, then run again\n", config.Path())
	}

	configuration, problems, err := config.Load()
	if err != nil {
		fmt.Printf("config: %v\n", err)
		os.Exit(1)
	}
	for _, problem := range problems {
		fmt.Println("config:", problem)
	}

	// Go maps CTRL_C / CTRL_CLOSE / LOGOFF / SHUTDOWN to these signals; the
	// governor's deferred revert runs within the ~5s close grace window.
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A diagnostic, not a mode: it attaches nothing, tunes nothing and reverts
	// nothing. See probe().
	if *probeOnly {
		if err := probe(rootCtx, configuration); err != nil {
			fmt.Println("probe:", err)
			os.Exit(1)
		}
		return
	}

	useGUI := !*noGUI && ui.Available()
	if !*noGUI && !ui.Available() {
		fmt.Println("GUI not compiled in (this is a -tags nogui build); using text report")
	}

	// The supervisor owns the whole lifecycle — scan, attach, run, rescan — and
	// narrates it as events. Both front ends consume the same stream, which is
	// why "waiting for a game" is no longer a console line the GUI can't show.
	events := make(chan ui.Event, 4)
	supervised := make(chan struct{})
	go func() {
		defer close(supervised)
		supervise(rootCtx, configuration, events)
	}()

	if !useGUI {
		reportEvents(rootCtx, events)
		return
	}

	if err := ui.Run(ui.Feed{
		Events:   events,
		Watching: watchedExecutables(configuration),
		OnQuit:   stop,
	}); err != nil {
		fmt.Println("ui:", err)
	}

	// Closing the window cancelled rootCtx; the governor still has to put every
	// tuned thread back before the process exits.
	select {
	case <-supervised:
	case <-time.After(shutdownGrace):
		fmt.Println("timed out waiting for threads to be reverted")
	}
}

// supervise runs the scan → attach → run → rescan cycle until ctx is
// cancelled, publishing one event per transition. It closes events on the way
// out so the consumer knows no more sessions are coming.
func supervise(ctx context.Context, configuration config.Config, events chan<- ui.Event) {
	defer close(events)

	for ctx.Err() == nil {
		if !send(ctx, events, ui.Event{Status: "Waiting for a game…"}) {
			return
		}

		game, gameProcess, err := scanner.Process(ctx, configuration)
		if err != nil {
			if ctx.Err() == nil {
				send(ctx, events, ui.Event{Status: "Scan failed: " + err.Error(), Fatal: true})
			}
			return
		}

		name := gameProcess.Executable()
		pid := uint32(gameProcess.Pid())

		if game.Auto == nil {
			send(ctx, events, ui.Event{
				Status: fmt.Sprintf("%s has no \"auto\" section configured; nothing for the governor to do.", name),
				Fatal:  true,
			})
			return
		}

		g := governor.New(name, game, pid)
		g.OnSave(tuningSaver(configuration, name))
		if !send(ctx, events, ui.Event{Session: g}) {
			return
		}

		if err := g.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			if !send(ctx, events, ui.Event{Status: "Governor stopped: " + err.Error()}) {
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(rescanDelay):
		}
	}
}

// send publishes an event unless ctx is cancelled first. It reports whether the
// event was delivered, so the caller can stop rather than push into a stream
// nobody is reading.
func send(ctx context.Context, events chan<- ui.Event, event ui.Event) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// reportEvents is the -nogui consumer: it prints status transitions and runs
// the text reporter for the duration of each session.
func reportEvents(ctx context.Context, events <-chan ui.Event) {
	// stopReporter always refers to the currently running reporter, so any exit
	// path — a new event, or the stream closing — tears it down exactly once.
	stopReporter := func() {}
	defer func() { stopReporter() }()

	for event := range events {
		stopReporter()
		stopReporter = func() {}

		if event.Session != nil {
			reportCtx, cancel := context.WithCancel(ctx)
			stopReporter = cancel
			go governor.Report(reportCtx, event.Session)
			continue
		}

		if event.Status != "" {
			fmt.Println(event.Status)
		}
	}
}

// tuningSaver writes an edited tuning table back into config.json under the
// game's own auto section, leaving every other game and every manual rule as it
// was. The config map is shared with the supervisor loop, so a saved table is
// also what the next session starts from.
func tuningSaver(configuration config.Config, name string) func(config.Tuning) error {
	return func(tuning config.Tuning) error {
		game, ok := configuration.Games[name]
		if !ok || game.Auto == nil {
			return fmt.Errorf("%s has no auto section in %s to save into", name, config.Path())
		}

		auto := *game.Auto
		auto.Tuning = tuning
		game.Auto = &auto
		configuration.Games[name] = game

		return config.Save(configuration)
	}
}

// printSettings dumps the tuning reference for one preset: every key, what it
// defaults to, and the sentence explaining it. Same registry the settings panel
// renders, so the two cannot drift apart.
func printSettings(aggression string) {
	switch aggression {
	case config.AggressionConservative, config.AggressionStandard, config.AggressionAggressive:
	default:
		fmt.Printf("unknown aggression %q; showing the %s preset\n\n", aggression, config.AggressionStandard)
		aggression = config.AggressionStandard
	}

	live := config.DefaultTuning(aggression)
	defaults := config.DefaultTuning(aggression)
	settings := config.Settings(&live, &defaults)

	fmt.Printf("Tuning reference for the %q preset.\n", aggression)
	fmt.Printf("Set any of these under games.<exe>.auto.tuning in %s, or edit them in the app's Settings tab.\n",
		config.Path())

	for _, group := range config.Groups(settings) {
		fmt.Printf("\n%s\n", group)
		for i := range settings {
			setting := &settings[i]
			if setting.Group != group {
				continue
			}

			fmt.Printf("  %-38s %s\n", setting.Path, setting.Default())
			if len(setting.Choices) > 0 {
				fmt.Printf("  %-38s one of: %s\n", "", strings.Join(setting.Choices, ", "))
			}
			for _, line := range wrap(setting.Desc, 74) {
				fmt.Printf("  %-38s %s\n", "", line)
			}
		}
	}
}

// wrap breaks a description into lines no longer than width, on word
// boundaries. A long word is left over-long rather than cut in half.
func wrap(text string, width int) []string {
	var lines []string
	var line string

	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}

	return lines
}

// watchedExecutables lists the configured game binaries, for the GUI to show
// while it waits.
func watchedExecutables(configuration config.Config) []string {
	names := make([]string, 0, len(configuration.Games))
	for name := range configuration.Games {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
