package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/prometheus/client_golang/prometheus"

	optimizationv1alpha1 "istio-adaptive-least-request/api/v1alpha1"
	"istio-adaptive-least-request/internal/helpers"
	customMetrics "istio-adaptive-least-request/internal/metrics"

	istioNetworkingV1 "istio.io/api/networking/v1"
	istioClientV1 "istio.io/client-go/pkg/apis/networking/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const istioAdaptiveRequestOptimizerFinalizer = "optimization.liorfranko.github.io/finalizer"

// DefaultWeightForNewEndpoints represents the default weight assigned to new endpoints in a ServiceEntry.
const DefaultWeightForNewEndpoints uint32 = 1000

// IstioAdaptiveRequestOptimizerReconciler reconciles a IstioAdaptiveRequestOptimizer object
type IstioAdaptiveRequestOptimizerReconciler struct {
	client.Client
	Scheme                          *runtime.Scheme
	LoggerName                      string
	EndpointsAnnotationKey          *string
	EndpointsPodScrapeAnnotationKey *string
	ServiceEntryLabelKey            *string
	ServiceEntryServiceNameLabelKey *string
	NamespaceList                   []string
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
	logger.V(1).Info("Reconcile IstioAdaptiveRequestOptimizer", "IstioAdaptiveRequestOptimizer.Namespace", req.Namespace, "IstioAdaptiveRequestOptimizer.Name", req.Name)
	// Fetch the IstioAdaptiveRequestOptimizer instance
	optimizer := &optimizationv1alpha1.IstioAdaptiveRequestOptimizer{}
	if err := r.Get(ctx, req.NamespacedName, optimizer); err != nil {
		logger.Info("IstioAdaptiveRequestOptimizer not found", "Namespace", req.Namespace, "Name", req.Name)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.V(1).Info("IstioAdaptiveRequestOptimizer fetched", "IstioAdaptiveRequestOptimizer", optimizer.Spec)

	if optimizer.GetDeletionTimestamp() != nil {
		logger.Info("IstioAdaptiveRequestOptimizer marked for deletion", "IstioAdaptiveRequestOptimizer", optimizer.Name)
		_, err := r.handleFinalizer(ctx, optimizer)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// If the object is not marked for deletion and does not have a finalizer
	if !helpers.ContainsString(optimizer.GetFinalizers(), istioAdaptiveRequestOptimizerFinalizer) {
		// Attempt to add the finalizer
		logger.Info("Finalizer not found, adding finalizer")
		if err := r.addFinalizer(ctx, optimizer); err != nil {
			logger.Error(err, "Failed to add finalizer")
			// Return with error to requeue and try adding finalizer again
			return ctrl.Result{}, err
		}
	}

	// Fetch the Service based on the optimizer spec
	service := &corev1.Service{}
	if err := r.Get(ctx, client.ObjectKey{Name: optimizer.Spec.ServiceName, Namespace: optimizer.Spec.ServiceNamespace}, service); err != nil {
		logger.Error(err, "Failed to fetch Service", "ServiceName", optimizer.Spec.ServiceName)
		return ctrl.Result{}, err
	}
	logger.V(1).Info("Service fetched", "Service", service)

	// Fetch the EndpointSlices associated with the service
	var endpointSliceList discoveryv1.EndpointSliceList
	labelSelector := client.MatchingLabels{
		discoveryv1.LabelServiceName: service.Name,
	}
	if err := r.List(ctx, &endpointSliceList, client.InNamespace(service.Namespace), labelSelector); err != nil {
		logger.Error(err, "Failed to list EndpointSlices for Service", "ServiceName", service.Name)
		return ctrl.Result{}, err
	}

	// Annotate the EndpointSlices
	if err := r.annotateEndpointSlices(ctx, endpointSliceList.Items, optimizer); err != nil {
		logger.Error(err, "Failed to annotate EndpointSlices", "ServiceName", service.Name)
		return ctrl.Result{}, err
	}

	// Create ServiceEntry using EndpointSlices
	serviceEntry, err := r.createServiceEntry(ctx, service, endpointSliceList.Items, optimizer)
	if err != nil {
		logger.Error(err, "Failed to create ServiceEntry for port")
		// TODO: Add a metric for failed ServiceEntry creation
		return ctrl.Result{}, err
	}

	if err := r.updateOptimizerStatus(ctx, optimizer, serviceEntry); err != nil {
		// TODO: roman check why.
		logger.Error(err, "Failed to update optimizer status with service entries")
		return ctrl.Result{}, err
	}
	// Requeue reconciliation every 60 seconds to track new EndpointSlices.
	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
}

func (r *IstioAdaptiveRequestOptimizerReconciler) serviceEntryExists(ctx context.Context, namespace, name string) *istioClientV1.ServiceEntry {
	var serviceEntry istioClientV1.ServiceEntry
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &serviceEntry)
	if err != nil {
		return nil
	}
	return &serviceEntry
}

// createServiceEntry constructs a ServiceEntry resource based on the provided Service, ServicePort, and EndpointSlices.
// It then creates the ServiceEntry in the Kubernetes API server.
func (r *IstioAdaptiveRequestOptimizerReconciler) createServiceEntry(ctx context.Context, service *corev1.Service, endpointSlices []discoveryv1.EndpointSlice, opt *optimizationv1alpha1.IstioAdaptiveRequestOptimizer) (*istioClientV1.ServiceEntry, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	if existServiceEntry := r.serviceEntryExists(ctx, service.Namespace, service.Name); existServiceEntry != nil {
		//logger.Info("ServiceEntry already exists", "ServiceEntry", service.Name+"-"+fmt.Sprint(port.Port))
		return existServiceEntry, nil
	}
	host := fmt.Sprintf("%s.%s.svc.cluster.local", service.Name, service.Namespace)

	var serviceEntryEndpoints []*istioNetworkingV1.WorkloadEntry
	for _, endpointSlice := range endpointSlices {
		for _, endpoint := range endpointSlice.Endpoints {
			for _, address := range endpoint.Addresses {
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
	}
	// Construct the ServiceEntry resource
	serviceEntry := &istioClientV1.ServiceEntry{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "networking.istio.io/v1",
			Kind:       "ServiceEntry",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      service.Name,
			Namespace: service.Namespace,
			Labels: map[string]string{
				*r.ServiceEntryLabelKey:            "true",
				*r.ServiceEntryServiceNameLabelKey: service.Name,
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
			Hosts:      []string{host},
			Endpoints:  serviceEntryEndpoints,
			Location:   istioNetworkingV1.ServiceEntry_MESH_INTERNAL,
			Resolution: istioNetworkingV1.ServiceEntry_STATIC,
		},
	}
	serviceEntry.Spec.Ports = coreServicePortsToIstioServicePorts(service.Spec.Ports, serviceEntry.Spec.Ports[:0])

	// Create ServiceEntry resource
	logger.Info("Creating ServiceEntry", "ServiceEntry", serviceEntry.Name)
	err := r.Create(ctx, serviceEntry)
	if err != nil {
		logger.Error(err, "Failed to create ServiceEntry", "ServiceEntry", serviceEntry.Name)
		return nil, err
	}
	logger.Info("ServiceEntry created successfully", "ServiceEntry", serviceEntry.Name)
	return serviceEntry, nil
}

func coreServicePortsToIstioServicePorts(coreServicePorts []corev1.ServicePort, istioServicePorts []*istioNetworkingV1.ServicePort) []*istioNetworkingV1.ServicePort {
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

// handleFinalizer handles the finalizer logic for the IstioAdaptiveRequestOptimizer resource
func (r *IstioAdaptiveRequestOptimizerReconciler) handleFinalizer(ctx context.Context, optimizer *optimizationv1alpha1.IstioAdaptiveRequestOptimizer) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)

	// If the resource is marked for deletion
	if helpers.ContainsString(optimizer.GetFinalizers(), istioAdaptiveRequestOptimizerFinalizer) {
		if err := r.cleanupSpecificEndpointSliceAnnotations(ctx, optimizer, []string{*r.EndpointsAnnotationKey}); err != nil {
			if !errors.IsNotFound(err) {
				logger.Error(err, "Error cleaning up specific EndpointSlice annotations")
				// Return with error to requeue and try cleanup again
				return ctrl.Result{}, err
			}
		}

		if err := r.cleanupSpecificPodAnnotations(ctx, optimizer, []string{*r.EndpointsAnnotationKey, *r.EndpointsPodScrapeAnnotationKey}); err != nil {
			if !errors.IsNotFound(err) {
				logger.Error(err, "Error cleaning up specific Pod annotations")
				// Return with error to requeue and try cleanup again
				return ctrl.Result{}, err
			}
		}

		if err := r.removeFinalizer(ctx, optimizer); err != nil {
			if !errors.IsNotFound(err) {
				logger.Error(err, "Failed to remove finalizer")
				// Return with error to requeue and try cleanup again
				return ctrl.Result{}, err
			}
		}
	}
	// After finalizer is handled, no need to requeue
	return ctrl.Result{}, nil
}

// addFinalizer adds the finalizer to the IstioAdaptiveRequestOptimizer
func (r *IstioAdaptiveRequestOptimizerReconciler) addFinalizer(ctx context.Context, optimizer *optimizationv1alpha1.IstioAdaptiveRequestOptimizer) error {
	if !helpers.ContainsString(optimizer.GetFinalizers(), istioAdaptiveRequestOptimizerFinalizer) {
		logger := log.FromContext(ctx).WithName(r.LoggerName)
		logger.Info("Adding Finalizer for the IstioAdaptiveRequestOptimizer", "IstioAdaptiveRequestOptimizer", optimizer.Name)
		optimizer.SetFinalizers(append(optimizer.GetFinalizers(), istioAdaptiveRequestOptimizerFinalizer))
		// Update CR to add finalizer
		if err := r.Update(ctx, optimizer); err != nil {
			logger.Error(err, "Failed to add finalizer to IstioAdaptiveRequestOptimizer", "IstioAdaptiveRequestOptimizer", optimizer.Name)
			return err
		}
	}
	return nil
}

// removeFinalizer removes the finalizer from the IstioAdaptiveRequestOptimizer
func (r *IstioAdaptiveRequestOptimizerReconciler) removeFinalizer(ctx context.Context, optimizer *optimizationv1alpha1.IstioAdaptiveRequestOptimizer) error {
	if helpers.ContainsString(optimizer.GetFinalizers(), istioAdaptiveRequestOptimizerFinalizer) {
		logger := log.FromContext(ctx).WithName(r.LoggerName)
		logger.Info("Removing Finalizer for the IstioAdaptiveRequestOptimizer", "IstioAdaptiveRequestOptimizer", optimizer.Name)
		optimizer.SetFinalizers(helpers.RemoveString(optimizer.GetFinalizers(), istioAdaptiveRequestOptimizerFinalizer))

		// Update CR to remove the finalizer
		if err := r.Update(ctx, optimizer); err != nil {
			logger.Error(err, "Failed to remove finalizer from IstioAdaptiveRequestOptimizer", "IstioAdaptiveRequestOptimizer", optimizer.Name)
			return err
		}
	}
	return nil
}

// normalizeProtocol converts high-level protocol names to their underlying transport protocol.
func normalizeProtocol(protocol string) string {
	lowerProtocol := strings.ToLower(protocol)
	switch lowerProtocol {
	case "http", "grpc":
		return "tcp" // Treat both HTTP and gRPC as TCP, in lowercase
	default:
		return lowerProtocol // Return the protocol in lowercase if not HTTP or gRPC
	}
}

// annotateEndpointSlices adds the specified annotation to all EndpointSlices associated with the service.
func (r *IstioAdaptiveRequestOptimizerReconciler) annotateEndpointSlices(ctx context.Context, endpointSlices []discoveryv1.EndpointSlice, optimizer *optimizationv1alpha1.IstioAdaptiveRequestOptimizer) error {
	// Skip if the optimizer is marked for deletion
	if optimizer.GetDeletionTimestamp() != nil {
		return nil
	}

	if len(optimizer.Spec.ServicePorts) > 0 {
		for _, endpointSlice := range endpointSlices {
			// Add the optimizer annotation to the EndpointSlice object
			if endpointSlice.Annotations == nil {
				endpointSlice.Annotations = make(map[string]string)
			}
			endpointSlice.Annotations[*r.EndpointsAnnotationKey] = "true"

			// Update the EndpointSlice object with the new annotations
			if err := r.Update(ctx, &endpointSlice); err != nil {
				return err
			}
		}
	}
	return nil
}

// cleanupSpecificEndpointSliceAnnotations removes specified annotations from all EndpointSlices associated with the service.
func (r *IstioAdaptiveRequestOptimizerReconciler) cleanupSpecificEndpointSliceAnnotations(ctx context.Context, optimizer *optimizationv1alpha1.IstioAdaptiveRequestOptimizer, annotationKeysToRemove []string) error {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	logger.Info("Cleaning up specific EndpointSlice annotations for IstioAdaptiveRequestOptimizer", "IstioAdaptiveRequestOptimizer", optimizer.Name)

	// Fetch the EndpointSlices associated with the service
	var endpointSliceList discoveryv1.EndpointSliceList
	labelSelector := client.MatchingLabels{
		discoveryv1.LabelServiceName: optimizer.Spec.ServiceName,
	}
	if err := r.List(ctx, &endpointSliceList, client.InNamespace(optimizer.Spec.ServiceNamespace), labelSelector); err != nil {
		logger.Error(err, "Failed to list EndpointSlices for Service", "ServiceName", optimizer.Spec.ServiceName)
		return err
	}

	for _, endpointSlice := range endpointSliceList.Items {
		modified := false
		// Iterate over the list of annotation keys that need to be removed
		for _, key := range annotationKeysToRemove {
			if _, found := endpointSlice.Annotations[key]; found {
				delete(endpointSlice.Annotations, key)
				modified = true
			}
		}

		// Update the EndpointSlice object to reflect the changes, if any annotation was removed
		if modified {
			err := r.Update(ctx, &endpointSlice)
			if err != nil {
				logger.Error(err, "Failed to update EndpointSlice after removing specific annotations", "EndpointSlice", endpointSlice.Name, "keysRemoved", annotationKeysToRemove)
				// If you want to handle errors in a specific way, do it here.
				return err
			}
			logger.V(1).Info("Specific annotations removed from EndpointSlice", "EndpointSlice", endpointSlice.Name, "keysRemoved", annotationKeysToRemove)
		}
	}

	return nil
}

func (r *IstioAdaptiveRequestOptimizerReconciler) cleanupSpecificPodAnnotations(ctx context.Context, optimizer *optimizationv1alpha1.IstioAdaptiveRequestOptimizer, annotationKeysToRemove []string) error {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	logger.Info("Initiating cleanup of specific Pod annotations", "IstioAdaptiveRequestOptimizer", optimizer.Name)
	logger.Info("Preparing to remove Prometheus metrics for the service", "service_name", optimizer.Spec.ServiceName, "service_namespace", optimizer.Spec.ServiceNamespace)

	// Define Prometheus metrics to be removed
	metricsToRemove := []*prometheus.GaugeVec{
		customMetrics.WeightMetric,
		// Extend with additional metrics as necessary
	}

	// Retrieve the Service to access its Pod selector
	service := &corev1.Service{}
	if err := r.Get(ctx, client.ObjectKey{Name: optimizer.Spec.ServiceName, Namespace: optimizer.Spec.ServiceNamespace}, service); err != nil {
		logger.Error(err, "Failed to retrieve the Service", "ServiceName", optimizer.Spec.ServiceName)
		return err
	}

	// List all Pods in the namespace that match the Service's selector
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(optimizer.Spec.ServiceNamespace), client.MatchingLabels(service.Spec.Selector)); err != nil {
		logger.Error(err, "Failed to list matching Pods for the Service", "ServiceName", optimizer.Spec.ServiceName)
		return err
	}

	// Iterate through each Pod to remove metrics and annotations
	for _, pod := range podList.Items {
		// Remove Prometheus metrics associated with the Pod
		for _, metricVec := range metricsToRemove {
			if !metricVec.Delete(prometheus.Labels{"service_name": optimizer.Spec.ServiceName, "service_namespace": optimizer.Spec.ServiceNamespace, "pod_name": pod.Name, "pod_ip": pod.Status.PodIP}) {
				logger.Info("No Prometheus metrics found for removal", "service_name", optimizer.Spec.ServiceName, "service_namespace", optimizer.Spec.ServiceNamespace)
				continue
			}
			logger.Info("Prometheus metrics successfully removed", "service_name", optimizer.Spec.ServiceName, "service_namespace", optimizer.Spec.ServiceNamespace, "metric_name", metricVec)
		}

		// Remove specified annotations from the Pod
		modified := false
		for _, key := range annotationKeysToRemove {
			if _, found := pod.Annotations[key]; found {
				delete(pod.Annotations, key)
				modified = true
			}
		}

		// Update the Pod if any annotations were removed
		if modified {
			if err := r.Update(ctx, &pod); err != nil {
				logger.Error(err, "Failed to update Pod after annotation removal", "PodName", pod.Name)
				continue // Proceed with the next Pod instead of halting
			}
			logger.V(1).Info("Removed specified annotations from Pod", "PodName", pod.Name, "keysRemoved", annotationKeysToRemove)
		}
	}
	return nil
}

func (r *IstioAdaptiveRequestOptimizerReconciler) updateOptimizerStatus(ctx context.Context, optimizer *optimizationv1alpha1.IstioAdaptiveRequestOptimizer, serviceEntry *istioClientV1.ServiceEntry) error {
	optimizer.Status.ServiceEntries = []optimizationv1alpha1.ServiceEntry{{
		Name:         serviceEntry.Name,
		Namespace:    serviceEntry.Namespace,
		CreationTime: serviceEntry.CreationTimestamp,
	}} // TODO(romang): change ServiceEntries to ServiceEntry
	return r.Status().Update(ctx, optimizer)
}

// SetupWithManager sets up the controller with the Manager.
func (r *IstioAdaptiveRequestOptimizerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	namespacePredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return helpers.NamespaceInFilteredList(obj.GetNamespace(), r.NamespaceList)
	})
	// ignore status changes
	statusPredicate := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&optimizationv1alpha1.IstioAdaptiveRequestOptimizer{}).
		WithEventFilter(namespacePredicate).
		WithEventFilter(statusPredicate).
		Complete(r)
}
