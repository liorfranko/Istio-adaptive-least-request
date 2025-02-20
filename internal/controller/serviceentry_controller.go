/*
Copyright 2024.

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

package controller

import (
	"context"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	istioNetworkingV1 "istio.io/api/networking/v1"
	istioClientV1 "istio.io/client-go/pkg/apis/networking/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	optimizationv1alpha1 "istio-adaptive-least-request/api/v1alpha1"
	"istio-adaptive-least-request/internal/helpers"
	customMetrics "istio-adaptive-least-request/internal/metrics"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ServiceEntryReconciler reconciles a ServiceEntry object
type ServiceEntryReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	LoggerName string
	// Channel used to trigger reconciliation of ServiceEntry resources.
	ServiceEntryReconcileTriggerChannel <-chan event.GenericEvent
	ServiceEntryServiceNameLabelKey     *string
	NamespaceList                       []string
	NewEndpointsPercentileWeight        int
	MinimumWeight                       int
	MaximumWeight                       int

	//TODO: add dry run mode logic
	//DryRun bool
}

//+kubebuilder:rbac:groups=core,resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=serviceentries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=serviceentries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=serviceentries/finalizers,verbs=update
//+kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=istioadaptiverequestoptimizers,verbs=get;list;watch

func (r *ServiceEntryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	logger := log.FromContext(ctx).WithName(r.LoggerName)
	logger.V(1).Info("Reconcile ServiceEntry", "ServiceEntry.Namespace", req.Namespace, "ServiceEntry.Name", req.Name)

	// Step 1: Fetch the IstioAdaptiveRequestOptimizer object based on the request.
	var istioAdaptiveRequestOptimizer optimizationv1alpha1.IstioAdaptiveRequestOptimizer
	if err := r.Get(ctx, req.NamespacedName, &istioAdaptiveRequestOptimizer); err != nil {
		logger.Info("IstioAdaptiveRequestOptimizer not found. No weight adjustments made.")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Step 2: Check for Endpoint updates.
	oldWorkloads, err := r.handleEndpointUpdate(ctx, req, &istioAdaptiveRequestOptimizer)
	if client.IgnoreNotFound(err) != nil {
		logger.Error(err, "Failed to handle Endpoint update.")
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "handle_endpoint_update", "name": req.Name, "namespace": req.Namespace}).Inc()
		return ctrl.Result{}, err
	}

	// Step 3: Fetch the WeightOptimizer object based on the request.
	var opt optimizationv1alpha1.WeightOptimizer
	if err := r.Get(ctx, req.NamespacedName, &opt); err != nil {
		logger.Info("WeightOptimizer not found. No weight adjustments made.")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	cleanupPodMetrics(oldWorkloads, istioAdaptiveRequestOptimizer.Spec.ServiceNamespace, req.Name)
	// Step 4: Validate that the endpoints in the ServiceEntry match the current cluster state
	if err := r.validateAndUpdateWeights(ctx, req, &opt); err != nil {
		if errors.IsConflict(err) {
			logger.Info("Got update event for endpoint need to wait new weight update")
			// recue the request delay after 1 second
			//return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to validate or update weights.")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// handleEndpointUpdate checks for changes in the EndpointSlices and updates the ServiceEntry if necessary.
func (r *ServiceEntryReconciler) handleEndpointUpdate(ctx context.Context, req ctrl.Request, opt *optimizationv1alpha1.IstioAdaptiveRequestOptimizer) ([]*istioNetworkingV1.WorkloadEntry, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)

	// Fetch the specific ServiceEntry.
	var serviceEntry istioClientV1.ServiceEntry
	err := r.Get(ctx, req.NamespacedName, &serviceEntry)
	if client.IgnoreNotFound(err) != nil {
		logger.Error(err, "Failed to fetch ServiceEntry.")
		return nil, err
	}

	// Extract the original service name from the ServiceEntry's labels.
	originalServiceName := serviceEntry.Labels[*r.ServiceEntryServiceNameLabelKey]
	if originalServiceName == "" {
		logger.Error(nil, "ServiceEntry does not contain the expected label.", "Label", *r.ServiceEntryServiceNameLabelKey)
		return nil, err // or an error, as appropriate
	}

	// List all EndpointSlices for the given service in the same namespace using label selectors.
	var endpointSlices discoveryv1.EndpointSliceList
	labelSelector := client.MatchingLabels{
		"kubernetes.io/service-name": originalServiceName,
	}
	if err := r.List(ctx, &endpointSlices, client.InNamespace(req.Namespace), labelSelector); err != nil {
		logger.Error(err, "Failed to list EndpointSlices for service", "Namespace", req.Namespace, "Name", originalServiceName)
		return nil, err
	}

	if len(endpointSlices.Items) == 0 {
		logger.Info("No EndpointSlices found for service", "Namespace", req.Namespace, "Name", originalServiceName)
		return nil, err // No endpoints to process
	}

	// Track existing endpoint allWeights.
	weightsByAddress := make(map[string]uint32)
	var allWeights []uint32
	weightsByLocality := map[string][]uint32{}
	for _, ep := range serviceEntry.Spec.Endpoints {
		weightsByAddress[ep.Address] = ep.Weight
		allWeights = append(allWeights, ep.Weight)
		weightsByLocality[ep.Locality] = append(weightsByLocality[ep.Locality], ep.Weight)
	}

	// Calculate average weight for new endpoints.
	logger.Info("Calculating average weight for new endpoints", "ExistingWeights", weightsByAddress)

	averageWeightByLocality := make(map[string]uint32, len(weightsByLocality))

	for locality, localityWeights := range weightsByLocality {
		averageWeightByLocality[locality] = r.CreateDefaultWeightForNewEndpoints(localityWeights)
	}

	averageWeight := r.CreateDefaultWeightForNewEndpoints(allWeights)
	logger.Info("Calculated average weight for new endpoints", "AverageWeight", averageWeight)

	// Aggregate endpoints from all EndpointSlices, assigning average weight to new ones.
	mergedEndpoints := make([]*istioNetworkingV1.WorkloadEntry, 0)
	for _, slice := range endpointSlices.Items {
		for _, endpoint := range slice.Endpoints {
			// Prefer the IPv4 address; fallback to IPv6 if necessary

			if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready {
				logger.Info("Endpoint is not ready", "Endpoint", endpoint)
				continue
			}

			var ip string
			if len(endpoint.Addresses) > 0 {
				ip = endpoint.Addresses[0]
			}
			if ip == "" {
				continue // Skip endpoints without an IP address
			}

			var locality string

			if opt.Spec.LocalityEnabled {
				// Safely handle the *string for zone:
				if zonePtr := endpoint.Zone; zonePtr != nil {
					locality = *zonePtr
					locality = locality[:len(locality)-1] + "/" + locality
				}
			}

			weight, ok := weightsByAddress[ip]
			if !ok {
				logger.Info("New endpoint detected entering the cluster with default weight", "IP", ip, "Weight", averageWeight)
				if locality != "" {
					weight, ok = averageWeightByLocality[locality]
					if !ok {
						weight = averageWeight
					}
				} else {
					weight = averageWeight // Assign the calculated average weight if new.
				}
			}

			mergedEndpoints = append(mergedEndpoints, &istioNetworkingV1.WorkloadEntry{
				Address: ip,
				Weight:  weight,
				// Capture the zone/locality information for this endpoint.
				Locality: locality,
			})
		}
	}

	var coreService corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &coreService); err != nil {
		logger.Error(err, "Failed to fetch Service.")
		return nil, err
	}
	serviceEntry.Spec.Ports = coreServicePortsToIstioServicePorts(coreService.Spec.Ports, serviceEntry.Spec.Ports[:0])

	// Check if updates are required based on endpoint changes.
	//if !(addressesChanged(serviceEntry.Spec.Endpoints, mergedEndpoints) || checkPortsChanged(serviceEntry.Spec.Ports, service.Spec.ServicePorts)) {
	//	logger.Info("No changes detected", "ServiceEntry", serviceEntry.Name)
	//	return nil, err // No changes, no need to update.
	//}
	oldWorkloads := helpers.Diff(mergedEndpoints, serviceEntry.Spec.Endpoints)
	//logger.Info("Detected endpoint changes", "ServiceEntry", serviceEntry.Name)

	// Update the ServiceEntry with the newly merged endpoints.
	serviceEntry.Spec.Endpoints = mergedEndpoints
	if err := r.Update(ctx, &serviceEntry); err != nil {
		logger.Error(err, "Failed to update ServiceEntry", "ServiceEntry", serviceEntry.Name)
		return nil, err // Return the error to retry
	}
	logger.Info("ServiceEntry updated successfully with dynamic weighting", "ServiceEntry", serviceEntry.Name)
	return oldWorkloads, nil
}

func checkPortsChanged(istioPorts []*istioNetworkingV1.ServicePort, optPorts []optimizationv1alpha1.ServicePort) bool {
	if len(istioPorts) != len(optPorts) {
		return true
	}
	istioPortsMap := make(map[string]uint32)
	for _, port := range istioPorts {
		istioPortsMap[port.Name] = port.TargetPort
	}
	for i := range optPorts {
		port := &optPorts[i]
		if istioPortsMap[port.Protocol] != port.TargetPort {
			return true
		}
	}
	return false
}

// validateAndUpdateWeights checks if endpoints match the expected state and updates weights if necessary.
func (r *ServiceEntryReconciler) validateAndUpdateWeights(ctx context.Context, req ctrl.Request, optWeight *optimizationv1alpha1.WeightOptimizer) error {
	logger := log.FromContext(ctx).WithName(r.LoggerName)

	// Fetch the specific ServiceEntry.
	var serviceEntry istioClientV1.ServiceEntry
	if err := r.Get(ctx, req.NamespacedName, &serviceEntry); client.IgnoreNotFound(err) != nil {
		logger.Error(err, "Failed to fetch ServiceEntry.")
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "fetch_service_entry", "name": req.Name, "namespace": req.Namespace}).Inc()
		return err
	}

	// Create weightsMap to store the weights of the endpoints
	weightsMap := make(map[string]uint32)
	endpoints := optWeight.Spec.Endpoints
	for i := range endpoints {
		endpoint := &endpoints[i]
		weightsMap[endpoint.IP] = endpoint.Weight
	}
	for _, workloadEntry := range serviceEntry.Spec.Endpoints {
		weight, ok := weightsMap[workloadEntry.Address]
		if ok {
			workloadEntry.Weight = weight
		}
	}

	// Update the ServiceEntry with the new weights
	logger.Info("ServiceEntry updated with new weights", "ServiceEntry", serviceEntry.Name, "ServiceEntry.Spec.Endpoints", serviceEntry.Spec.Endpoints)
	if err := r.Update(ctx, &serviceEntry); err != nil {
		if errors.IsConflict(err) {
			logger.Info("ServiceEntry has been modified. Requeueing.")
			return err
		}
		logger.Error(err, "Failed to update ServiceEntry weights.")
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "validate_and_update_weights", "name": req.Name, "namespace": req.Namespace}).Inc()
		return err
	}
	// Update the metrics for the service
	updateMetrics(serviceEntry.Spec.Endpoints, serviceEntry.Namespace, serviceEntry.Name)

	return nil // Return true indicating weights were updated.
}

func updateMetrics(workloads []*istioNetworkingV1.WorkloadEntry, serviceEntryNamespace string, serviceEntryName string) {
	for _, workload := range workloads {
		podAddress := workload.Address
		podZone := ""
		if zonePtr := workload.Locality; zonePtr != "" {
			podZone = zonePtr
			podZone = getLocalityForMetric(podZone)
		}
		podWeight := workload.Weight
		customMetrics.WeightMetric.WithLabelValues(serviceEntryNamespace, serviceEntryName, podAddress, podZone).Set(float64(podWeight))
	}
}

func cleanupPodMetrics(oldWorkloads []*istioNetworkingV1.WorkloadEntry, serviceEntryNamespace string, serviceEntryName string) {
	for _, workload := range oldWorkloads {
		podAddress := workload.Address
		podZone := ""
		if zonePtr := workload.Locality; zonePtr != "" {
			podZone = zonePtr
			podZone = getLocalityForMetric(podZone)
		}
		helpers.CleanupPodMetrics(serviceEntryNamespace, serviceEntryName, podAddress, podZone)
	}
}

func getLocalityForMetric(locality string) string {
	if locality == "" {
		return "global"
	}
	parts := strings.Split(locality, "/")
	if len(parts) == 2 {
		return parts[1]
	}
	return locality
}

func (r *ServiceEntryReconciler) CreateDefaultWeightForNewEndpoints(weights []uint32) uint32 {
	if len(weights) == 0 {
		middle := (r.MaximumWeight + r.MinimumWeight) / 2
		return uint32(middle) // Return the middle value if no existing weights
	}

	// Sort the slice
	sort.Slice(weights, func(i, j int) bool {
		return weights[i] < weights[j]
	})

	// Calculate number of elements in the lowest newEndpointsPercentileWeight
	n := len(weights) * r.NewEndpointsPercentileWeight / 100
	if n == 0 {
		n = 1 // Ensure at least one element is considered if len(weights) < 5
	}

	// Sum up the lowest newEndpointsPercentileWeight
	var sum uint32
	for i := 0; i < n; i++ {
		sum += weights[i]
	}

	// Calculate the average of the lowest newEndpointsPercentileWeight
	return sum / uint32(n)
}

func addressesChanged(existingEndpoints, newEndpoints []*istioNetworkingV1.WorkloadEntry) bool {
	if len(existingEndpoints) != len(newEndpoints) {
		return true // Different number of endpoints
	}

	existingAddresses := make(map[string]bool)
	for _, endpoint := range existingEndpoints {
		existingAddresses[endpoint.Address] = true
	}

	for _, endpoint := range newEndpoints {
		if !existingAddresses[endpoint.Address] {
			return true // Found a new address not present in the existing endpoints
		}
	}

	return false // No changes in addresses found
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceEntryReconciler) SetupWithManager(mgr ctrl.Manager, logger logr.Logger) error {
	namespacePredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			//logger.Info("Create event received", "object", e.Object)
			namespace := e.Object.GetNamespace()
			//logger.Info("Create event for namespace", "namespace", namespace)
			inList := helpers.NamespaceInFilteredList(namespace, r.NamespaceList)
			//logger.Info("Namespace in list", "inList", inList)
			return inList
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			//logger.Info("Update event received", "old object", e.ObjectOld, "new object", e.ObjectNew)
			namespace := e.ObjectNew.GetNamespace()
			//logger.Info("Update event for namespace", "namespace", namespace)
			inList := helpers.NamespaceInFilteredList(namespace, r.NamespaceList)
			//logger.Info("Namespace in list", "inList", inList)
			return inList
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&optimizationv1alpha1.WeightOptimizer{}).
		WithEventFilter(namespacePredicate).
		WatchesRawSource(source.Channel(r.ServiceEntryReconcileTriggerChannel, &handler.EnqueueRequestForObject{})).
		Complete(r)
}
