// Command topology-gen turns a declarative cluster topology into the Kubernetes objects a
// scheduler reads: simulated nodes with topology labels, DRA ResourceSlices carrying
// per-GPU NVLink, PCIe and NUMA attributes, and the scheduler's own topology object.
//
// Usage:
//
//	topology-gen render -f topologies/two-racks-h100.yaml
//	topology-gen apply  -f topologies/two-racks-h100.yaml
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"

	"github.com/MiguelGarrido02/gpu-sim/internal/cluster"
	"github.com/MiguelGarrido02/gpu-sim/internal/generate"
	"github.com/MiguelGarrido02/gpu-sim/internal/topology"
)

const usage = `topology-gen turns a cluster topology into simulated Kubernetes objects.

Commands:
  render   print the generated objects without touching the cluster
  apply    create or update the objects in the cluster

Flags:
  -f, --file        topology file (required)
      --namespace   namespace holding the GPU profiles (default "gpu-operator")
      --kubeconfig  path to a kubeconfig (default: standard resolution)
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "topology-gen: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return errors.New("no command given")
	}

	command := args[0]

	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	var file, namespace, kubeconfig string
	fs.StringVar(&file, "f", "", "topology file")
	fs.StringVar(&file, "file", "", "topology file")
	fs.StringVar(&namespace, "namespace", "gpu-operator", "namespace holding the GPU profiles")
	fs.StringVar(&kubeconfig, "kubeconfig", "", "path to a kubeconfig")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	if file == "" {
		fmt.Print(usage)
		return errors.New("no topology file given")
	}

	ct, err := topology.Load(file)
	if err != nil {
		return err
	}

	// Profiles are read from the cluster even when only rendering: they are the source of
	// the PCIe and NUMA attributes, and rendering them from invented data would defeat
	// the point of rendering as a preview of what apply will do.
	client, err := cluster.New(kubeconfig, namespace)
	if err != nil {
		return err
	}

	ctx := context.Background()
	resolved, err := ct.Resolve(client.LoadProfile(ctx))
	if err != nil {
		return err
	}

	switch command {
	case "render":
		return render(resolved)
	case "apply":
		return apply(ctx, client, resolved)
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", command)
	}
}

func render(resolved *topology.Resolved) error {
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

func apply(ctx context.Context, client *cluster.Client, resolved *topology.Resolved) error {
	nodes := generate.Nodes(resolved)
	for _, node := range nodes {
		if err := client.ApplyNode(ctx, node); err != nil {
			return err
		}
	}
	fmt.Printf("applied %d nodes\n", len(nodes))

	// ResourceSlices come second: a slice names its node, and publishing one for a node
	// that does not exist yet leaves the scheduler briefly seeing GPUs nowhere.
	slices := generate.ResourceSlices(resolved)
	gpus := 0
	for _, slice := range slices {
		if err := client.ApplyResourceSlice(ctx, slice); err != nil {
			return err
		}
		gpus += len(slice.Spec.Devices)
	}
	fmt.Printf("applied %d ResourceSlices covering %d GPUs\n", len(slices), gpus)

	kaiTopology := generate.KAITopologyFor(resolved)
	if err := client.ApplyKAITopology(ctx, kaiTopology); err != nil {
		return err
	}
	fmt.Printf("applied Topology %q with %d levels\n", kaiTopology.Metadata.Name, len(kaiTopology.Spec.Levels))

	return nil
}
