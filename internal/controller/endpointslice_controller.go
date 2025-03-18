package controller

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	istioNetworkingV1 "istio.io/api/networking/v1"
	istioClientV1 "istio.io/client-go/pkg/apis/networking/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	api "istio-adaptive-least-request/api/v1alpha1"
	"istio-adaptive-least-request/internal/metrics"
)

// EndpointSliceReconciler reconciles an EndpointSlice object
type EndpointSliceReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	LoggerName    string
	NamespaceList []string
	InitialWeight uint32
}

//+kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
//+kubebuilder:rbac:groups=networking.istio.io,resources=serviceentries,verbs=get;list;watch
//+kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=istioadaptiverequestoptimizers,verbs=get;list;watch

func (r *EndpointSliceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	serviceName := endpointSliceNameToServiceName(req.Name)
	if serviceName == "" {
		logger.Info("Failed to extract service name from EndpointSlice name")
		return ctrl.Result{}, nil
	}
	var serviceEntry istioClientV1.ServiceEntry
	key := client.ObjectKey{
		Namespace: req.Namespace,
		Name:      serviceName,
	}
	if err := r.Client.Get(ctx, key, &serviceEntry); err != nil {
		// logger.Error(err, "Failed to list ServiceEntry", "Namespace", req.Namespace, "ServiceName", serviceName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ownerReferences := serviceEntry.OwnerReferences
	if len(ownerReferences) == 0 {
		logger.Info("ServiceEntry does not have owner reference. No weight adjustments made.")
		return ctrl.Result{}, nil
	}
	optName := ownerReferences[0].Name
	objectKey := client.ObjectKey{
		Namespace: req.Namespace,
		Name:      optName,
	}
	var opt api.IstioAdaptiveRequestOptimizer
	if err := r.Get(ctx, objectKey, &opt); err != nil {
		logger.Info("IstioAdaptiveRequestOptimizer not found. No weight adjustments made.")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.Info("Starting Reconcile EndpointSlice")
	oldWorkloads, requeue, err := handleEndpointUpdate(
		ctx,
		logger,
		r.Client,
		r.InitialWeight,
		&serviceEntry,
		opt.Spec.LocalityEnabled,
	)
	if err != nil {
		return ctrl.Result{
			Requeue: requeue,
		}, err
	}
	cleanupPodMetricsFromWorkloadEntries(oldWorkloads, opt.Spec.ServiceNamespace, req.Name)
	return ctrl.Result{
		Requeue: requeue,
	}, nil
}

func cleanupPodMetricsFromWorkloadEntries(oldWorkloadEntries []*istioNetworkingV1.WorkloadEntry, serviceEntryNamespace, serviceEntryName string) {
	for _, workloadEntry := range oldWorkloadEntries {
		podAddress := workloadEntry.Address
		metrics.WeightMetric.Delete(prometheus.Labels{
			"service_namespace": serviceEntryNamespace,
			"service_name":      serviceEntryName,
			"pod_ip":            podAddress,
		})
	}
}

// endpointSliceNameToServiceName extracts the Service name from the EndpointSlice name.
func endpointSliceNameToServiceName(endpointSliceName string) string {
	lastHyphenIndex := strings.LastIndex(endpointSliceName, "-")
	if lastHyphenIndex == -1 {
		return ""
	}
	return endpointSliceName[:lastHyphenIndex]
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

func tryLock(namespace, name string) (bool, func(), func() bool) {
	markMux := getMux(namespace, name)
	if !markMux.mux.TryLock() {
		atomic.AddInt64(&markMux.touched, 1)
		return false, nil, nil
	}
	return true, markMux.mux.Unlock, markMux.checkTouchedAndReset
}

func eqAddress(a, b *istioNetworkingV1.WorkloadEntry) bool {
	return a.Address == b.Address
}

func oneSideDiff(dst, a, b []*istioNetworkingV1.WorkloadEntry) []*istioNetworkingV1.WorkloadEntry {
	for _, workloadEntry := range a {
		if !contains(b, workloadEntry, eqAddress) {
			dst = append(dst, workloadEntry)
		}
	}
	return dst
}

func diff(dst, desired, actual []*istioNetworkingV1.WorkloadEntry) []*istioNetworkingV1.WorkloadEntry {
	// Find elements in actual that are not in desired (removals)
	dst = oneSideDiff(dst, actual, desired)
	// Find elements in desired that are not in actual (additions)
	dst = oneSideDiff(dst, desired, actual)
	return dst
}

func equalAddressAndLocality(a, b *istioNetworkingV1.WorkloadEntry) bool {
	if a.Address != b.Address {
		return false
	}
	if a.Locality != b.Locality {
		return false
	}
	return true
}

func contains[T any](entries []T, targetEntry T, eqFn func(T, T) bool) bool {
	for _, entry := range entries {
		if eqFn(entry, targetEntry) {
			return true
		}
	}
	return false
}

func hasChanges(old, new []*istioNetworkingV1.WorkloadEntry) bool {
	if len(old) != len(new) {
		return true
	}
	for _, newEntry := range new {
		if !contains(old, newEntry, equalAddressAndLocality) {
			return true
		}
	}
	return false
}

func handleEndpointUpdate(
	ctx context.Context,
	logger logr.Logger,
	c client.Client,
	initialWeight uint32,
	serviceEntry *istioClientV1.ServiceEntry,
	localityEnabled bool,
) ([]*istioNetworkingV1.WorkloadEntry, bool, error) {
	ok, unlock, checkTouchedAndReset := tryLock(serviceEntry.Namespace, serviceEntry.Name)
	if !ok {
		return nil, false, nil
	}
	defer unlock()
	var endpointSlices discoveryv1.EndpointSliceList
	labelSelector := client.MatchingLabels{
		discoveryv1.LabelServiceName: serviceEntry.Name,
	}
	if err := c.List(ctx, &endpointSlices, client.InNamespace(serviceEntry.Namespace), labelSelector); err != nil {
		logger.Error(err, "Failed to list EndpointSlices for service",
			"Namespace", serviceEntry.Namespace,
			"Name", serviceEntry.Name,
		)
		return nil, checkTouchedAndReset(), err
	}
	if len(endpointSlices.Items) == 0 {
		logger.Info("No EndpointSlices found for service",
			"Namespace", serviceEntry.Namespace,
			"Name", serviceEntry.Name,
		)
		return nil, checkTouchedAndReset(), nil // No endpoints to process
	}
	addressToWeight := make(map[string]uint32)
	oldWorkloadEntries := serviceEntry.Spec.Endpoints
	for _, oldWorkloadEntry := range oldWorkloadEntries {
		addressToWeight[oldWorkloadEntry.Address] = oldWorkloadEntry.Weight
	}
	existAddresses := make(map[string]struct{})
	newWorkloadEntries := make([]*istioNetworkingV1.WorkloadEntry, 0)
	for i := range endpointSlices.Items {
		endpointSlice := &endpointSlices.Items[i]
		if !endpointSlice.DeletionTimestamp.IsZero() {
			// Skip deleted EndpointSlices
			continue
		}
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
			weight, ok := addressToWeight[address]
			if !ok {
				weight = initialWeight
			}
			var locality string
			if localityEnabled {
				if zonePtr := endpoint.Zone; zonePtr != nil {
					locality = *zonePtr
					locality = locality[:len(locality)-1] + "/" + locality
				}
			}
			newWorkloadEntry := &istioNetworkingV1.WorkloadEntry{
				Address:  address,
				Weight:   weight,
				Locality: locality,
			}
			newWorkloadEntries = append(newWorkloadEntries, newWorkloadEntry)
		}
	}
	var coreService corev1.Service
	objectKey := client.ObjectKey{
		Namespace: serviceEntry.Namespace,
		Name:      serviceEntry.Name,
	}
	if err := c.Get(ctx, objectKey, &coreService); err != nil {
		logger.Error(err, "Failed to fetch Service.")
		return nil, checkTouchedAndReset(), err
	}
	serviceEntry.Spec.Ports = appendCoreServicePortsToIstioServicePorts(serviceEntry.Spec.Ports[:0], coreService.Spec.Ports)
	workloadEntriesDiff := diff(nil, newWorkloadEntries, oldWorkloadEntries)
	if !hasChanges(oldWorkloadEntries, newWorkloadEntries) {
		logger.Info("No changes detected", "ServiceEntry", serviceEntry.Name)
		return nil, checkTouchedAndReset(), nil // No changes, no need to update.
	}
	if err := c.Update(ctx, serviceEntry); err != nil {
		logger.Error(err, "Failed to update ServiceEntry", "ServiceEntry", serviceEntry.Name)
		return nil, checkTouchedAndReset(), err // Return the error to retry
	}
	updateMetrics(serviceEntry)
	logger.Info("ServiceEntry updated handleEndpointUpdate with new weights", "ServiceEntry", serviceEntry.Name, "ServiceEntry.Spec.Endpoints", newWorkloadEntries)
	return workloadEntriesDiff, checkTouchedAndReset(), nil
}

func (r *EndpointSliceReconciler) checkNamespace(obj client.Object) bool {
	return slices.Contains(r.NamespaceList, obj.GetNamespace())
}

// SetupWithManager sets up the controller with the Manager.
func (r *EndpointSliceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	namespaceAndAnnotationPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return r.checkNamespace(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return r.checkNamespace(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return r.checkNamespace(e.ObjectNew)
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&discoveryv1.EndpointSlice{}).
		WithEventFilter(namespaceAndAnnotationPredicate).
		Complete(r)
}
