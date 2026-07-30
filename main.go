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
	flag.Parse()

	configuration, err := config.Load()
	if err != nil {
		panic(err)
	}

	// Go maps CTRL_C / CTRL_CLOSE / LOGOFF / SHUTDOWN to these signals; the
	// governor's deferred revert runs within the ~5s close grace window.
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
