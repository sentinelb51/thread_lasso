//go:build windows && amd64

package governor

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"
)

// reportTopRows caps how many threads the text report prints per tick; the
// rows arrive already sorted by cycle rate, so this is the hottest N.
const reportTopRows = 20

// Report is the -nogui consumer of a governor's view-model stream. It drains
// g.Views and prints a periodic table until ctx is cancelled. Run it
// concurrently with Governor.Run (they share the Views channel with the UI as
// mutually exclusive consumers).
func Report(ctx context.Context, g *Governor) {
	for {
		select {
		case <-ctx.Done():
			return
		case view := <-g.Views:
			printReport(view)
		}
	}
}

func printReport(view ViewModel) {
	focus := ""
	if !view.Focused {
		focus = " [unfocused — metrics paused]"
	}
	fmt.Printf("\n[%s] %s (pid %d) — phase=%s optim=%s aggression=%s mode=%s%s | %d threads, %.1fM cyc/s, %.1f MB/s read\n",
		view.At.Format(time.Kitchen), view.GameName, view.PID,
		view.Phase, view.Optimisation, view.Aggression, view.AccessMode, focus,
		view.ThreadCount, view.TotalCycles/1e6, view.ReadRate/1e6)

	for _, warning := range view.Warnings {
		fmt.Printf("  ! %s\n", warning)
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	// PRIO is dynamic/base: the pair is what distinguishes a thread the game
	// elevated from one Windows is briefly boosting, and from one we promoted.
	fmt.Fprintln(writer, "  TID\tTHREAD\tCYC/S\tSW/S\tPRIO\tWAIT\tROLE\tCONF\tBUCKET\tAPPLIED")

	for i, row := range view.Rows {
		if i >= reportTopRows {
			break
		}

		applied := row.Applied
		if applied == "" {
			applied = "-"
		}
		flags := ""
		if row.Starved {
			flags = " *starved"
		}

		fmt.Fprintf(writer, "  %d\t%s\t%.2fM\t%.0f\t%d/%d\t%s\t%s\t%.0f%%\t%s\t%s%s\n",
			row.TID, truncate(row.Identity(), 34),
			row.CyclesRate/1e6, row.SwitchRate, row.Priority, row.BasePriority,
			shortWait(row.WaitProfile), row.Role, row.Confidence*100,
			row.Bucket, applied, flags)
	}

	writer.Flush()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

func shortWait(profile string) string {
	if profile == "" {
		return "-"
	}
	return profile
}
