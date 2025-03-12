package controller

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	istioNetworkingV1 "istio.io/api/networking/v1"
	istioClientV1 "istio.io/client-go/pkg/apis/networking/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	api "istio-adaptive-least-request/api/v1alpha1"
	"istio-adaptive-least-request/internal/helpers"
	"istio-adaptive-least-request/internal/metrics"
)

// DefaultWeightForNewEndpoints represents the default weight assigned to new endpoints in a ServiceEntry.
const DefaultWeightForNewEndpoints uint32 = 1000

// IstioAdaptiveRequestOptimizerReconciler reconciles a IstioAdaptiveRequestOptimizer object
type IstioAdaptiveRequestOptimizerReconciler struct {
	client.Client
	Scheme                          *runtime.Scheme
	LoggerName                      string
	ServiceEntryLabelKey            string
	ServiceEntryServiceNameLabelKey string
	NamespaceList                   []string
	RequeueAfter                    time.Duration
	QueryInterval                   string
	VmdbUrl                         string
	StepInterval                    string
	ScaleupFactor                   float64
	ScaledownFactor                 float64
	MinimumWeight                   int
	InitialWeight                   int
}

// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=istioadaptiverequestoptimizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=istioadaptiverequestoptimizers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=istioadaptiverequestoptimizers/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=networking.istio.io,resources=serviceentries,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.istio.io,resources=serviceentries/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=networking.istio.io,resources=serviceentries/finalizers,verbs=update

func (r *IstioAdaptiveRequestOptimizerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	logger.V(0).Info("Reconcile IstioAdaptiveRequestOptimizer",
		"IstioAdaptiveRequestOptimizer.Namespace", req.Namespace,
		"IstioAdaptiveRequestOptimizer.Name", req.Name,
	)
	var opt api.IstioAdaptiveRequestOptimizer
	if err := r.Get(ctx, req.NamespacedName, &opt); err != nil {
		logger.Info("IstioAdaptiveRequestOptimizer not found",
			"Namespace", req.Namespace,
			"Name", req.Name,
		)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.V(1).Info("IstioAdaptiveRequestOptimizer fetched",
		"IstioAdaptiveRequestOptimizer", opt.Spec,
	)
	if opt.GetDeletionTimestamp() != nil {
		logger.Info("IstioAdaptiveRequestOptimizer marked for deletion",
			"IstioAdaptiveRequestOptimizer", opt.Name,
		)
		return ctrl.Result{}, nil
	}
	var service corev1.Service
	objectKey := client.ObjectKey{
		Name:      opt.Spec.ServiceName,
		Namespace: opt.Spec.ServiceNamespace,
	}
	if err := r.Get(ctx, objectKey, &service); err != nil {
		logger.Error(err, "Failed to fetch Service", "ServiceName", opt.Spec.ServiceName)
		return ctrl.Result{}, err
	}
	logger.V(1).Info("Service fetched", "Service", &service)
	var serviceEntry istioClientV1.ServiceEntry
	objectKey = client.ObjectKey{
		Name:      opt.Name,
		Namespace: opt.Namespace,
	}
	if err := r.Get(ctx, objectKey, &serviceEntry); err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		var endpointSliceList discoveryv1.EndpointSliceList
		labelSelector := client.MatchingLabels{
			discoveryv1.LabelServiceName: service.Name,
		}
		if err := r.List(ctx, &endpointSliceList, client.InNamespace(service.Namespace), labelSelector); err != nil {
			logger.Error(err, "Failed to list EndpointSlices for Service", "ServiceName", service.Name)
			return ctrl.Result{}, err
		}
		r.initServiceEntry(ctx, &service, endpointSliceList.Items, &opt, &serviceEntry)
		if err := r.Create(ctx, &serviceEntry); err != nil {
			logger.Error(err, "Failed to create ServiceEntry",
				"ServiceEntry", serviceEntry.Name,
			)
			return ctrl.Result{}, err
		}
		logger.Info("ServiceEntry created successfully",
			"ServiceEntry", serviceEntry.Name,
		)
	} else {
		if len(serviceEntry.Spec.Endpoints) == 0 {
			logger.Info("ServiceEntry doesn't have any endpoints, continue",
				"ServiceEntry", serviceEntry.Name,
			)
			return ctrl.Result{RequeueAfter: r.RequeueAfter * time.Second}, nil
		}
		var podList corev1.PodList
		listOpts := client.ListOptions{
			Namespace: opt.Namespace,
			LabelSelector: labels.SelectorFromSet(labels.Set{
				"service.istio.io/canonical-name": opt.Spec.ServiceName,
			}),
		}
		if err := r.Client.List(ctx, &podList, &listOpts); err != nil {
			return ctrl.Result{}, err
		}
		podsInfo := addPods(logger, podList.Items, nil)
		getPodMetricsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		podAddressToPodMetrics, err := getPodMetrics(
			getPodMetricsCtx,
			logger,
			opt.Name,
			opt.Namespace,
			podsInfo,
			r.QueryInterval,
			r.VmdbUrl,
			r.StepInterval,
		)
		if err != nil {
			// If there is a problem with pulling the metrics from VictoriaMetrics, log an error and continue to the next port
			metrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName,
				"type":      "get_metrics_from_vm",
				"name":      opt.Name,
				"namespace": opt.Namespace,
			}).Inc()
			if err := fallbackStrategy(ctx, logger, r.Client, &opt, &serviceEntry, uint32(r.MinimumWeight)); err != nil {
				metrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName,
					"type":      "fallback_strategy",
					"name":      opt.Name,
					"namespace": opt.Namespace,
				}).Inc()
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}
		enrichPodMetrics(logger, podAddressToPodMetrics)
		distributeWeightsBasedOnCPU(
			logger,
			podAddressToPodMetrics,
			&serviceEntry,
			r.ScaleupFactor,
			r.ScaledownFactor,
			r.MinimumWeight,
		)
		if err := r.Update(ctx, &serviceEntry); err != nil {
			logger.Error(err, "Failed to validate or update weights.")
			return ctrl.Result{}, err
		}
		updateMetrics(&serviceEntry)
	}
	status := &opt.Status
	now := metav1.Now()
	status.LastOptimizedTime = &now
	status.ObservedGeneration = opt.Generation
	status.ServiceEntries = []api.ServiceEntry{{
		Name:         serviceEntry.Name,
		Namespace:    serviceEntry.Namespace,
		CreationTime: serviceEntry.CreationTimestamp,
	}} // TODO(romang): change ServiceEntries to ServiceEntry
	if err := r.Client.Status().Update(ctx, &opt); err != nil {
		// TODO: roman check why.
		logger.Error(err, "Failed to update opt status with service entries")
		return ctrl.Result{}, err
	}
	// Requeue reconciliation every 60 seconds to track new EndpointSlices.
	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
}

