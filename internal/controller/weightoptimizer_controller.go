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
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/montanaflynn/stats"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"istio-adaptive-least-request/internal/helpers"
	customMetrics "istio-adaptive-least-request/internal/metrics"

	istioapinetworkingv1 "istio.io/api/networking/v1"
	istionetworkingv1 "istio.io/client-go/pkg/apis/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	optimizationv1alpha1 "istio-adaptive-least-request/api/v1alpha1"
)

// WeightOptimizerReconciler reconciles a WeightOptimizer object
type WeightOptimizerReconciler struct {
	client.Client
	Scheme                        *runtime.Scheme
	LoggerName                    string
	VmdbUrl                       *string
	NamespaceList                 []string
	RequeueAfter                  time.Duration
	MinimumWeight                 int
	MaximumWeight                 int
	QueryInterval                 string
	StepInterval                  string
	MinOptimizeCpuDistancePercent float64
	CpuDistanceMultiplierPercent  float64
	ScaleupFactor                 float64
	ScaledownFactor               float64
}

type VmdbCPURespone struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric struct {
				Pod string `json:"pod"`
			} `json:"metric"`
			Value []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

type PodMetrics struct {
	PodName    string  `json:"podName"`
	PodAddress string  `json:"podAddress"`
	CPUTime    float64 `json:"cpuTime"`
}

type PodCPUMetrics struct {
	CPUTime float64 `json:"average"`
}

type PodInfo struct {
	PodAddress string
	PodName    string
	CPUTime    float64
}

// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=weightoptimizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=weightoptimizers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=weightoptimizers/finalizers,verbs=update
//+kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=serviceentries,verbs=get;list;watch
//+kubebuilder:rbac:groups="core",resources=pods,verbs=get;list;watch

func (r *WeightOptimizerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	logger.V(1).Info("Reconcile WeightOptimizer", "WeightOptimizer.Namespace", req.Namespace, "WeightOptimizer.Name", req.Name)
	var opt optimizationv1alpha1.IstioAdaptiveRequestOptimizer
	if err := r.Get(ctx, req.NamespacedName, &opt); err != nil {
		logger.Info("IstioLatencyOptimizer not found", "Namespace", req.Namespace, "Name", req.Name)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.V(1).Info("IstioAdaptiveRequestOptimizer fetched", "IstioAdaptiveRequestOptimizer", opt.Spec)
	if opt.GetDeletionTimestamp() != nil {
		// We've got an update event that indicate the instance is being deleted, the event it update by this controller doesn't need to do anything with it as the IstioLatencyOptimizer handle the deltion
		logger.Info("IstioLatencyOptimizer is being deleted", "Namespace", opt.Namespace, "Name", opt.Name)
		return ctrl.Result{}, nil
	}
	var serviceEntry istionetworkingv1.ServiceEntry
	if err := r.Get(ctx, req.NamespacedName, &serviceEntry); err != nil {
		logger.Error(err, "ServiceEntry not exists, continue to the next port if there is", "ServiceEntry", serviceEntry.Name)
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "fetch_service_entry", "name": opt.Name, "namespace": opt.Namespace}).Inc()
		return ctrl.Result{}, err
	}
	logger.V(1).Info("Fetched ServiceEntry", "ServiceEntry", serviceEntry.Name)
	if len(serviceEntry.Spec.Endpoints) == 0 {
		logger.Info("ServiceEntry doesn't have any endpoints, continue", "ServiceEntry", serviceEntry.Name)
		return ctrl.Result{RequeueAfter: r.RequeueAfter * time.Second}, nil
	}
	labelsForPods := labels.SelectorFromSet(labels.Set{
		"service.istio.io/canonical-name": opt.Spec.ServiceName,
	})
	podsInfo, err := r.listPods(ctx, opt.Namespace, labelsForPods)
	if err != nil {
		logger.Error(err, "Failed to list Pods", "Namespace", opt.Namespace)
		return ctrl.Result{}, err
	}
	//logger.V(1).Info("Pods fetched", "Pods", podsInfo)
	// Get the metrics from VictoriaMetrics for the service and protocol
	getPodMetricsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	podsMetrics, err := r.getPodMetrics(getPodMetricsCtx, opt.Name, opt.Namespace, podsInfo)
	if err != nil {
		// If there is a problem with pulling the metrics from VictoriaMetrics, log an error and continue to the next port
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "get_metrics_from_vm", "name": opt.Name, "namespace": opt.Namespace}).Inc()
		if err := r.fallbackStrategy(ctx, &opt, &serviceEntry); err != nil {
			customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "fallback_strategy", "name": opt.Name, "namespace": opt.Namespace}).Inc()
			return ctrl.Result{}, err
		}
		logger.Info("continue to the next port if there is", "service.Name", opt.Name)
		return ctrl.Result{}, err
	}
	r.updatePodMetrics(ctx, &podsMetrics) // TODO(romang): this func really do nothing
	r.distributeWeightsBasedOnCPU(ctx, podsMetrics, &serviceEntry)
	if err := r.Update(ctx, &serviceEntry); err != nil {
		logger.Error(err, "Failed to validate or update weights.")
		return ctrl.Result{}, err
	}

	status := &opt.Status
	now := metav1.Now()
	status.LastOptimizedTime = &now
	status.ObservedGeneration = opt.Generation
	if err := r.Client.Update(ctx, &opt); err != nil {
		logger.Error(err, "Failed to update IstioAdaptiveRequestOptimizer")
		return ctrl.Result{}, err
	}

	// Requeue to process services periodically
	return ctrl.Result{RequeueAfter: r.RequeueAfter * time.Second}, nil
}

