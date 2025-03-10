package controller

import (
	"context"
	"slices"
	"strings"

	istioClientV1 "istio.io/client-go/pkg/apis/networking/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	api "istio-adaptive-least-request/api/v1alpha1"
)

// EndpointSliceReconciler reconciles an EndpointSlice object
type EndpointSliceReconciler struct {
	client.Client
	Scheme                          *runtime.Scheme
	LoggerName                      string
	ServiceEntryServiceNameLabelKey string
	NamespaceList                   []string
	InitialWeight                   uint32
}

//+kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=networking.istio.io,resources=serviceentries,verbs=get;list;watch
//+kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=istioadaptiverequestoptimizers,verbs=get;list;watch

func (r *EndpointSliceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	logger.V(1).Info("Reconcile EndpointSlice", "Namespace", req.Namespace, "Name", req.Name)
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
		logger.Error(err, "Failed to list ServiceEntry", "Namespace", req.Namespace, "ServiceName", serviceName)
		return ctrl.Result{}, err
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
	oldWorkloads, requeue, err := handleEndpointUpdate(
		ctx,
		logger,
		r.Client,
		r.ServiceEntryServiceNameLabelKey,
		r.InitialWeight,
		req,
		&serviceEntry,
		opt.Spec.LocalityEnabled,
	)
	if err != nil {
		return ctrl.Result{
			Requeue: requeue,
		}, err
	}
	cleanupPodMetrics(oldWorkloads, opt.Spec.ServiceNamespace, req.Name)
	return ctrl.Result{
		Requeue: requeue,
	}, nil
}

// endpointSliceNameToServiceName extracts the Service name from the EndpointSlice name.
func endpointSliceNameToServiceName(endpointSliceName string) string {
	lastHyphenIndex := strings.LastIndex(endpointSliceName, "-")
	if lastHyphenIndex == -1 {
		return ""
	}
	return endpointSliceName[:lastHyphenIndex]
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
