package controller

import (
	"context"
	"github.com/prometheus/client_golang/prometheus"
	"istio-adaptive-least-request/internal/helpers"
	customMetrics "istio-adaptive-least-request/internal/metrics"
	istioClientV1 "istio.io/client-go/pkg/apis/networking/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// EndpointSliceReconciler reconciles an EndpointSlice object
type EndpointSliceReconciler struct {
	client.Client
	Scheme                          *runtime.Scheme
	LoggerName                      string
	DryRun                          bool
	EndpointsAnnotationKey          *string
	ServiceEntryServiceNameLabelKey *string
	// Channel used to trigger reconciliation of ServiceEntry resources.
	ServiceEntryReconcileTriggerChannel chan event.GenericEvent
	NamespaceList                       []string
}

//+kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=networking.istio.io,resources=serviceentries,verbs=get;list;watch
//+kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=istioadaptiverequestoptimizers,verbs=get;list;watch

func (r *EndpointSliceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	logger.V(1).Info("Reconcile EndpointSlice", "EndpointSlice.Namespace", req.Namespace, "EndpointSlice.Name", req.Name)

	endpointSlice := &discoveryv1.EndpointSlice{}
	if err := r.Get(ctx, req.NamespacedName, endpointSlice); err != nil {
		logger.Error(err, "Failed to fetch EndpointSlice", "Namespace", req.Namespace, "Name", req.Name)
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "fetching_endpointslice", "name": req.Name, "namespace": req.Namespace}).Inc()
		return ctrl.Result{}, err
	}
	logger.V(1).Info("EndpointSlice fetched", "Endpoints", endpointSlice.Endpoints)

	// Extract the service name from the EndpointSlice labels
	serviceName := endpointSlice.Labels[discoveryv1.LabelServiceName]

	// Fetch the corresponding ServiceEntry resources
	var serviceEntryList istioClientV1.ServiceEntryList
	labelKey := *r.ServiceEntryServiceNameLabelKey
	if err := r.List(ctx, &serviceEntryList, client.InNamespace(req.Namespace), client.MatchingLabels{labelKey: serviceName}); err != nil {
		logger.Error(err, "Failed to list ServiceEntry", "Namespace", req.Namespace, "ServiceName", serviceName)
		return ctrl.Result{}, err
	}
	logger.V(1).Info("ServiceEntry fetched", "ServiceEntries", serviceEntryList.Items)

	// Collect addresses from the EndpointSlice
	// TODO: add ready addresses only
	endpointAddressesSet := make(map[string]struct{})
	for _, endpoint := range endpointSlice.Endpoints {
		for _, addr := range endpoint.Addresses {
			endpointAddressesSet[addr] = struct{}{}
		}
	}

	for _, se := range serviceEntryList.Items {
		// Collect addresses from the ServiceEntry
		serviceEntryAddressesSet := make(map[string]struct{})
		for _, addr := range se.Spec.Addresses {
			serviceEntryAddressesSet[addr] = struct{}{}
		}
		// Compare the addresses
		if !addressesEqual(endpointAddressesSet, serviceEntryAddressesSet) {
			// Addresses are different, trigger reconciliation
			logger.V(1).Info("EndpointSlice and ServiceEntry addresses differ, triggering reconciliation", "ServiceEntry.Name", se.Name)
			r.ServiceEntryReconcileTriggerChannel <- event.GenericEvent{
				Object: &istioClientV1.ServiceEntry{
					ObjectMeta: metav1.ObjectMeta{
						Name:      se.Name,
						Namespace: se.Namespace,
					},
				},
			}
			// TODO: send metric in the end
		}
	}

	return ctrl.Result{}, nil
}

// addressesEqual compares two sets of addresses represented as maps
func addressesEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// checkNamespaceAndAnnotation checks if the namespace of the object is allowed
// and if the specified annotation exists and has the correct value.
func (r *EndpointSliceReconciler) checkNamespaceAndAnnotation(obj client.Object) bool {
	if r.EndpointsAnnotationKey == nil {
		// Optionally log a warning
		//log.Log.V(1).Info("EndpointsAnnotationKey is not set, skipping annotation check")
		return false
	}
	annotationKey := *r.EndpointsAnnotationKey
	annotationValue := "true"

	// First, check if the namespace is in the allowed list.
	if !helpers.NamespaceInFilteredList(obj.GetNamespace(), r.NamespaceList) {
		return false
	}
	// Then, check if the annotation exists and has the correct value.
	annotations := obj.GetAnnotations()
	val, exists := annotations[annotationKey]
	return exists && val == annotationValue
}

// SetupWithManager sets up the controller with the Manager.
func (r *EndpointSliceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	namespaceAndAnnotationPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return r.checkNamespaceAndAnnotation(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return r.checkNamespaceAndAnnotation(e.ObjectNew)
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&discoveryv1.EndpointSlice{}).
		WithEventFilter(namespaceAndAnnotationPredicate).
		Complete(r)
}