func (r *IstioAdaptiveRequestOptimizerReconciler) initServiceEntry(
	ctx context.Context,
	service *corev1.Service,
	endpointSlices []discoveryv1.EndpointSlice,
	opt *api.IstioAdaptiveRequestOptimizer,
	serviceEntry *istioClientV1.ServiceEntry,
) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	existAddresses := make(map[string]struct{})
	var serviceEntryEndpoints []*istioNetworkingV1.WorkloadEntry
	for i := range endpointSlices {
		endpointSlice := &endpointSlices[i]
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
			workloadEntry := &istioNetworkingV1.WorkloadEntry{
				Address: address,
				Weight:  DefaultWeightForNewEndpoints,
			}
			if opt.Spec.LocalityEnabled {
				if zonePtr := endpoint.Zone; zonePtr != nil {
					zone := *zonePtr
					workloadEntry.Locality = zone[:len(zone)-1] + "/" + zone
				}
			}
			serviceEntryEndpoints = append(serviceEntryEndpoints, workloadEntry)
		}
	}
	*serviceEntry = istioClientV1.ServiceEntry{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "networking.istio.io/v1",
			Kind:       "ServiceEntry",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      service.Name,
			Namespace: service.Namespace,
			Labels: map[string]string{
				r.ServiceEntryLabelKey:            "true",
				r.ServiceEntryServiceNameLabelKey: service.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					Name:       opt.Name,
					APIVersion: opt.APIVersion,
					Kind:       opt.Kind,
					UID:        opt.UID,
				},
			},
		},
		Spec: istioNetworkingV1.ServiceEntry{
			Hosts:      []string{service.Name + "." + service.Namespace + ".svc.cluster.local"},
			Endpoints:  serviceEntryEndpoints,
			Location:   istioNetworkingV1.ServiceEntry_MESH_INTERNAL,
			Resolution: istioNetworkingV1.ServiceEntry_STATIC,
		},
	}
	serviceEntry.Spec.Ports = appendCoreServicePortsToIstioServicePorts(serviceEntry.Spec.Ports[:0], service.Spec.Ports)
	logger.Info("Try to create ServiceEntry",
		"ServiceEntry", serviceEntry.Name,
	)
}

func appendCoreServicePortsToIstioServicePorts(
	istioServicePorts []*istioNetworkingV1.ServicePort,
	coreServicePorts []corev1.ServicePort,
) []*istioNetworkingV1.ServicePort {
	for i := range coreServicePorts {
		port := &coreServicePorts[i]
		istioServicePort := &istioNetworkingV1.ServicePort{
			Number:     uint32(port.Port),
			Protocol:   helpers.SafeDereferenceAppProtocol(port.AppProtocol),
			Name:       port.Name,
			TargetPort: uint32(port.TargetPort.IntValue()),
		}
		istioServicePorts = append(istioServicePorts, istioServicePort)
	}
	return istioServicePorts
}

// SetupWithManager sets up the controller with the Manager.
func (r *IstioAdaptiveRequestOptimizerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	namespacePredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return helpers.NamespaceInFilteredList(obj.GetNamespace(), r.NamespaceList)
	})
	ignoreStatusUpdatesPredicate := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			// Only reconcile if the spec has changed
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.IstioAdaptiveRequestOptimizer{}).
		WithEventFilter(namespacePredicate).
		WithEventFilter(ignoreStatusUpdatesPredicate).
		Complete(r)
}
