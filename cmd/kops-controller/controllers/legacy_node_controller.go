/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
	api "k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/apis/kops/registry"
	"k8s.io/kops/pkg/kopscodecs"
	"k8s.io/kops/pkg/nodeidentity"
	"k8s.io/kops/pkg/nodelabels"
	"k8s.io/kops/util/pkg/vfs"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// NewLegacyNodeReconciler is the constructor for a LegacyNodeReconciler
func NewLegacyNodeReconciler(mgr manager.Manager, vfsContext *vfs.VFSContext, configPath string, identifier nodeidentity.LegacyIdentifier) (*LegacyNodeReconciler, error) {
	r := &LegacyNodeReconciler{
		client:     mgr.GetClient(),
		log:        ctrl.Log.WithName("controllers").WithName("Node"),
		identifier: identifier,
		cache:      vfs.NewCache(),
	}

	coreClient, err := corev1client.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("error building corev1 client: %v", err)
	}
	r.coreV1Client = coreClient

	configBase, err := vfsContext.BuildVfsPath(configPath)
	if err != nil {
		return nil, fmt.Errorf("cannot parse ConfigBase %q: %v", configPath, err)
	}
	r.configBase = configBase

	return r, nil
}

// LegacyNodeReconciler observes Node objects, and labels them with the correct labels for the instancegroup
// This used to be done by the kubelet, but is moving to a central controller for greater security in 1.16
type LegacyNodeReconciler struct {
	// client is the controller-runtime client
	client client.Client

	// log is a logr
	log logr.Logger

	// coreV1Client is a client-go client for patching nodes
	coreV1Client *corev1client.CoreV1Client

	// identifier is a provider that can securely map node ProviderIDs to InstanceGroups
	identifier nodeidentity.LegacyIdentifier

	// configBase is the parsed path to the base location of our configuration files
	configBase vfs.Path

	// cache caches the instancegroup and cluster values, to avoid repeated GCS/S3 calls
	cache *vfs.Cache
}

