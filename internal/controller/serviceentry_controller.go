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
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	istioNetworkingV1 "istio.io/api/networking/v1"
	istioClientV1 "istio.io/client-go/pkg/apis/networking/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
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
	InitialWeight                       int

	//TODO: add dry run mode logic
	//DryRun bool
}

type tKeyToMuxKey struct {
	Namespace string
	Name      string
}

type tMarkMux struct {
	mux     sync.Mutex
	touched int64
}

func (m *tMarkMux) checkTouchedAndReset() bool {
	return atomic.SwapInt64(&m.touched, 0) > 0
}

var (
	keyToMuxMux sync.Mutex
	keyToMux    = make(map[tKeyToMuxKey]*tMarkMux)
)

func getMux(namespace, name string) *tMarkMux {
	key := tKeyToMuxKey{
		Namespace: namespace,
		Name:      name,
	}
	keyToMuxMux.Lock()
	defer keyToMuxMux.Unlock()
	markMux, ok := keyToMux[key]
	if !ok {
		markMux = new(tMarkMux)
		keyToMux[key] = markMux
	}
	return markMux
}

func tryLock(name types.NamespacedName) (bool, func(), func() bool) {
	markMux := getMux(name.Namespace, name.Name)
	if !markMux.mux.TryLock() {
		atomic.AddInt64(&markMux.touched, 1)
		return false, nil, nil
	}
	return true, markMux.mux.Unlock, markMux.checkTouchedAndReset
}

//+kubebuilder:rbac:groups=core,resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=serviceentries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=serviceentries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=serviceentries/finalizers,verbs=update
//+kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=istioadaptiverequestoptimizers,verbs=get;list;watch

func (r *ServiceEntryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ok, unlock, checkTouchedAndReset := tryLock(req.NamespacedName)
	if !ok {
		return ctrl.Result{}, nil
	}
	defer unlock()
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	logger.V(1).Info("Reconcile ServiceEntry", "ServiceEntry.Namespace", req.Namespace, "ServiceEntry.Name", req.Name)
	// Step 1: Fetch the IstioAdaptiveRequestOptimizer object based on the request.
	var opt optimizationv1alpha1.IstioAdaptiveRequestOptimizer
	if err := r.Get(ctx, req.NamespacedName, &opt); err != nil {
		logger.Info("IstioAdaptiveRequestOptimizer not found. No weight adjustments made.")
		return ctrl.Result{
			Requeue: checkTouchedAndReset(),
		}, client.IgnoreNotFound(err)
	}

	// Step 2: Check for Endpoint updates.
	var serviceEntry istioClientV1.ServiceEntry
	err := r.Get(ctx, req.NamespacedName, &serviceEntry)
	if client.IgnoreNotFound(err) != nil {
		logger.Error(err, "Failed to fetch ServiceEntry.")
		return ctrl.Result{
			Requeue: checkTouchedAndReset(),
		}, err
	}

	oldWorkloads, err := handleEndpointUpdate(
		ctx,
		logger,
		r.Client,
		*r.ServiceEntryServiceNameLabelKey,
		uint32(r.InitialWeight),
		req,
		&serviceEntry,
		opt.Spec.LocalityEnabled,
	)
	if client.IgnoreNotFound(err) != nil {
		logger.Error(err, "Failed to handle Endpoint update.")
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "handle_endpoint_update", "name": req.Name, "namespace": req.Namespace}).Inc()
		return ctrl.Result{
			Requeue: checkTouchedAndReset(),
		}, err
	}
	cleanupPodMetrics(oldWorkloads, opt.Spec.ServiceNamespace, req.Name)
	return ctrl.Result{
		Requeue: checkTouchedAndReset(),
	}, nil
}