// getVMQueryMetric queries VictoriaMetrics for the given service and protocol and returns the response.
func (r *WeightOptimizerReconciler) getPodMetrics(ctx context.Context, service string, namespace string, podsInfo []PodInfo) (map[string]*PodMetrics, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	cpuMetrics, err := r.getCPUMetrics(ctx, service, namespace)
	logger.V(1).Info("cpuMetrics", "cpuMetrics", cpuMetrics)
	if err != nil {
		return nil, err
	}
	if len(cpuMetrics) == 0 {
		return nil, fmt.Errorf("no results when getting cpu usage for service %s", service)
	}
	// Initialize a map to store PodInfo pointers.
	podInfoMap := make(map[string]*PodInfo)
	for i := range podsInfo {
		podInfoMap[podsInfo[i].PodName] = &podsInfo[i]
	}
	podMetricsMap := map[string]*PodMetrics{}

	// Update each pod's CPU time if metrics are available for it.
	for podName, podInfo := range podInfoMap {
		if cpuMetric, exists := cpuMetrics[podName]; exists {
			podMetricsMap[podInfo.PodAddress] = &PodMetrics{
				PodName:    podName,
				PodAddress: podInfo.PodAddress,
				CPUTime:    cpuMetric.CPUTime,
			}
			podInfo.CPUTime = cpuMetric.CPUTime
			logger.V(1).Info("Updated pod with CPU metrics", "podName", podName, "cpuTime", cpuMetric.CPUTime)
		}
	}

	return podMetricsMap, nil
}

func (r *WeightOptimizerReconciler) getCPUMetrics(ctx context.Context, service string, namespace string) (map[string]*PodCPUMetrics, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	logger.Info("Querying CPU from VictoriaMetrics for service", "service.name", service, "service.namespace", namespace)
	queryPattern := fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{namespace="%s",container="%s"}[%s])) by (pod)`, namespace, service, r.QueryInterval)
	logger.V(1).Info("queryPattern", "queryPattern", queryPattern)
	query := url.QueryEscape(queryPattern)
	// Start timer
	startTime := time.Now()
	CpuTime, err := r.getVMCPUQueryMetric(ctx, query)
	if err != nil {
		return map[string]*PodCPUMetrics{}, err
	}
	// Measure elapsed time
	elapsedTime := time.Since(startTime).Seconds() // in seconds
	queryLabels := prometheus.Labels{
		"service_name":      service,
		"service_namespace": namespace,
	}
	customMetrics.QueryLatencyMetric.With(queryLabels).Set(elapsedTime)
	// Then process the response to return a slice of `PodMetrics`. Assume `response` is what you got from VictoriaMetrics.
	podCPUMetrics := make(map[string]*PodCPUMetrics)
	for _, v := range CpuTime.Data.Result {
		cpuTimeStr := strings.Trim(string(v.Value[1]), `"`)
		CPUTime, err := strconv.ParseFloat(cpuTimeStr, 64)
		podName := v.Metric.Pod
		if err != nil {
			return map[string]*PodCPUMetrics{}, fmt.Errorf("error parsing ResponseTime for pod %v: %w", v.Metric.Pod, err)
		}
		podCPUMetrics[podName] = &PodCPUMetrics{
			CPUTime: CPUTime,
		}
	}
	return podCPUMetrics, nil
}

