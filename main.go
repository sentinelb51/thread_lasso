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
	"syscall"
)

func main() {
	// The Fyne GUI is the default. It needs CGO, so a -tags nogui build drops it;
	// in that build, or with the -nogui flag, the text reporter consumes the same
	// view-model stream.
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

	for rootCtx.Err() == nil {
		fmt.Println("Waiting for a game...")
		game, gameProcess, err := scanner.Process(configuration)
		if err != nil {
			panic(err)
		}

		name := gameProcess.Executable()
		pid := uint32(gameProcess.Pid())
		fmt.Printf("Found game: %s (pid %d)\n", name, pid)

		if game.Auto == nil {
			fmt.Printf("%s has no \"auto\" section configured; nothing for the governor to do.\n", name)
			return
		}

		if err := runGovernor(rootCtx, name, game, pid, useGUI); err != nil {
			fmt.Println("governor stopped:", err)
		}

		// Fyne's GL loop can only run once per process, and closing the window is
		// the user's quit signal — re-scanning would spin up a second app and
		// crash. Text mode is free to loop and re-attach when the game restarts.
		if useGUI {
			break
		}
	}
}

// runGovernor drives one game session: it starts the view-model consumer and
// runs the governor loop until the game exits or the process is interrupted.
// useGUI is decided once by the caller (flag + build tag); re-deriving it here
// is what silently forced every session into the text reporter.
func runGovernor(ctx context.Context, name string, game config.Game, pid uint32, useGUI bool) error {
	g := governor.New(name, game, pid)

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !useGUI {
		go governor.Report(sessionCtx, g)
		err := g.Run(sessionCtx)
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}

	// The Fyne loop must own the main goroutine, so the governor runs alongside
	// it; closing the window cancels the session and unblocks the loop.
	go func() {
		g.Run(sessionCtx)
		cancel()
	}()
	return ui.Run(g)
}