// +kubebuilder:rbac:groups=,resources=nodes,verbs=get;list;watch;patch
// Reconcile is the main reconciler function that observes node changes.
func (r *LegacyNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = r.log.WithValues("nodecontroller", req.NamespacedName)

	node := &corev1.Node{}
	if err := r.client.Get(ctx, req.NamespacedName, node); err != nil {
		klog.Warningf("unable to fetch node %s: %v", node.Name, err)
		if apierrors.IsNotFound(err) {
			// we'll ignore not-found errors, since they can't be fixed by an immediate
			// requeue (we'll need to wait for a new notification), and we can get them
			// on deleted requests.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	cluster, err := r.getClusterForNode(node)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to load cluster object for node %s: %v", node.Name, err)
	}

	ig, err := r.getInstanceGroupForNode(ctx, node)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to load instance group object for node %s: %v", node.Name, err)
	}

	labels, err := nodelabels.BuildNodeLabels(cluster, ig)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error building node labels for node %q: %w", node.Name, err)
	}

	lifecycle, err := r.getInstanceLifecycle(ctx, node)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to get instance lifecycle %s: %v", node.Name, err)
	}

	if len(lifecycle) > 0 {
		labels[fmt.Sprintf("node-role.kubernetes.io/%s-worker", lifecycle)] = "true"
	}

	updateLabels := make(map[string]string)
	for k, v := range labels {
		actual, found := node.Labels[k]
		if !found || actual != v {
			updateLabels[k] = v
		}
	}

	deleteLabels := make(map[string]struct{})
	for k := range node.Labels {
		// If it is one of our managed labels, "prune" values we don't want to be there
		switch k {
		case nodelabels.RoleLabelMaster16, nodelabels.RoleLabelAPIServer16, nodelabels.RoleLabelNode16, nodelabels.RoleLabelControlPlane20:
			if _, found := labels[k]; !found {
				deleteLabels[k] = struct{}{}
			}
		}
	}

	if len(updateLabels) == 0 && len(deleteLabels) == 0 {
		klog.V(4).Infof("no label changes needed for %s", node.Name)
		return ctrl.Result{}, nil
	}

	if err := patchNodeLabels(r.coreV1Client, ctx, node, updateLabels, deleteLabels); err != nil {
		klog.Warningf("failed to patch node labels on %s: %v", node.Name, err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *LegacyNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Complete(r)
}

// getClusterForNode returns the api.Cluster object for the node
// The cluster is actually loaded when we first start
func (r *LegacyNodeReconciler) getClusterForNode(node *corev1.Node) (*api.Cluster, error) {
	clusterPath := r.configBase.Join(registry.PathClusterCompleted)
	cluster, err := r.loadCluster(clusterPath)
	if err != nil {
		return nil, err
	}
	return cluster, nil
}

// getInstanceLifecycle returns InstanceLifecycle string object
func (r *LegacyNodeReconciler) getInstanceLifecycle(ctx context.Context, node *corev1.Node) (string, error) {
	identity, err := r.identifier.IdentifyNode(ctx, node)
	if err != nil {
		return "", fmt.Errorf("error identifying node %q: %v", node.Name, err)
	}

	return identity.InstanceLifecycle, nil
}

// getInstanceGroupForNode returns the api.InstanceGroup object for the node
func (r *LegacyNodeReconciler) getInstanceGroupForNode(ctx context.Context, node *corev1.Node) (*api.InstanceGroup, error) {
	// We assume that if the instancegroup label is set, that it is correct
	// TODO: Should we be paranoid?
	instanceGroupName := node.Labels["kops.k8s.io/instancegroup"]

	if instanceGroupName == "" {
		providerID := node.Spec.ProviderID
		if providerID == "" {
			return nil, fmt.Errorf("node providerID not set for node %q", node.Name)
		}

		identity, err := r.identifier.IdentifyNode(ctx, node)
		if err != nil {
			return nil, fmt.Errorf("error identifying node %q: %v", node.Name, err)
		}

		if identity.InstanceGroup == "" {
			return nil, fmt.Errorf("node %q did not have an associate instance group", node.Name)
		}
		instanceGroupName = identity.InstanceGroup
	}

	return r.loadNamedInstanceGroup(instanceGroupName)
}

// loadCluster loads a api.Cluster object from a vfs.Path
func (r *LegacyNodeReconciler) loadCluster(p vfs.Path) (*api.Cluster, error) {
	ttl := time.Hour

	b, err := r.cache.Read(p, ttl)
	if err != nil {
		return nil, fmt.Errorf("error loading Cluster %q: %v", p, err)
	}

	o, _, err := kopscodecs.Decode(b, nil)
	if err != nil {
		return nil, fmt.Errorf("error parsing Cluster %q: %v", p, err)
	}
	if cluster, ok := o.(*api.Cluster); ok {
		return cluster, nil
	}
	return nil, fmt.Errorf("unexpected object type for Cluster %q: %T", p, o)
}

// loadNamedInstanceGroup loads a api.InstanceGroup object from the vfs backing store
func (r *LegacyNodeReconciler) loadNamedInstanceGroup(name string) (*api.InstanceGroup, error) {
	p := r.configBase.Join("instancegroup", name)

	ttl := time.Hour
	b, err := r.cache.Read(p, ttl)
	if err != nil {
		return nil, fmt.Errorf("error loading InstanceGroup %q: %v", p, err)
	}

	object, _, err := kopscodecs.Decode(b, nil)
	if err != nil {
		return nil, fmt.Errorf("error parsing %s: %w", p, err)
	}

	instanceGroup, ok := object.(*api.InstanceGroup)
	if !ok {
		return nil, fmt.Errorf("unexpected type, expected InstanceGroup, got %T", object)
	}

	return instanceGroup, nil
}
