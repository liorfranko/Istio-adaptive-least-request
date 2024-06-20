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
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	optimizationv1alpha1 "istio-adaptive-least-request/api/v1alpha1"
	"istio-adaptive-least-request/internal/helpers"
	customMetrics "istio-adaptive-least-request/internal/metrics"
	istioNetworkingV1 "istio.io/api/networking/v1"
	istioClientV1 "istio.io/client-go/pkg/apis/networking/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"sort"

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
}

//+kubebuilder:rbac:groups=core,resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=serviceentries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=serviceentries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=serviceentries/finalizers,verbs=update
//+kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=istioadaptiverequestoptimizers,verbs=get;list;watch

func (r *ServiceEntryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	logger.V(1).Info("Reconcile ServiceEntry", "ServiceEntry.Namespace", req.Namespace, "ServiceEntry.Name", req.Name)
	// Step 1: Check for Endpoint updates.
	_, updateErr := r.HandleEndpointUpdate(ctx, req)
	if client.IgnoreNotFound(updateErr) != nil {
		logger.Error(updateErr, "Failed to handle Endpoint update.")
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "handle_endpoint_update", "name": req.Name, "namespace": req.Namespace}).Inc()
		return ctrl.Result{}, updateErr
	}

	// Step 2: Fetch the WeightOptimizer object based on the request.
	opt := &optimizationv1alpha1.WeightOptimizer{}
	if err := r.Get(ctx, req.NamespacedName, opt); err != nil {
		logger.Info("WeightOptimizer not found. No weight adjustments made.")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Step 3: Validate that the endpoints in the ServiceEntry match the current cluster state
	//serviceEntryUpdated, validationErr := r.ValidateAndUpdateWeights(ctx, req, opt)
	_, validationErr := r.ValidateAndUpdateWeights(ctx, req, opt)
	if validationErr != nil {
		logger.Error(validationErr, "Failed to validate or update weights.")
		return ctrl.Result{}, validationErr
	}

	return ctrl.Result{}, nil
}

// HandleEndpointUpdate checks for changes in the Endpoints and updates the ServiceEntry if necessary.
func (r *ServiceEntryReconciler) HandleEndpointUpdate(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)

	// Fetch the specific ServiceEntry.
	serviceEntry := istioClientV1.ServiceEntry{}
	err := r.Get(ctx, req.NamespacedName, &serviceEntry)
	if client.IgnoreNotFound(err) != nil {
		logger.Error(err, "Failed to fetch ServiceEntry.")
		return ctrl.Result{}, err
	}

	// Extract the original service name from the ServiceEntry's labels.
	originalServiceName := serviceEntry.Labels[*r.ServiceEntryServiceNameLabelKey]
	if originalServiceName == "" {
		logger.Error(nil, "ServiceEntry does not contain the expected label.", "Label", *r.ServiceEntryServiceNameLabelKey)
		return ctrl.Result{}, nil // or an error, as appropriate
	}

	// Construct the NamespacedName for the Endpoints object, assuming it's in the same namespace.
	endpointsNamespacedName := types.NamespacedName{Namespace: req.Namespace, Name: originalServiceName}

	// Fetch the corresponding Endpoints object.
	endpoints := &corev1.Endpoints{}
	if err := r.Get(ctx, endpointsNamespacedName, endpoints); err != nil {
		logger.Error(err, "Failed to fetch Endpoints for service", "Namespace", endpointsNamespacedName.Namespace, "Name", endpointsNamespacedName.Name)
		return ctrl.Result{}, err
	}
	// Track existing endpoint weights.
	existingWeights := map[string]uint32{}
	for _, ep := range serviceEntry.Spec.Endpoints {
		existingWeights[ep.Address] = ep.Weight
	}

	// Calculate average weight for new endpoints.
	averageWeight := r.CreateDefaultWeightForNewEndpoints(existingWeights)
	logger.Info("Calculated average weight for new endpoints", "AverageWeight", averageWeight)
	// Aggregate endpoints, assigning average weight to new ones.
	var mergedEndpoints []*istioNetworkingV1.WorkloadEntry
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			weight, found := existingWeights[address.IP]
			if !found {
				weight = averageWeight // Assign the calculated average weight if new.
			}

			mergedEndpoints = append(mergedEndpoints, &istioNetworkingV1.WorkloadEntry{
				Address: address.IP,
				Weight:  weight,
			})
		}
	}

	// Check if updates are required based on endpoint changes.
	if !addressesChanged(serviceEntry.Spec.Endpoints, mergedEndpoints) {
		logger.Info("No endpoint changes detected", "ServiceEntry", serviceEntry.Name)
		return ctrl.Result{}, nil // No changes, no need to update.
	}
	logger.Info("Detected endpoint changes", "ServiceEntry", serviceEntry.Name)
	// Update the ServiceEntry with the newly merged endpoints.
	serviceEntry.Spec.Endpoints = mergedEndpoints
	if err := r.Update(ctx, &serviceEntry); err != nil {
		logger.Error(err, "Failed to update ServiceEntry", "ServiceEntry", serviceEntry.Name)
		return ctrl.Result{}, nil // or an error, as appropriate
	}

	logger.Info("ServiceEntry updated successfully with dynamic weighting", "ServiceEntry", serviceEntry.Name)
	return ctrl.Result{}, nil
}

// ValidateAndUpdateWeights checks if endpoints match the expected state and updates weights if necessary.
func (r *ServiceEntryReconciler) ValidateAndUpdateWeights(ctx context.Context, req ctrl.Request, optWeight *optimizationv1alpha1.WeightOptimizer) (bool, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)

	// Fetch the specific ServiceEntry.
	serviceEntry := istioClientV1.ServiceEntry{}
	err := r.Get(ctx, req.NamespacedName, &serviceEntry)
	if client.IgnoreNotFound(err) != nil {
		logger.Error(err, "Failed to fetch ServiceEntry.")
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "fetch_service_entry", "name": req.Name, "namespace": req.Namespace}).Inc()
		return false, err
	}

	// Create weightsMap to store the weights of the endpoints
	weightsMap := make(map[string]uint32)
	for _, ep := range optWeight.Spec.Endpoints {
		weightsMap[ep.IP] = ep.Weight
	}
	for _, ep := range serviceEntry.Spec.Endpoints {
		if weightsMap[ep.Address] != 0 {
			ep.Weight = weightsMap[ep.Address]
		}
	}

	// Update the ServiceEntry with the new weights
	logger.Info("ServiceEntry updated with new weights", "ServiceEntry", serviceEntry.Name, "ServiceEntry.Spec.Endpoints", serviceEntry.Spec.Endpoints)
	if err := r.Update(ctx, &serviceEntry); err != nil {
		if errors.IsConflict(err) {
			logger.Info("ServiceEntry has been modified. Requeueing.")
			return false, err
		}
		logger.Error(err, "Failed to update ServiceEntry weights.")
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "validate_and_update_weights", "name": req.Name, "namespace": req.Namespace}).Inc()
		return false, err
	}
	return true, nil // Return true indicating weights were updated.
}

func (r *ServiceEntryReconciler) CreateDefaultWeightForNewEndpoints(existingWeights map[string]uint32) uint32 {
	if len(existingWeights) == 0 {
		middle := (r.MaximumWeight + r.MinimumWeight) / 2
		return uint32(middle) // Return the middle value if no existing weights
	}

	// Convert map values to slice
	weights := make([]uint32, 0, len(existingWeights))
	for _, weight := range existingWeights {
		weights = append(weights, weight)
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