func (r *WeightOptimizerReconciler) getVMCPUQueryMetric(ctx context.Context, query string) (VmdbCPURespone, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	// Define query pattern based on protocol
	apiURL := fmt.Sprintf("%s/prometheus/api/v1/query?query=%s&step=%s", *r.VmdbUrl, query, r.StepInterval)
	logger.V(1).Info("Querying VictoriaMetrics", "apiURL", apiURL)
	// Create an HTTP client and make the request
	httpClient := &http.Client{}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return VmdbCPURespone{}, fmt.Errorf("creating request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return VmdbCPURespone{}, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	// Check the response status code
	if resp.StatusCode != http.StatusOK {
		return VmdbCPURespone{}, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	var vmdbRespone VmdbCPURespone
	// Decode the response body
	if err := json.NewDecoder(resp.Body).Decode(&vmdbRespone); err != nil {
		return VmdbCPURespone{}, fmt.Errorf("decoding response: %w", err)
	}
	logger.V(1).Info("Response from VictoriaMetrics", "vmdbRespone", vmdbRespone)

	return vmdbRespone, nil
}

func (r *WeightOptimizerReconciler) listPods(ctx context.Context, namespace string, selector labels.Selector) ([]PodInfo, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	podList := &corev1.PodList{}
	listOpts := &client.ListOptions{Namespace: namespace, LabelSelector: selector}
	if err := r.Client.List(ctx, podList, listOpts); err != nil {
		return nil, err
	}

	var podsInfo []PodInfo
	for _, pod := range podList.Items {
		// Skip pods that are being deleted
		if pod.DeletionTimestamp != nil {
			logger.V(1).Info("Skipping pod: marked for deletion", "podName", pod.Name)
			continue
		}

		// Check if pod is ready
		isReady := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				isReady = true
				break
			}
		}

		if !isReady {
			logger.V(1).Info("Skipping pod: not ready", "podName", pod.Name)
			continue
		}

		if pod.Status.PodIP == "" {
			logger.V(1).Info("Skipping pod: no IP address", "podName", pod.Name)
			continue
		}

		podsInfo = append(podsInfo, PodInfo{
			PodAddress: pod.Status.PodIP,
			PodName:    pod.Name,
			CPUTime:    0.0,
		})
		logger.V(1).Info("Added pod to list", "podName", pod.Name, "podIP", pod.Status.PodIP)
	}

	logger.Info("Finished listing pods", "totalPods", len(podList.Items), "readyPods", len(podsInfo))
	return podsInfo, nil
}

func (r *WeightOptimizerReconciler) fallbackStrategy(ctx context.Context, opt *optimizationv1alpha1.IstioAdaptiveRequestOptimizer, serviceEntry *istionetworkingv1.ServiceEntry) error {
	logger := log.FromContext(ctx).WithName(r.LoggerName).WithValues("service.name", opt.Name, "service.namespace", opt.Namespace)
	logger.Info("Initiating fallback strategy check")
	if shouldSkipFallback(opt) {
		logger.Info("Recent optimization detected; skipping fallback strategy")
		return nil
	}
	if err := r.resetWeights(ctx, serviceEntry); err != nil {
		return err
	}
	logger.Info("Weights reset to default due to timeout")
	return nil
}

// Helper function to decide whether to skip fallback based on optimization times.
func shouldSkipFallback(opt *optimizationv1alpha1.IstioAdaptiveRequestOptimizer) bool {
	optimizedTime := opt.Status.LastOptimizedTime
	if optimizedTime == nil {
		return false
	}
	return time.Since(optimizedTime.Time) < 5*time.Minute
}

// Reset weights to default values and update the WeightOptimizer.
func (r *WeightOptimizerReconciler) resetWeights(ctx context.Context, serviceEntry *istionetworkingv1.ServiceEntry) error {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	for _, workloadEntry := range serviceEntry.Spec.Endpoints {
		logger.Info("Resetting endpoint to default values", "endpoint", workloadEntry.Address)
		workloadEntry.Weight = 300
	}
	if err := r.Update(ctx, serviceEntry); err != nil {
		logger.Error(err, "Failed to update serviceEntry")
		return err
	}
	return nil
}

