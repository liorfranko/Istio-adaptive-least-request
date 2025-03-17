package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"github.com/montanaflynn/stats"
	"github.com/prometheus/client_golang/prometheus"
	istioNetworkingV1 "istio.io/api/networking/v1"
	istioClientV1 "istio.io/client-go/pkg/apis/networking/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	istioapinetworkingv1 "istio.io/api/networking/v1"
	istionetworkingv1 "istio.io/client-go/pkg/apis/networking/v1"
	clientPkg "sigs.k8s.io/controller-runtime/pkg/client"

	api "istio-adaptive-least-request/api/v1alpha1"
	"istio-adaptive-least-request/internal/helpers"
	"istio-adaptive-least-request/internal/metrics"
)

// DefaultWeightForNewEndpoints represents the default weight assigned to new endpoints in a ServiceEntry.
const DefaultWeightForNewEndpoints uint32 = 1000

// IstioAdaptiveRequestOptimizerReconciler reconciles a IstioAdaptiveRequestOptimizer object
type IstioAdaptiveRequestOptimizerReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	LoggerName      string
	NamespaceList   []string
	RequeueAfter    time.Duration
	QueryInterval   string
	VmdbUrl         string
	StepInterval    string
	ScaleupFactor   float64
	ScaledownFactor float64
	MinimumWeight   int
	InitialWeight   int
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
		getPodMetricsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		podAddressToPodMetrics, err := getPodMetrics(
			getPodMetricsCtx,
			logger,
			opt.Namespace,
			&serviceEntry,
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
		distributeWeightsBasedOnCPU(
			logger,
			podAddressToPodMetrics,
			&serviceEntry,
			r.ScaleupFactor,
			r.ScaledownFactor,
			r.MinimumWeight,
		)
		logger.Info("Trying to update ServiceEntry", "ServiceEntry", serviceEntry.Name)
		if err := r.Update(ctx, &serviceEntry); err != nil {
			logger.Error(err, "Failed to validate or update weights.")
			return ctrl.Result{}, err
		}
		logger.Info("Successfully updated ServiceEntry", "ServiceEntry", serviceEntry.Name)
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

type tVmDBItem struct {
	Metric struct {
		PodAddress string `json:"pod_ip"`
	} `json:"metric"`
	Value []any `json:"value"`
}

// getVMQueryMetric queries VictoriaMetrics for the given service and protocol and returns the response.
func getPodMetrics(
	ctx context.Context,
	logger logr.Logger,
	namespace string,
	serviceEntry *istioClientV1.ServiceEntry,
	queryInterval string,
	vmDbUrl string,
	stepInterval string,
) (map[string]float64, error) {
	unfilteredPodAddressToCPUTime, err := getCPUMetrics(
		ctx,
		logger,
		serviceEntry.Name,
		namespace,
		queryInterval,
		vmDbUrl,
		stepInterval,
	)
	if err != nil {
		return nil, err
	}
	endpoints := serviceEntry.Spec.Endpoints
	podAddressToCPUTime := make(map[string]float64, len(endpoints))
	cpuTimesSum := 0.0
	cpuTimeLen := 0
	for _, workloadEntry := range endpoints {
		address := workloadEntry.Address
		cpuTime, ok := unfilteredPodAddressToCPUTime[address]
		if ok && cpuTime > 0.0 {
			cpuTimesSum += cpuTime
			cpuTimeLen++
		}
		podAddressToCPUTime[address] = cpuTime
	}
	avgCPU := cpuTimesSum / float64(cpuTimeLen)
	for address, cpuTime := range podAddressToCPUTime {
		if cpuTime == 0.0 {
			podAddressToCPUTime[address] = avgCPU
		}
	}
	return podAddressToCPUTime, nil
}

func getCPUMetrics(
	ctx context.Context,
	logger logr.Logger,
	service string,
	namespace string,
	queryInterval string,
	vmDbUrl string,
	stepInterval string,
) (map[string]float64, error) {
	logger.Info("Querying CPU from VictoriaMetrics for service",
		"service.name", service,
		"service.namespace", namespace,
	)
	query := fmt.Sprintf(
		`sum(rate(container_cpu_usage_seconds_total{namespace="%s",container="%s"}[%s]) * on(pod) group_left(pod_ip) (kube_pod_info)) by (pod_ip)`,
		namespace,
		service,
		queryInterval,
	)
	logger.V(1).Info("query", "query", query)
	// Start timer
	startTime := time.Now()
	cpuTimes, err := getVMCPUQueryMetric(ctx, logger, query, vmDbUrl, stepInterval)
	if err != nil {
		return nil, err
	}
	// Measure elapsed time
	elapsedTime := time.Since(startTime).Seconds() // in seconds
	queryLabels := prometheus.Labels{
		"service_name":      service,
		"service_namespace": namespace,
	}
	metrics.QueryLatencyMetric.With(queryLabels).Set(elapsedTime)
	podAddressToCPUTime := make(map[string]float64)
	for i := range cpuTimes {
		v := &cpuTimes[i]
		if len(v.Value) < 2 {
			return nil, fmt.Errorf("error parsing ResponseTime for pod %v: %w", v.Metric.PodAddress, err)
		}
		cpuTime, err := strconv.ParseFloat(v.Value[1].(string), 64)
		if err != nil {
			return nil, fmt.Errorf("error parsing ResponseTime for pod %v: %w", v.Metric.PodAddress, err)
		}
		podAddress := v.Metric.PodAddress
		podAddressToCPUTime[podAddress] = cpuTime
	}
	return podAddressToCPUTime, nil
}

func closeDeferred(logger logr.Logger, closer io.Closer) {
	if err := closer.Close(); err != nil {
		logger.Error(err, "Failed to close the closer")
	}
}

func getVMCPUQueryMetric(
	ctx context.Context,
	logger logr.Logger,
	query string,
	vmDbUrl string,
	stepInterval string,
) ([]tVmDBItem, error) {
	query = url.QueryEscape(query)
	apiURL := fmt.Sprintf("%s/prometheus/api/v1/query?query=%s&step=%s", vmDbUrl, query, stepInterval)
	logger.V(1).Info("Querying VictoriaMetrics", "apiURL", apiURL)
	// Create an HTTP client and make the request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer closeDeferred(logger, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	var vmDBRes struct {
		Data struct {
			Result []tVmDBItem `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&vmDBRes); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	logger.V(1).Info("Response from VictoriaMetrics", "vmDBRes", vmDBRes)
	return vmDBRes.Data.Result, nil
}

func fallbackStrategy(
	ctx context.Context,
	logger logr.Logger,
	client clientPkg.Client,
	opt *api.IstioAdaptiveRequestOptimizer,
	serviceEntry *istionetworkingv1.ServiceEntry,
	resetWeight uint32,
) error {
	logger.Info("Initiating fallback strategy check")
	if shouldSkipFallback(opt) {
		logger.Info("Recent optimization detected; skipping fallback strategy")
		return nil
	}
	if err := resetWeights(ctx, logger, client, serviceEntry, resetWeight); err != nil {
		return err
	}
	logger.Info("Weights reset to default due to timeout")
	return nil
}

// Helper function to decide whether to skip fallback based on optimization times.
func shouldSkipFallback(opt *api.IstioAdaptiveRequestOptimizer) bool {
	optimizedTime := opt.Status.LastOptimizedTime
	if optimizedTime == nil {
		return false
	}
	return time.Since(optimizedTime.Time) < 10*time.Minute
}

func resetWeights(
	ctx context.Context,
	logger logr.Logger,
	client clientPkg.Client,
	serviceEntry *istionetworkingv1.ServiceEntry,
	resetWeight uint32,
) error {
	logger.Info("Resetting weights to default values", "serviceEntry", serviceEntry.Name)
	for _, workloadEntry := range serviceEntry.Spec.Endpoints {
		workloadEntry.Weight = resetWeight
	}
	logger.Info("Trying to update serviceEntry", "serviceEntry", serviceEntry.Name)
	if err := client.Update(ctx, serviceEntry); err != nil {
		logger.Error(err, "Failed to update serviceEntry", "serviceEntry", serviceEntry.Name)
		return err
	}
	updateMetrics(serviceEntry)
	logger.Info("ServiceEntry updated successfully", "serviceEntry", serviceEntry.Name)
	return nil
}

func distributeWeightsBasedOnCPU(
	logger logr.Logger,
	podAddressToCPUTime map[string]float64,
	serviceEntry *istionetworkingv1.ServiceEntry,
	scaleupFactor float64,
	scaledownFactor float64,
	minimumWeight int,
) {
	podAddressToWorkloadEntry := make(map[string]*istioapinetworkingv1.WorkloadEntry)
	podAddressToLocality := make(map[string]string)
	for _, workloadEntry := range serviceEntry.Spec.Endpoints {
		podAddressToWorkloadEntry[workloadEntry.Address] = workloadEntry
		podAddressToLocality[workloadEntry.Address] = workloadEntry.Locality
	}
	type tPodCPUTime struct {
		address string
		cpuTime float64
	}
	groups := make(map[string][]tPodCPUTime, len(podAddressToCPUTime))
	for address := range podAddressToWorkloadEntry {
		locality := podAddressToLocality[address]
		cpuTime := podAddressToCPUTime[address]
		groups[locality] = append(groups[locality], tPodCPUTime{
			address: address,
			cpuTime: cpuTime,
		})
	}

	// Process each locality group
	for localityKey, groupPods := range groups {
		logger.V(4).Info("Processing group", "localityKey", localityKey, "groupPods", groupPods)

		// Calculate group total weight
		var groupTotalWeight float64
		for i := range groupPods {
			podCPUTime := &groupPods[i]
			address := podCPUTime.address
			groupTotalWeight += float64(podAddressToWorkloadEntry[address].Weight)
		}

		// Check if group total weight is below minimum threshold
		minGroupTotal := float64(len(groupPods)) * 200.0
		if groupTotalWeight < minGroupTotal {
			for _, pm := range groupPods {
				podAddressToWorkloadEntry[pm.address].Weight *= 5
			}
			continue
		}

		// Calculate average CPU for the group
		var cpuTimes []float64
		for i := range groupPods {
			podCPUTime := &groupPods[i]
			cpuTime := podCPUTime.cpuTime
			cpuTimes = append(cpuTimes, cpuTime)
		}
		avgCPU, _ := stats.Mean(cpuTimes)
		if avgCPU < 0.20 {
			// Set all to maximum if insufficient data
			for i := range groupPods {
				podCPUTime := &groupPods[i]
				address := podCPUTime.address
				podAddressToWorkloadEntry[address].Weight = 1000
			}
			continue
		}
		// Calculate X_sum for the "cake" formula
		var xSum float64
		for _, pm := range groupPods {
			if pm.cpuTime > 0 {
				xSum += avgCPU / pm.cpuTime * float64(podAddressToWorkloadEntry[pm.address].Weight)
			}
		}

		// Adjust weights for each pod in the group
		avgGroupWeight := groupTotalWeight / float64(len(groupPods))
		for _, pm := range groupPods {
			currentWeight := float64(podAddressToWorkloadEntry[pm.address].Weight)
			if currentWeight == 0 {
				// TODO: its hack we need change the iteration to be based on the serviceEntryWeightsMap in next version.
				logger.Info("Weight is 0, skipping",
					"address", pm.address,
				)
				podAddressToWorkloadEntry[pm.address].Weight = uint32(minimumWeight)
				continue
			}
			newShare := (avgCPU / pm.cpuTime) * (currentWeight / xSum) * groupTotalWeight
			rawDistance := newShare - currentWeight
			scalingFactor := scaleupFactor
			if rawDistance < 0 {
				scalingFactor = scaledownFactor
			}
			adjustedWeight := currentWeight + scalingFactor*(newShare-currentWeight)

			// Cap big spikes
			distance := adjustedWeight - currentWeight
			//maxAllowedDistance := avgGroupWeight / 4
			maxAllowedDistance := 250.0
			if distance > maxAllowedDistance {
				adjustedWeight = currentWeight + maxAllowedDistance
			}
			if adjustedWeight < float64(minimumWeight) {
				logger.Info("Adjusted weight is less than minimumWeight, setting it to minimumWeight Weight optimization",
					"address", pm.address,
					"cpuTime", pm.cpuTime,
					"Weight", podAddressToWorkloadEntry[pm.address].Weight,
					"NewShare", newShare,
					"AdjustedWeight", adjustedWeight,
					"Distance", distance,
					"MaxAllowedDistance", maxAllowedDistance,
					"avgCPU", avgCPU,
					"xSum", xSum,
					"scaleupFactor", scaleupFactor,
					"scaledownFactor", scaledownFactor,
					"avgGroupWeight", avgGroupWeight,
					"currentWeight", currentWeight,
					"minimumWeight", minimumWeight,
				)
				adjustedWeight = float64(minimumWeight)
			}

			podAddressToWorkloadEntry[pm.address].Weight = uint32(adjustedWeight)
		}
		// Normalize weights to keep the average weight equal to 1000
		var totalWeight float64
		for _, podMetrics := range groupPods {
			totalWeight += float64(podAddressToWorkloadEntry[podMetrics.address].Weight)
		}
		normalizationFactor := (1000 * float64(len(groupPods))) / totalWeight
		for _, podMetrics := range groupPods {
			weight := uint32(float64(podAddressToWorkloadEntry[podMetrics.address].Weight) * normalizationFactor)
			podAddressToWorkloadEntry[podMetrics.address].Weight = weight
		}

	}
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
