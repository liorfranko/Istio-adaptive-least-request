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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"istio-adaptive-least-request/internal/helpers"
	customMetrics "istio-adaptive-least-request/internal/metrics"

	//istionetworkingv1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
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
	var istioOptimizer optimizationv1alpha1.IstioAdaptiveRequestOptimizer
	if err := r.Get(ctx, req.NamespacedName, &istioOptimizer); err != nil {
		logger.Info("IstioLatencyOptimizer not found", "Namespace", req.Namespace, "Name", req.Name)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if istioOptimizer.GetDeletionTimestamp() != nil {
		// We've got an update event that indicate the instance is being deleted, the event it update by this controller doesn't need to do anything with it as the IstioLatencyOptimizer handle the deltion
		logger.Info("IstioLatencyOptimizer is being deleted", "Namespace", istioOptimizer.Namespace, "Name", istioOptimizer.Name)
		return ctrl.Result{}, nil
	}
	objectKey := client.ObjectKey{
		Name:      istioOptimizer.Name,
		Namespace: istioOptimizer.Namespace,
	}
	// Fetch the ServiceEntry for the port
	var serviceEntry istionetworkingv1.ServiceEntry
	if err := r.Get(ctx, objectKey, &serviceEntry); err != nil {
		// If the ServiceEntry not found, log an error and continue to the next port
		logger.Error(err, "ServiceEntry not exists, continue to the next port if there is", "ServiceEntry", serviceEntry.Name)
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "fetch_service_entry", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
		return ctrl.Result{}, err
	}
	logger.V(1).Info("Fetched ServiceEntry", "ServiceEntry", serviceEntry.Name)

	// Create a map of the weights of the endpoints from the ServiceEntry
	serviceEntryWeightsMap := r.getServiceEntryWeightMap(&serviceEntry)
	serviceEntryLocalityMap := r.getServiceEntryLocalityMap(&serviceEntry)

	if len(serviceEntry.Spec.Endpoints) == 0 {
		logger.Info("ServiceEntry doesn't have any endpoints, continue", "ServiceEntry", serviceEntry.Name)
		return ctrl.Result{RequeueAfter: r.RequeueAfter * time.Second}, nil
	}

	// create a map of pod ips and their names from pods ips
	// Fetch pods using the selector
	// C
	labelsForPods := labels.SelectorFromSet(labels.Set{
		"service.istio.io/canonical-name": istioOptimizer.Spec.ServiceName,
	})
	podsInfo, err := r.listPods(ctx, istioOptimizer.Namespace, labelsForPods)
	if err != nil {
		logger.Error(err, "Failed to list Pods", "Namespace", istioOptimizer.Namespace)
		return ctrl.Result{}, err
	}
	//logger.V(1).Info("Pods fetched", "Pods", podsInfo)
	// Get the metrics from VictoriaMetrics for the service and protocol
	getPodMetricsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	podsMetrics, err := r.getPodMetrics(getPodMetricsCtx, istioOptimizer.Name, istioOptimizer.Namespace, podsInfo)
	if err != nil {
		// If there is a problem with pulling the metrics from VictoriaMetrics, log an error and continue to the next port
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "get_metrics_from_vm", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
		err := r.fallbackStrategy(ctx, &istioOptimizer, objectKey, serviceEntryWeightsMap, serviceEntryLocalityMap, podsInfo)
		if err != nil {
			customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "fallback_strategy", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
			return ctrl.Result{}, err
		}
		logger.Info("continue to the next port if there is", "service.Name", istioOptimizer.Name)
		return ctrl.Result{}, err
	}

	// Update pod metrics based on the response from VictoriaMetrics
	if err = r.updatePodMetrics(ctx, &podsMetrics); err != nil {
		// If there is a problem with calculating the Alpha,Distance,Multiplier from VictoriaMetrics, log an error and continue to the next port
		logger.Error(err, "Error calculating the Alpha,Distance,Multiplier based on the response from VictoriaMetrics, continue to the next port if there is", "service.Name", istioOptimizer.Name)
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "update_pod_metrics", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
		return ctrl.Result{}, err
	}

	// Fetch the WeightOptimizer for the port
	weightOptimizer, err := r.ensureWeightOptimizer(ctx, &istioOptimizer, objectKey, serviceEntryWeightsMap, serviceEntryLocalityMap)
	if err != nil {
		logger.Error(err, "Failed to ensure WeightOptimizer is available")
		return ctrl.Result{}, err
	}

	newWeightsMap, err := r.distributeWeightsBasedOnCPU(ctx, podsMetrics, serviceEntryWeightsMap, serviceEntryLocalityMap)
	updatedWeightOptimizer, _, err := r.calculateNewWeights(ctx, podsMetrics, newWeightsMap, istioOptimizer.Namespace, weightOptimizer, serviceEntryLocalityMap)
	// Calculate the new weights based on the metrics
	//updatedWeightOptimizer, totalWeight, err := r.calculateNewWeights(ctx, podsMetrics, serviceEntryWeightsMap, objectKey, istioOptimizer.Namespace, weightOptimizer)
	if err != nil {
		logger.Error(err, "Error calculating new weights, continue to the next port if there is", "service.Name", istioOptimizer.Name)
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "calculate_new_weights", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
		return ctrl.Result{}, err
	}

	// Update the WeightOptimizer resource with the updated weights and metrics
	if err := r.Update(ctx, updatedWeightOptimizer); err != nil {
		logger.Error(err, "Error updating WeightOptimizer, retry reconcile", "service.Name", istioOptimizer.Name)
		return ctrl.Result{}, err
	}

	// Requeue to process services periodically
	return ctrl.Result{RequeueAfter: r.RequeueAfter * time.Second}, nil
}