func handleEndpointUpdate(
	ctx context.Context,
	logger logr.Logger,
	c client.Client,
	serviceEntryServiceNameLabelKey string,
	initialWeight uint32,
	req ctrl.Request,
	serviceEntry *istioClientV1.ServiceEntry,
	localityEnabled bool,
) ([]*istioNetworkingV1.WorkloadEntry, error) {
	// Extract the original service name from the ServiceEntry's labels.
	originalServiceName := serviceEntry.Labels[serviceEntryServiceNameLabelKey]
	if originalServiceName == "" {
		logger.Error(nil, "ServiceEntry does not contain the expected label.", "Label", serviceEntryServiceNameLabelKey)
		return nil, fmt.Errorf("ServiceEntry does not contain the expected label %s", serviceEntryServiceNameLabelKey)
	}
	var endpointSlices discoveryv1.EndpointSliceList
	labelSelector := client.MatchingLabels{
		discoveryv1.LabelServiceName: originalServiceName,
	}
	if err := c.List(ctx, &endpointSlices, client.InNamespace(req.Namespace), labelSelector); err != nil {
		logger.Error(err, "Failed to list EndpointSlices for service", "Namespace", req.Namespace, "Name", originalServiceName)
		return nil, err
	}
	if len(endpointSlices.Items) == 0 {
		logger.Info("No EndpointSlices found for service", "Namespace", req.Namespace, "Name", originalServiceName)
		return nil, nil // No endpoints to process
	}
	addressToWorkloadEntry := make(map[string]*istioNetworkingV1.WorkloadEntry)
	for _, workloadEntry := range serviceEntry.Spec.Endpoints {
		addressToWorkloadEntry[workloadEntry.Address] = workloadEntry
	}
	existAddresses := make(map[string]struct{})
	newWorkloadEntries := make([]*istioNetworkingV1.WorkloadEntry, 0)
	for _, endpointSlice := range endpointSlices.Items {
		for _, endpoint := range endpointSlice.Endpoints {
			if readyPtr := endpoint.Conditions.Ready; readyPtr == nil || !*readyPtr {
				logger.Info("Endpoint is not ready", "Endpoint", endpoint)
				continue
			}
			var address string
			if addresses := endpoint.Addresses; len(addresses) > 0 {
				address = addresses[0]
			}
			if address == "" {
				// TODO(romang): check why this happens, if it is
				continue
			}
			if _, ok := existAddresses[address]; ok {
				// Skip duplicate endpoints
				// https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/#duplicate-endpoints
				continue
			}
			existAddresses[address] = struct{}{}
			workloadEntry, ok := addressToWorkloadEntry[address]
			if !ok {
				workloadEntry = &istioNetworkingV1.WorkloadEntry{
					Address: address,
					Weight:  initialWeight,
				}
			}
			var locality string
			if localityEnabled {
				if zonePtr := endpoint.Zone; zonePtr != nil {
					locality = *zonePtr
					workloadEntry.Locality = locality[:len(locality)-1] + "/" + locality
				}
			}
			newWorkloadEntries = append(newWorkloadEntries, workloadEntry)
		}
	}

	var coreService corev1.Service
	if err := c.Get(ctx, req.NamespacedName, &coreService); err != nil {
		logger.Error(err, "Failed to fetch Service.")
		return nil, err
	}
	serviceEntry.Spec.Ports = appendCoreServicePortsToIstioServicePorts(serviceEntry.Spec.Ports[:0], coreService.Spec.Ports)

	// Check if updates are required based on endpoint changes.
	//if !(addressesChanged(serviceEntry.Spec.Endpoints, newWorkloadEntries) || checkPortsChanged(serviceEntry.Spec.Ports, service.Spec.ServicePorts)) {
	//	logger.Info("No changes detected", "ServiceEntry", serviceEntry.Name)
	//	return nil, err // No changes, no need to update.
	//}
	workloadEntriesDiff := helpers.Diff(nil, newWorkloadEntries, serviceEntry.Spec.Endpoints)
	//logger.Info("Detected endpoint changes", "ServiceEntry", serviceEntry.Name)

	if len(workloadEntriesDiff) == 0 {
		logger.Info("No changes detected", "ServiceEntry", serviceEntry.Name)
		return nil, nil // No changes, no need to update.
	}
	// Update the ServiceEntry with the newly merged endpoints.
	serviceEntry.Spec.Endpoints = newWorkloadEntries
	if err := c.Update(ctx, serviceEntry); err != nil {
		logger.Error(err, "Failed to update ServiceEntry", "ServiceEntry", serviceEntry.Name)
		return nil, err // Return the error to retry
	}
	updateMetrics(serviceEntry)
	logger.Info("ServiceEntry updated handleEndpointUpdate with new weights", "ServiceEntry", serviceEntry.Name, "ServiceEntry.Spec.Endpoints", serviceEntry.Spec.Endpoints)
	return workloadEntriesDiff, nil
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

func updateMetrics(serviceEntry *istioClientV1.ServiceEntry) {
	for _, workloadEntry := range serviceEntry.Spec.Endpoints {
		podAddress := workloadEntry.Address
		podZone := getLocalityForMetric(workloadEntry.Locality)
		podWeight := workloadEntry.Weight
		customMetrics.WeightMetric.WithLabelValues(serviceEntry.Namespace, serviceEntry.Name, podAddress, podZone).Set(float64(podWeight))
	}
}

func cleanupPodMetrics(oldWorkloadEntries []*istioNetworkingV1.WorkloadEntry, serviceEntryNamespace, serviceEntryName string) {
	for _, workloadEntry := range oldWorkloadEntries {
		podAddress := workloadEntry.Address
		podZone := getLocalityForMetric(workloadEntry.Locality)
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