func (r *WeightOptimizerReconciler) updatePodMetrics(ctx context.Context, podsMetrics *map[string]*PodMetrics) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	if len(*podsMetrics) == 0 {
		return
	}
	var cpuTimes []float64
	for _, cpuMetric := range *podsMetrics {
		if cpuMetric.CPUTime != 0.0 {
			cpuTimes = append(cpuTimes, cpuMetric.CPUTime)
		}
	}
	averageCPU, _ := stats.Mean(cpuTimes)
	standardDeviationCPU, _ := stats.StdDevP(cpuTimes)
	logger.V(4).Info("averageCPU", "averageCPU", averageCPU)
	logger.V(4).Info("standardDeviationCPU", "standardDeviationCPU", standardDeviationCPU)
	for _, ep := range *podsMetrics {
		if ep.CPUTime == 0.0 {
			// Pods without CPU metrics will have the averageCPU, and are not optimized
			ep.CPUTime = averageCPU
		}
		cpuDistance := ep.CPUTime - averageCPU
		logger.V(4).Info("CPU Distance", "CPU Distance", cpuDistance)
	}
}

func (r *WeightOptimizerReconciler) getWeightOptimizerMap(weightOptimizer *optimizationv1alpha1.WeightOptimizer) map[string]optimizationv1alpha1.Endpoint {
	weightsMap := make(map[string]optimizationv1alpha1.Endpoint)
	for _, ep := range weightOptimizer.Spec.Endpoints {
		weightsMap[ep.IP] = ep
	}
	return weightsMap
}

func (r *WeightOptimizerReconciler) assignNewWeights(ctx context.Context, podsMetrics map[string]*PodMetrics, newWeights map[string]uint32, namespace string, weightOptimizer *optimizationv1alpha1.WeightOptimizer, serviceEntryLocalityMap map[string]string) error {
	logger := log.FromContext(ctx).WithName(r.LoggerName)

	// create a map of the weights of the endpoints from the WeightOptimizer
	weightOptimizerMap := r.getWeightOptimizerMap(weightOptimizer)

	weightsMap := make(map[string]uint32)
	var filteredEndpoints []optimizationv1alpha1.Endpoint
	for _, result := range podsMetrics {
		newWeight, ok := newWeights[result.PodAddress]
		if !ok {
			logger.Info("Endpoint exists in VictoriaMetrics response but not found in serviceEntryWeightsMap - don't optimize it and continue", "IP", result.PodAddress)
			continue
		}
		logger.V(1).Info("newWeight status", "IP", result.PodAddress, "CurrentWeight", newWeight)
		// Check if the endpoint exists in the WeightOptimizer
		weightOptimizerEndpoint, ok := weightOptimizerMap[result.PodAddress]
		if !ok {
			logger.Info("Endpoint exists in VictoriaMetrics response but not found in WeightOptimizer - adding it", "IP", result.PodAddress)
			if newWeights[result.PodAddress] == 0 {
				logger.Info("Weight is 0, skipping", "PodAddress", result.PodAddress, "PodName", result.PodName)
				continue
			}
			weightOptimizerEndpoint = optimizationv1alpha1.Endpoint{
				ServiceName:      weightOptimizer.Name,
				ServiceNamespace: namespace,
				IP:               result.PodAddress,
				Name:             result.PodName,
				Weight:           newWeights[result.PodAddress],
				Optimized:        false,
				LastOptimized:    metav1.Time{Time: time.Now()},
			}
		}
		weightOptimizerEndpoint.Locality = serviceEntryLocalityMap[result.PodAddress]
		weightsMap[result.PodAddress] = newWeight
		filteredEndpoints = append(filteredEndpoints, weightOptimizerEndpoint)
	}

	weightOptimizer.Spec.Endpoints = filteredEndpoints

	for i, weightOptimizerEndpoint := range weightOptimizer.Spec.Endpoints {
		weightOptimizer.Spec.Endpoints[i].Weight = newWeights[weightOptimizerEndpoint.IP]
		logger.V(1).Info("Updated weight", "IP", weightOptimizerEndpoint.IP, "NewWeight", newWeights[weightOptimizerEndpoint.IP])
	}

	return nil
}