func (r *WeightOptimizerReconciler) getServiceEntryLocalityMap(serviceEntry *istionetworkingv1.ServiceEntry) map[string]string {
	localityMap := make(map[string]string)
	for _, workloadEntry := range serviceEntry.Spec.Endpoints {
		localityMap[workloadEntry.Address] = workloadEntry.Locality
	}
	return localityMap
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

// fallbackStrategy checks if a fallback condition is met and resets weights if necessary.
func (r *WeightOptimizerReconciler) fallbackStrategy(ctx context.Context, istioOptimizer *optimizationv1alpha1.IstioAdaptiveRequestOptimizer, objectKey client.ObjectKey, serviceEntryWeightsMap map[string]uint32, serviceEntryLocalityMap map[string]string, podsInfo []PodInfo) error {
	logger := log.FromContext(ctx).WithName(r.LoggerName).WithValues("service.name", istioOptimizer.Name, "service.namespace", istioOptimizer.Namespace)
	logger.Info("Initiating fallback strategy check")

	weightOptimizer, err := r.ensureWeightOptimizer(ctx, istioOptimizer, objectKey, serviceEntryWeightsMap, serviceEntryLocalityMap)
	if err != nil {
		logger.Error(err, "Failed to ensure WeightOptimizer is available")
		return err
	}

	if shouldSkipFallback(weightOptimizer) {
		logger.Info("Recent optimization detected; skipping fallback strategy")
		return nil
	}

	logger.Info("Weights reset to default due to timeout")
	weightOptimizer.Spec.Endpoints = []optimizationv1alpha1.Endpoint{}
	if err := r.resetWeights(ctx, weightOptimizer, podsInfo, serviceEntryLocalityMap); err != nil {
		return err
	}

	return nil
}

// Helper function to decide whether to skip fallback based on optimization times.
func shouldSkipFallback(weightOptimizer *optimizationv1alpha1.WeightOptimizer) bool {
	minLastOptimizedTime := metav1.Now()
	for _, endpoint := range weightOptimizer.Spec.Endpoints {
		if endpoint.LastOptimized.Before(&minLastOptimizedTime) {
			minLastOptimizedTime = endpoint.LastOptimized
		}
	}
	return time.Since(minLastOptimizedTime.Time) < 5*time.Minute
}

// Reset weights to default values and update the WeightOptimizer.
func (r *WeightOptimizerReconciler) resetWeights(ctx context.Context, weightOptimizer *optimizationv1alpha1.WeightOptimizer, podsInfo []PodInfo, serviceEntryLocalityMap map[string]string) error {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	for _, podInfo := range podsInfo {
		logger.Info("Resetting endpoint to default values", "endpoint", podInfo.PodAddress)
		weightOptimizer.Spec.Endpoints = append(weightOptimizer.Spec.Endpoints, optimizationv1alpha1.Endpoint{
			IP:               podInfo.PodAddress,
			Name:             podInfo.PodName,
			Weight:           300,
			Multiplier:       1,
			Alpha:            0,
			Distance:         0,
			ResponseTime:     0,
			Optimized:        false,
			LastOptimized:    metav1.Time{Time: time.Now()},
			ServiceName:      weightOptimizer.Name,
			ServiceNamespace: weightOptimizer.Namespace,
			Locality:         serviceEntryLocalityMap[podInfo.PodAddress],
		})
	}
	if err := r.Update(ctx, weightOptimizer); err != nil {
		logger.Error(err, "Failed to apply changes to WeightOptimizer")
		return err
	}
	return nil
}

func (r *WeightOptimizerReconciler) ensureWeightOptimizer(ctx context.Context, istioOptimizer *optimizationv1alpha1.IstioAdaptiveRequestOptimizer, objectKey client.ObjectKey, serviceEntryWeightsMap map[string]uint32, serviceEntryLocalityMap map[string]string) (*optimizationv1alpha1.WeightOptimizer, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	// Fetch the WeightOptimizer for the port
	weightOptimizer := new(optimizationv1alpha1.WeightOptimizer)
	err := r.Get(ctx, objectKey, weightOptimizer)
	if err == nil {
		logger.V(1).Info("Fetched or created WeightOptimizer", "Namespace", istioOptimizer.Namespace, "Name", objectKey.Name, "Spec", weightOptimizer.Spec)
		if weightOptimizer.Spec.LocalityEnabled != istioOptimizer.Spec.LocalityEnabled {
			weightOptimizer.Spec.LocalityEnabled = istioOptimizer.Spec.LocalityEnabled
		}
		return weightOptimizer, nil
	}
	if !errors.IsNotFound(err) {
		logger.Error(err, "Failed to fetch WeightOptimizer", "Namespace", istioOptimizer.Namespace, "Name", objectKey.Name)
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "fetch_weight_optimizer", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
		return nil, err
	}
	// If weightOptimizer not found, create a new instance
	logger.Info("weightOptimizer not found, creating a new one", "Namespace", istioOptimizer.Namespace, "Name", objectKey.Name)
	weightOptimizer, err = r.createWeightOptimizer(ctx, nil, objectKey, istioOptimizer.Namespace, *istioOptimizer, serviceEntryWeightsMap, serviceEntryLocalityMap)
	if err != nil {
		logger.Error(err, "Failed to create WeightOptimizer", "Namespace", istioOptimizer.Namespace, "Name", objectKey.Name)
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "create_weight_optimizer", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
		return nil, err
	}
	return weightOptimizer, nil
}

// updatePodMetrics processes the raw metrics data retrieved from VictoriaMetrics to calculate statistical values and update the pod metrics.
func (r *WeightOptimizerReconciler) updatePodMetrics(ctx context.Context, podsMetrics *map[string]*PodMetrics) error {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	if len(*podsMetrics) == 0 {
		return fmt.Errorf("no metrics to process")
	}

	cpuTimes := []float64{}
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
	return nil
}

func (r *WeightOptimizerReconciler) getServiceEntryWeightMap(serviceEntry *istionetworkingv1.ServiceEntry) map[string]uint32 {
	// TODO: Change it to return a map of WorkloadEntry
	weightsMap := make(map[string]uint32)
	for _, ep := range serviceEntry.Spec.Endpoints {
		weightsMap[ep.Address] = ep.Weight
	}
	return weightsMap
}

func (r *WeightOptimizerReconciler) getWeightOptimizerMap(weightOptimizer *optimizationv1alpha1.WeightOptimizer) map[string]optimizationv1alpha1.Endpoint {
	weightsMap := make(map[string]optimizationv1alpha1.Endpoint)
	for _, ep := range weightOptimizer.Spec.Endpoints {
		weightsMap[ep.IP] = ep
	}
	return weightsMap
}

func (r *WeightOptimizerReconciler) calculateNewWeights(ctx context.Context, podsMetrics map[string]*PodMetrics, newWeights map[string]uint32, namespace string, weightOptimizer *optimizationv1alpha1.WeightOptimizer, serviceEntryLocalityMap map[string]string) (*optimizationv1alpha1.WeightOptimizer, float64, error) {
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

	totalWeight := 0.0
	for i, weightOptimizerEndpoint := range weightOptimizer.Spec.Endpoints {
		weightOptimizer.Spec.Endpoints[i].Weight = newWeights[weightOptimizerEndpoint.IP]
		totalWeight += float64(newWeights[weightOptimizerEndpoint.IP])
		logger.V(1).Info("Updated weight", "IP", weightOptimizerEndpoint.IP, "NewWeight", newWeights[weightOptimizerEndpoint.IP])
	}

	return weightOptimizer, totalWeight, nil
}

// constructWeightOptimizer creates a new WeightOptimizer instance based on the processed metrics.
func (r *WeightOptimizerReconciler) createWeightOptimizer(ctx context.Context, podsMetrics map[string]*PodMetrics, objectKey client.ObjectKey, namespace string, istioOptimizer optimizationv1alpha1.IstioAdaptiveRequestOptimizer, serviceEntryWeightsMap map[string]uint32, serviceEntryLocalityMap map[string]string) (*optimizationv1alpha1.WeightOptimizer, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	// Create the weightOptimizer instance with the owner reference set to the IstioLatencyOptimizer
	weightOptimizer := &optimizationv1alpha1.WeightOptimizer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objectKey.Name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					Name:       istioOptimizer.Name,
					APIVersion: istioOptimizer.APIVersion,
					Kind:       istioOptimizer.Kind,
					UID:        istioOptimizer.UID,
				},
			},
		},
	}
	logger.V(1).Info("Constructing WeightOptimizer", "Namespace", namespace, "Name", weightOptimizer.Name)

	spec := &weightOptimizer.Spec
	spec.LocalityEnabled = istioOptimizer.Spec.LocalityEnabled

	// Iterate over the podsMetrics and add the pods that exists in the ServiceEntry
	// If the endpoint is not found in the ServiceEntry, log a warning and continue without that pod
	for _, result := range podsMetrics {
		weight, ok := serviceEntryWeightsMap[result.PodAddress]
		if !ok {
			logger.Info("Endpoint IP found in VictoriaMetrics but not found in serviceEntryWeightsMap - can't set initial weight to weightOptimizer, continue without that pod", "IP", result.PodAddress)
			continue
		}
		spec.Endpoints = append(spec.Endpoints, optimizationv1alpha1.Endpoint{
			ServiceName:      objectKey.Name,
			ServiceNamespace: namespace,
			IP:               result.PodAddress,
			Name:             result.PodName,
			Weight:           weight,
			Optimized:        false,
			Locality:         serviceEntryLocalityMap[result.PodAddress],
			LastOptimized:    metav1.Time{Time: time.Now()},
		})
	}
	if err := r.Create(ctx, weightOptimizer); err != nil {
		logger.Error(err, "Failed to create weightOptimizer")
		return nil, err
	}
	return weightOptimizer, nil
}

