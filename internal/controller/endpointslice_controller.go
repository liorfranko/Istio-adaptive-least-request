package controller

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	istioClientV1 "istio.io/client-go/pkg/apis/networking/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	optimizationv1alpha1 "istio-adaptive-least-request/api/v1alpha1"
	"istio-adaptive-least-request/internal/helpers"
	customMetrics "istio-adaptive-least-request/internal/metrics"

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
	InitialWeight                       uint32
}

//+kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=networking.istio.io,resources=serviceentries,verbs=get;list;watch
//+kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=istioadaptiverequestoptimizers,verbs=get;list;watch

func (r *EndpointSliceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ok, unlock, checkTouchedAndReset := tryLock(req.NamespacedName)
	if !ok {
		return ctrl.Result{}, nil
	}
	defer unlock()
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	logger.V(1).Info("Reconcile EndpointSlice", "EndpointSlice.Namespace", req.Namespace, "EndpointSlice.Name", req.Name)
	var endpointSlice discoveryv1.EndpointSlice
	if err := r.Get(ctx, req.NamespacedName, &endpointSlice); err != nil {
		logger.Error(err, "Failed to fetch EndpointSlice", "Namespace", req.Namespace, "Name", req.Name)
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "fetching_endpointslice", "name": req.Name, "namespace": req.Namespace}).Inc()
		return ctrl.Result{
			Requeue: checkTouchedAndReset(),
		}, err
	}
	logger.V(1).Info("EndpointSlice fetched", "Endpoints", endpointSlice.Endpoints)
	if isObjectMarkedForDeletion(&endpointSlice) {
		return ctrl.Result{
			Requeue: checkTouchedAndReset(),
		}, nil
	}
	// Extract the service name from the EndpointSlice labels
	serviceName := endpointSlice.Labels[discoveryv1.LabelServiceName]

	// Fetch the corresponding ServiceEntry resources
	var serviceEntry istioClientV1.ServiceEntry
	key := client.ObjectKey{
		Namespace: req.Namespace,
		Name:      serviceName,
	}
	if err := r.Client.Get(ctx, key, &serviceEntry); err != nil {
		logger.Error(err, "Failed to list ServiceEntry", "Namespace", req.Namespace, "ServiceName", serviceName)
		return ctrl.Result{
			Requeue: checkTouchedAndReset(),
		}, err
	}
	var opt optimizationv1alpha1.IstioAdaptiveRequestOptimizer
	if err := r.Get(ctx, req.NamespacedName, &opt); err != nil {
		logger.Info("IstioAdaptiveRequestOptimizer not found. No weight adjustments made.")
		return ctrl.Result{
			Requeue: checkTouchedAndReset(),
		}, client.IgnoreNotFound(err)
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
	if err != nil {
		return ctrl.Result{
			Requeue: checkTouchedAndReset(),
		}, err
	}
	cleanupPodMetrics(oldWorkloads, opt.Spec.ServiceNamespace, req.Name)
	return ctrl.Result{
		Requeue: checkTouchedAndReset(),
	}, nil
}

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
