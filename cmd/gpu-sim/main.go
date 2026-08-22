// Command gpu-sim builds simulated GPU clusters and runs scheduling scenarios against them.
//
//	gpu-sim topology apply  -f topologies/two-racks-h100.yaml
//	gpu-sim topology render -f topologies/two-racks-h100.yaml
//	gpu-sim run scenarios/
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
	"sigs.k8s.io/yaml"

	"github.com/MiguelGarrido02/gpu-sim/internal/cluster"
	"github.com/MiguelGarrido02/gpu-sim/internal/generate"
	"github.com/MiguelGarrido02/gpu-sim/internal/runner"
	"github.com/MiguelGarrido02/gpu-sim/internal/scenario"
	"github.com/MiguelGarrido02/gpu-sim/internal/topology"
)

const usage = `gpu-sim builds simulated GPU clusters and tests scheduling against them.

Usage:
  gpu-sim topology apply  -f <topology.yaml>   make the cluster match a topology
  gpu-sim topology render -f <topology.yaml>   print the objects without applying them
  gpu-sim run <scenario.yaml | directory>      run one scenario or a whole suite
  gpu-sim fragmentation                        report MIG capacity that is free but unreachable

Flags:
      --namespace    namespace holding the GPU profiles (default "gpu-operator")
      --kubeconfig   path to a kubeconfig (default: standard resolution)
      --json <file>  also write results as JSON, for CI (run only)
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		if !errors.Is(err, errFailed) {
			fmt.Fprintf(os.Stderr, "gpu-sim: %v\n", err)
		}
		os.Exit(1)
	}
}

// errFailed means the work ran and reported failures, which have already been printed.
var errFailed = errors.New("scenarios failed")

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return errors.New("no command given")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "topology":
		return topologyCmd(ctx, args[1:])
	case "run":
		return runCmd(ctx, args[1:])
	case "fragmentation":
		return fragmentationCmd(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type commonFlags struct {
	file       string
	namespace  string
	kubeconfig string
	jsonPath   string
}

func (c *commonFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.file, "f", "", "topology file")
	fs.StringVar(&c.file, "file", "", "topology file")
	fs.StringVar(&c.namespace, "namespace", "gpu-operator", "namespace holding the GPU profiles")
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "path to a kubeconfig")
	fs.StringVar(&c.jsonPath, "json", "", "write results as JSON to this file")
}

func topologyCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return errors.New("topology needs a subcommand: apply or render")
	}

	action := args[0]
	var flags commonFlags
	fs := flag.NewFlagSet("topology "+action, flag.ContinueOnError)
	flags.bind(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if flags.file == "" {
		fmt.Print(usage)
		return errors.New("no topology file given")
	}

	client, err := cluster.New(flags.kubeconfig, flags.namespace)
	if err != nil {
		return err
	}

	switch action {
	case "apply":
		res, err := client.ApplyTopology(ctx, flags.file)
		if err != nil {
			return err
		}
		fmt.Printf("applied %d nodes\n", res.Nodes)
		fmt.Printf("applied %d ResourceSlices covering %d devices\n", res.Slices, res.Devices)
		fmt.Printf("applied scheduler topology %q\n", res.Name)
		if len(res.Removed) > 0 {
			fmt.Printf("removed %d objects no longer in the topology:\n", len(res.Removed))
			for _, name := range res.Removed {
				fmt.Printf("  %s\n", name)
			}
		}
		return nil

	case "render":
		return renderTopology(ctx, client, flags.file)

	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown topology subcommand %q", action)
	}
}

func renderTopology(ctx context.Context, client *cluster.Client, path string) error {
	ct, err := topology.Load(path)
	if err != nil {
		return err
	}
	// Profiles are read from the cluster even when only rendering: they are the source of
	// the PCIe and NUMA attributes, and rendering invented ones would defeat the point of
	// rendering as a preview of what apply will do.
	resolved, err := ct.Resolve(client.LoadProfile(ctx))
	if err != nil {
		return err
	}

	objects := []any{}
	for _, node := range generate.Nodes(resolved) {
		objects = append(objects, node)
	}
	for _, slice := range generate.ResourceSlices(resolved) {
		objects = append(objects, slice)
	}
	objects = append(objects, generate.KAITopologyFor(resolved))

	for _, obj := range objects {
		out, err := yaml.Marshal(obj)
		if err != nil {
			return fmt.Errorf("serialising output: %w", err)
		}
		fmt.Printf("---\n%s", out)
	}
	return nil
}

func runCmd(ctx context.Context, args []string) error {
	// Go's flag package stops parsing at the first positional argument, which would make
	// `run scenarios/ --json out.json` treat the flag as a file name. Separating them
	// first lets flags appear on either side of the paths, as people expect.
	flagArgs, positional := splitFlags(args)

	var flags commonFlags
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.bind(fs)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(positional) == 0 {
		fmt.Print(usage)
		return errors.New("no scenario file or directory given")
	}

	paths, err := scenarioPaths(positional)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return errors.New("no scenarios found")
	}

	client, err := cluster.New(flags.kubeconfig, flags.namespace)
	if err != nil {
		return err
	}

	report := runner.Report{
		Out:    os.Stdout,
		Colour: term.IsTerminal(int(os.Stdout.Fd())),
	}

	r := runner.New(client)
	r.Progress = func(line string) { fmt.Printf("    %s\n", line) }

	start := time.Now()
	results := make([]*runner.Result, 0, len(paths))
	for _, path := range paths {
		s, err := scenario.Load(path)
		if err != nil {
			return err
		}
		res := r.Run(ctx, s)
		report.WriteResult(res)
		results = append(results, res)

		if ctx.Err() != nil {
			break
		}
	}
	report.WriteSummary(results, time.Since(start))

	if flags.jsonPath != "" {
		f, err := os.Create(flags.jsonPath)
		if err != nil {
			return fmt.Errorf("creating JSON report: %w", err)
		}
		defer f.Close()
		if err := report.WriteJSON(f, results); err != nil {
			return err
		}
	}

	for _, res := range results {
		if !res.Passed || res.Error != "" {
			return errFailed
		}
	}
	return nil
}

// scenarioPaths expands directories into the scenario files they contain, sorted so a suite
// runs in a stable order.
func scenarioPaths(args []string) ([]string, error) {
	var paths []string
	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			paths = append(paths, arg)
			continue
		}
		entries, err := os.ReadDir(arg)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !isYAML(name) {
				continue
			}
			paths = append(paths, filepath.Join(arg, name))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// splitFlags separates flag arguments from positional ones.
//
// Every flag gpu-sim defines takes a value, so a bare `-name value` pair consumes the next
// argument unless the value was given inline as `-name=value`.
func splitFlags(args []string) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		if !strings.Contains(arg, "=") && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, positional
}

// fragmentationCmd reports, per GPU, how much capacity is free but unreachable.
//
// Fragmentation is always relative to a profile: a GPU can be perfectly usable for small
// partitions and useless for large ones at the same instant, so the table is per profile
// and the headline is the largest partition still allocatable.
func fragmentationCmd(ctx context.Context, args []string) error {
	var flags commonFlags
	fs := flag.NewFlagSet("fragmentation", flag.ContinueOnError)
	flags.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, err := cluster.New(flags.kubeconfig, flags.namespace)
	if err != nil {
		return err
	}

	nodes, err := client.Fragmentation(ctx)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return errors.New("no MIG-enabled nodes found; apply a topology with mig.enabled first")
	}

	if flags.jsonPath != "" {
		f, err := os.Create(flags.jsonPath)
		if err != nil {
			return fmt.Errorf("creating JSON report: %w", err)
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		if err := enc.Encode(nodes); err != nil {
			return err
		}
	}

	total := 0
	for _, node := range nodes {
		fmt.Printf("\n%s\n", node.Node)
		for _, gpu := range node.GPUs {
			largest := gpu.LargestAllocatable
			if largest == "" {
				largest = "nothing"
			}
			fmt.Printf("  GPU %d   %d memory slices free, %d SM slices free   largest allocatable: %s\n",
				gpu.GPUIndex, gpu.FreeMemorySlices, gpu.FreeSMSlices, largest)

			for _, fit := range gpu.Profiles {
				if fit.Lost == 0 {
					continue
				}
				fmt.Printf("      %-10s could hold %d, holds %d  -> %d lost to fragmentation\n",
					fit.Profile, fit.Ideal, fit.Actual, fit.Lost)
			}
		}
		fmt.Printf("  %d partitions lost to fragmentation on this node\n", node.TotalLost())
		total += node.TotalLost()
	}
	fmt.Printf("\n%d partitions lost to fragmentation in total\n\n", total)

	return nil
}
