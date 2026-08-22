// Package cluster wraps the Kubernetes access topology-gen needs: reading GPU profiles,
// and creating the nodes, ResourceSlices and scheduler topology it generates.
package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"

	"github.com/MiguelGarrido02/gpu-sim/internal/generate"
	"github.com/MiguelGarrido02/gpu-sim/internal/profile"
)

// profileConfigMapPrefix is how fake-gpu-operator names the ConfigMap holding each GPU
// profile, and profileKey is the key inside it.
const (
	profileConfigMapPrefix = "gpu-profile-"
	profileKey             = "profile.yaml"
)

var kaiTopologyGVR = schema.GroupVersionResource{
	Group:    "kai.scheduler",
	Version:  "v1alpha1",
	Resource: "topologies",
}

type Client struct {
	kube      kubernetes.Interface
	dynamic   dynamic.Interface
	namespace string
}

// New connects using the standard kubeconfig resolution order. namespace is where
// fake-gpu-operator's GPU profile ConfigMaps live.
func New(kubeconfig, namespace string) (*Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("building Kubernetes client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("building dynamic client: %w", err)
	}

	return &Client{kube: kube, dynamic: dyn, namespace: namespace}, nil
}

// LoadProfile reads a GPU profile published by fake-gpu-operator.
func (c *Client) LoadProfile(ctx context.Context) func(string) (*profile.Profile, error) {
	return func(name string) (*profile.Profile, error) {
		cm, err := c.kube.CoreV1().ConfigMaps(c.namespace).Get(
			ctx, profileConfigMapPrefix+name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf(
					"no GPU profile %q in namespace %s: fake-gpu-operator must be installed with builtinProfiles enabled",
					name, c.namespace)
			}
			return nil, err
		}

		data, ok := cm.Data[profileKey]
		if !ok {
			return nil, fmt.Errorf("ConfigMap %s has no %s key", cm.Name, profileKey)
		}
		return profile.Parse([]byte(data))
	}
}

// ApplyNode creates the node, or updates an existing one in place.
//
// Capacity lives in the node's status, which is a subresource, so it needs a second call
// the API server would otherwise silently drop.
//
// Both writes retry on conflict. Simulated nodes are not quiet objects: the KWOK controller
// writes heartbeats and conditions, and fake-gpu-operator's status-exporter writes GPU
// labels, so a plain read-modify-write loses the race often enough to fail a normal run.
func (c *Client) ApplyNode(ctx context.Context, node *corev1.Node) error {
	nodes := c.kube.CoreV1().Nodes()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := nodes.Get(ctx, node.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = nodes.Create(ctx, node, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}

		// Labels and taints are replaced wholesale: the topology file is the source of
		// truth, so a label dropped from it must disappear from the node rather than
		// linger and keep matching selectors.
		existing.Labels = node.Labels
		existing.Annotations = node.Annotations
		existing.Spec.Taints = node.Spec.Taints
		_, err = nodes.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("applying node %s: %w", node.Name, err)
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := nodes.Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		existing.Status.Capacity = node.Status.Capacity
		existing.Status.Allocatable = node.Status.Allocatable
		existing.Status.NodeInfo = node.Status.NodeInfo
		_, err = nodes.UpdateStatus(ctx, existing, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("setting status of node %s: %w", node.Name, err)
	}
	return nil
}

// ApplyResourceSlice creates the slice, or replaces the spec of an existing one. An
// existing slice is usually one fake-gpu-operator's plugin published before it was
// disabled; adopting it by name avoids leaving two slices describing the same node.
func (c *Client) ApplyResourceSlice(ctx context.Context, slice *resourceapi.ResourceSlice) error {
	slices := c.kube.ResourceV1().ResourceSlices()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := slices.Get(ctx, slice.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = slices.Create(ctx, slice, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}

		existing.Spec = slice.Spec
		existing.Labels = slice.Labels
		_, err = slices.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("applying ResourceSlice %s: %w", slice.Name, err)
	}
	return nil
}

// Prune deletes the simulated nodes and ResourceSlices gpu-sim created that the current
// topology no longer describes, and reports what it removed.
//
// Switching between topology files without this leaves the cluster describing two machines
// at once — the old nodes keep their labels and their GPUs, and a scheduler happily places
// work on hardware the topology file says does not exist.
//
// Only objects carrying gpu-sim's managed-by label are considered, so a real node in a
// mixed cluster, or a simulated node someone created by hand, is never touched.
func (c *Client) Prune(ctx context.Context, keepNodes map[string]bool) ([]string, error) {
	var removed []string

	slices, err := c.kube.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{
		LabelSelector: generate.ManagedSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing managed ResourceSlices: %w", err)
	}
	// Slices go first: deleting a node while its slice still advertises GPUs would
	// briefly leave the scheduler seeing devices on a node that no longer exists.
	for _, slice := range slices.Items {
		if slice.Spec.NodeName != nil && keepNodes[*slice.Spec.NodeName] {
			continue
		}
		if err := c.kube.ResourceV1().ResourceSlices().Delete(ctx, slice.Name, metav1.DeleteOptions{}); err != nil {
			return removed, fmt.Errorf("deleting ResourceSlice %s: %w", slice.Name, err)
		}
		removed = append(removed, "resourceslice/"+slice.Name)
	}

	nodes, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: generate.ManagedSelector,
	})
	if err != nil {
		return removed, fmt.Errorf("listing managed nodes: %w", err)
	}
	for _, node := range nodes.Items {
		if keepNodes[node.Name] {
			continue
		}
		if err := c.kube.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{}); err != nil {
			return removed, fmt.Errorf("deleting node %s: %w", node.Name, err)
		}
		removed = append(removed, "node/"+node.Name)
	}

	return removed, nil
}

// ApplyKAITopology creates or updates the scheduler's topology object.
func (c *Client) ApplyKAITopology(ctx context.Context, topo *generate.KAITopology) error {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(topo)
	if err != nil {
		return fmt.Errorf("converting topology: %w", err)
	}
	obj := &unstructured.Unstructured{Object: raw}

	topologies := c.dynamic.Resource(kaiTopologyGVR)

	existing, err := topologies.Get(ctx, topo.Metadata.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := topologies.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating Topology %s: %w", topo.Metadata.Name, err)
		}
		return nil
	}
	if err != nil {
		// The CRD is absent unless KAI is installed. Nodes and ResourceSlices are
		// useful without it, so report the reason rather than failing the whole run.
		return fmt.Errorf("reading Topology %s (is KAI Scheduler installed?): %w", topo.Metadata.Name, err)
	}

	obj.SetResourceVersion(existing.GetResourceVersion())
	if _, err := topologies.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating Topology %s: %w", topo.Metadata.Name, err)
	}
	return nil
}