func (r *WeightOptimizerReconciler) distributeWeightsBasedOnCPU(ctx context.Context, podMetricsMap map[string]*PodMetrics, serviceEntryWeightsMap map[string]uint32, serviceEntryLocalityMap map[string]string) (map[string]uint32, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	if len(podMetricsMap) == 0 {
		return nil, fmt.Errorf("no metrics to process")
	}

	groups := make(map[string][]*PodMetrics, len(podMetricsMap))
	for address := range serviceEntryWeightsMap {
		locality := serviceEntryLocalityMap[address]
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
			groupTotalWeight += float64(serviceEntryWeightsMap[pm.PodAddress])
		}

		// Check if group total weight is below minimum threshold
		minGroupTotal := float64(len(groupPods)) * 200.0
		if groupTotalWeight < minGroupTotal {
			for _, pm := range groupPods {
				serviceEntryWeightsMap[pm.PodAddress] *= 5
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
				serviceEntryWeightsMap[pm.PodAddress] = 1000
			}
			continue
		}
		// Calculate X_sum for the "cake" formula
		var xSum float64
		for _, pm := range groupPods {
			if pm.CPUTime > 0 {
				xSum += avgCPU / pm.CPUTime * float64(serviceEntryWeightsMap[pm.PodAddress])
			}
		}

		// Adjust weights for each pod in the group
		scaleupFactor := r.ScaleupFactor
		scaledownFactor := r.ScaledownFactor
		avgGroupWeight := groupTotalWeight / float64(len(groupPods))
		for _, pm := range groupPods {
			currentWeight := float64(serviceEntryWeightsMap[pm.PodAddress])
			if currentWeight == 0 {
				// TODO: its hack we need change the iteration to be based on the serviceEntryWeightsMap in next version.
				logger.Info("Weight is 0, skipping", "PodAddress", pm.PodAddress, "PodName", pm.PodName)
				serviceEntryWeightsMap[pm.PodAddress] = uint32(r.MinimumWeight)
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
				logger.Info("Adjusted weight is less than minimumWeight, setting it to minimumWeight Weight optimization", "PodAddress", pm.PodAddress, "PodName", pm.PodName, "CPUTime", pm.CPUTime, "Weight", serviceEntryWeightsMap[pm.PodAddress], "NewShare", newShare, "AdjustedWeight", adjustedWeight, "Distance", distance, "MaxAllowedDistance", maxAllowedDistance, "avgCPU", avgCPU, "xSum", xSum, "scaleupFactor", scaleupFactor, "scaledownFactor", scaledownFactor, "avgGroupWeight", avgGroupWeight, "currentWeight", currentWeight, "minimumWeight", r.MinimumWeight)
				adjustedWeight = float64(r.MinimumWeight)
			}

			serviceEntryWeightsMap[pm.PodAddress] = uint32(adjustedWeight)
		}
		// Normalize weights to keep the average weight equal to 1000
		var totalWeight float64
		for _, podMetrics := range groupPods {
			totalWeight += float64(serviceEntryWeightsMap[podMetrics.PodAddress])
		}
		normalizationFactor := (1000 * float64(len(groupPods))) / totalWeight
		for _, podMetrics := range groupPods {
			serviceEntryWeightsMap[podMetrics.PodAddress] = uint32(float64(serviceEntryWeightsMap[podMetrics.PodAddress]) * normalizationFactor)
		}
	}

	return serviceEntryWeightsMap, nil
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