func (r *WeightOptimizerReconciler) distributeWeightsBasedOnCPU(ctx context.Context, podMetricsMap map[string]*PodMetrics, serviceEntry *istionetworkingv1.ServiceEntry) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	if len(podMetricsMap) == 0 {
		return
	}

	podAddressToWorkloadEntry := make(map[string]*istioapinetworkingv1.WorkloadEntry)
	podAddressToLocality := make(map[string]string)
	for _, workloadEntry := range serviceEntry.Spec.Endpoints {
		podAddressToWorkloadEntry[workloadEntry.Address] = workloadEntry
		podAddressToLocality[workloadEntry.Address] = workloadEntry.Locality
	}

	groups := make(map[string][]*PodMetrics, len(podMetricsMap))
	for address := range podAddressToWorkloadEntry {
		locality := podAddressToLocality[address]
		podMetric, ok := podMetricsMap[address]
		if !ok {
			podMetric = &PodMetrics{
				PodAddress: address,
			}
		}
		groups[locality] = append(groups[locality], podMetric)
	}

	// Process each locality group
	for localityKey, groupPods := range groups {
		logger.V(4).Info("Processing group", "localityKey", localityKey, "groupPods", groupPods)

		// Calculate group total weight
		var groupTotalWeight float64
		for _, pm := range groupPods {
			groupTotalWeight += float64(podAddressToWorkloadEntry[pm.PodAddress].Weight)
		}

		// Check if group total weight is below minimum threshold
		minGroupTotal := float64(len(groupPods)) * 200.0
		if groupTotalWeight < minGroupTotal {
			for _, pm := range groupPods {
				podAddressToWorkloadEntry[pm.PodAddress].Weight *= 5
			}
			continue
		}

		// Calculate average CPU for the group
		var cpuTimes []float64
		for _, pm := range groupPods {
			cpuTimes = append(cpuTimes, pm.CPUTime)
		}
		avgCPU, _ := stats.Mean(cpuTimes)
		if avgCPU < 0.20 {
			// Set all to maximum if insufficient data
			for _, pm := range groupPods {
				podAddressToWorkloadEntry[pm.PodAddress].Weight = 1000
			}
			continue
		}
		// Calculate X_sum for the "cake" formula
		var xSum float64
		for _, pm := range groupPods {
			if pm.CPUTime > 0 {
				xSum += avgCPU / pm.CPUTime * float64(podAddressToWorkloadEntry[pm.PodAddress].Weight)
			}
		}

		// Adjust weights for each pod in the group
		scaleupFactor := r.ScaleupFactor
		scaledownFactor := r.ScaledownFactor
		avgGroupWeight := groupTotalWeight / float64(len(groupPods))
		for _, pm := range groupPods {
			currentWeight := float64(podAddressToWorkloadEntry[pm.PodAddress].Weight)
			if currentWeight == 0 {
				// TODO: its hack we need change the iteration to be based on the serviceEntryWeightsMap in next version.
				logger.Info("Weight is 0, skipping", "PodAddress", pm.PodAddress, "PodName", pm.PodName)
				podAddressToWorkloadEntry[pm.PodAddress].Weight = uint32(r.MinimumWeight)
				continue
			}
			newShare := (avgCPU / pm.CPUTime) * (currentWeight / xSum) * groupTotalWeight
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
			if adjustedWeight < float64(r.MinimumWeight) {
				logger.Info("Adjusted weight is less than minimumWeight, setting it to minimumWeight Weight optimization", "PodAddress", pm.PodAddress, "PodName", pm.PodName, "CPUTime", pm.CPUTime, "Weight", podAddressToWorkloadEntry[pm.PodAddress].Weight, "NewShare", newShare, "AdjustedWeight", adjustedWeight, "Distance", distance, "MaxAllowedDistance", maxAllowedDistance, "avgCPU", avgCPU, "xSum", xSum, "scaleupFactor", scaleupFactor, "scaledownFactor", scaledownFactor, "avgGroupWeight", avgGroupWeight, "currentWeight", currentWeight, "minimumWeight", r.MinimumWeight)
				adjustedWeight = float64(r.MinimumWeight)
			}

			podAddressToWorkloadEntry[pm.PodAddress].Weight = uint32(adjustedWeight)
		}
		// Normalize weights to keep the average weight equal to 1000
		var totalWeight float64
		for _, podMetrics := range groupPods {
			totalWeight += float64(podAddressToWorkloadEntry[podMetrics.PodAddress].Weight)
		}
		normalizationFactor := (1000 * float64(len(groupPods))) / totalWeight
		for _, podMetrics := range groupPods {
			podAddressToWorkloadEntry[podMetrics.PodAddress].Weight = uint32(float64(podAddressToWorkloadEntry[podMetrics.PodAddress].Weight) * normalizationFactor)
		}

	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *WeightOptimizerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	//logger := log.FromContext(context.Background())
	namespacePredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return helpers.NamespaceInFilteredList(e.Object.GetNamespace(), r.NamespaceList)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return helpers.NamespaceInFilteredList(e.ObjectNew.GetNamespace(), r.NamespaceList)
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&optimizationv1alpha1.IstioAdaptiveRequestOptimizer{}).
		WithEventFilter(namespacePredicate).
		Complete(r)
}
