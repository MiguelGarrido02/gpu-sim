package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Colours are emitted only when writing to a terminal; a CI log or a redirected file gets
// plain text.
const (
	colReset = "\033[0m"
	colGreen = "\033[32m"
	colRed   = "\033[31m"
	colBlue  = "\033[1;34m"
	colDim   = "\033[2m"
)

// Report renders results for a human.
type Report struct {
	Out    io.Writer
	Colour bool
}

func (r Report) colour(code, s string) string {
	if !r.Colour {
		return s
	}
	return code + s + colReset
}

// WriteResult prints one scenario's outcome.
//
// A failing assertion prints the scheduler's own words. That is the difference between a
// report saying a test failed and one saying what to do about it: in Phase 1 a generic
// "not enough resources" on an idle cluster cost a day, and the specific per-domain message
// would have pointed straight at the cause.
func (r Report) WriteResult(res *Result) {
	fmt.Fprintf(r.Out, "\n%s %s\n", r.colour(colBlue, "==>"), res.Name)
	if res.Description != "" {
		fmt.Fprintf(r.Out, "    %s\n", r.colour(colDim, res.Description))
	}

	if res.Error != "" {
		fmt.Fprintf(r.Out, "  %s %s\n", r.colour(colRed, "ERROR"), res.Error)
		return
	}

	fmt.Fprintf(r.Out, "    %s\n", r.colour(colDim, fmt.Sprintf(
		"cluster %s · %d nodes · %d GPUs · scheduler %s",
		res.Cluster.Topology, res.Cluster.Nodes, res.Cluster.GPUs, res.Cluster.Scheduler)))

	for _, a := range res.Assertions {
		if a.Passed {
			fmt.Fprintf(r.Out, "  %s %s\n", r.colour(colGreen, "PASS"), a.Name)
			if a.Detail != "" {
				fmt.Fprintf(r.Out, "       %s\n", r.colour(colDim, a.Detail))
			}
			continue
		}

		fmt.Fprintf(r.Out, "  %s %s\n", r.colour(colRed, "FAIL"), a.Name)
		if a.Detail != "" {
			fmt.Fprintf(r.Out, "       %s\n", a.Detail)
		}
		if len(a.SchedulerSaid) > 0 {
			fmt.Fprintf(r.Out, "\n       %s\n", r.colour(colDim, "the scheduler said:"))
			for _, said := range a.SchedulerSaid {
				for _, line := range strings.Split(strings.TrimSpace(said), "\n") {
					fmt.Fprintf(r.Out, "         %s\n", strings.TrimSpace(line))
				}
			}
		}
		if len(a.Placement) > 0 {
			fmt.Fprintf(r.Out, "\n       %s %s\n\n",
				r.colour(colDim, "placement:"), describePlacement(a.Placement))
		}
	}
}

// WriteSummary prints the tally across a suite.
func (r Report) WriteSummary(results []*Result, total time.Duration) {
	passed, failed := 0, 0
	for _, res := range results {
		if res.Passed && res.Error == "" {
			passed++
		} else {
			failed++
		}
	}

	fmt.Fprintf(r.Out, "\n%s Result\n", r.colour(colBlue, "==>"))
	tally := fmt.Sprintf("  %d passed, %d failed", passed, failed)
	if failed > 0 {
		fmt.Fprintf(r.Out, "%s", r.colour(colRed, tally))
	} else {
		fmt.Fprintf(r.Out, "%s", r.colour(colGreen, tally))
	}
	fmt.Fprintf(r.Out, "  %s\n\n", r.colour(colDim, "in "+total.Round(time.Second).String()))
}

// WriteJSON emits machine-readable results for CI.
func (r Report) WriteJSON(w io.Writer, results []*Result) error {
	// Durations serialise as nanoseconds by default, which is not a useful unit in a CI
	// artefact; milliseconds are.
	type jsonResult struct {
		*Result
		DurationMs int64 `json:"durationMs"`
	}
	out := make([]jsonResult, 0, len(results))
	for _, res := range results {
		out = append(out, jsonResult{Result: res, DurationMs: res.Duration.Milliseconds()})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func describePlacement(placement map[string]int) string {
	nodes := make([]string, 0, len(placement))
	for node := range placement {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	parts := make([]string, 0, len(nodes))
	for _, node := range nodes {
		parts = append(parts, fmt.Sprintf("%s=%d", node, placement[node]))
	}
	return strings.Join(parts, "  ")
}
