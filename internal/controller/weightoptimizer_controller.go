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
	"github.com/montanaflynn/stats"
	"github.com/prometheus/client_golang/prometheus"
	"istio-adaptive-least-request/internal/helpers"
	customMetrics "istio-adaptive-least-request/internal/metrics"
	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"math"
	"net/http"
	"net/url"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sort"
	"strconv"
	"strings"
	"time"

	istionetworkingv1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	optimizationv1alpha1 "istio-adaptive-least-request/api/v1alpha1"
)

// WeightOptimizerReconciler reconciles a WeightOptimizer object
type WeightOptimizerReconciler struct {
	client.Client
	Scheme                       *runtime.Scheme
	LoggerName                   string
	VmdbUrl                      *string
	NamespaceList                []string
	RequeueAfter                 time.Duration
	MinimumWeight                int
	MaximumWeight                int
	NewEndpointsPercentileWeight int
	QueryInterval                string
	StepInterval                 string
	MinOptimizeCpuDistance       float64
	CpuDistanceMultiplier        float64
}

type VmdbRespone struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric struct {
				PodName      string `json:"pod_name"`
				PodIp        string `json:"pod_address"`
				ResponseCode string `json:"response_code"`
				GrpcStatus   string `json:"grpc_response_status"`
			} `json:"metric"`
			Value []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
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
	PodName           string  `json:"podName"`
	PodAddress        string  `json:"podAddress"`
	Average           float64 `json:"average"`
	StandardDeviation float64 `json:"standard_deviation"`
	Multiplier        float64 `json:"multiplier"`
	Distance          float64 `json:"distance"`
	Alpha             float64 `json:"alpha"`
	CPUTime           float64 `json:"cpuTime"`
	Weight            uint32  `json:"weight"`
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
	istioOptimizer := &optimizationv1alpha1.IstioAdaptiveRequestOptimizer{}
	if err := r.Get(ctx, req.NamespacedName, istioOptimizer); err != nil {
		logger.Info("IstioLatencyOptimizer not found", "Namespace", req.Namespace, "Name", req.Name)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if istioOptimizer.GetDeletionTimestamp() != nil {
		// We've got an update event that indicate the instance is being deleted, the event it update by this controller doesn't need to do anything with it as the IstioLatencyOptimizer handle the deltion
		logger.Info("IstioLatencyOptimizer is being deleted", "Namespace", istioOptimizer.Namespace, "Name", istioOptimizer.Name)
		return ctrl.Result{}, nil
	}
	// Iterate over the service ports to process each one
	for _, servicePort := range istioOptimizer.Spec.ServicePorts {
		objectKey := client.ObjectKey{
			Name:      fmt.Sprintf("%s-%d", istioOptimizer.Name, servicePort.Number),
			Namespace: istioOptimizer.Namespace,
		}

		// Fetch the ServiceEntry for the port
		serviceEntry := istionetworkingv1beta1.ServiceEntry{}
		err := r.Get(ctx, objectKey, &serviceEntry)
		if err != nil {
			// If the ServiceEntry not found, log an error and continue to the next port
			logger.Error(err, "ServiceEntry not exists, continue to the next port if there is", "ServiceEntry", serviceEntry.Name)
			customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "fetch_service_entry", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
			continue
		}
		logger.V(1).Info("Fetched ServiceEntry", "ServiceEntry", serviceEntry.Name)

		// Create a map of the weights of the endpoints from the ServiceEntry
		serviceEntryWeightsMap, err := r.getServiceEntryWeightMap(&serviceEntry)
		if err != nil {
			// If there is a problem with creating the serviceEntryWeightsMap, log an error and continue to the next port
			logger.Error(err, "Error creating serviceEntryWeightsMap for ServiceEntry, continue to the next port if there is", "ServiceEntry", serviceEntry.Name)
			customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "create_service_entry_weight_map", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
			continue
		}

		if len(serviceEntry.Spec.Endpoints) == 0 {
			logger.Info("ServiceEntry doesn't have any endpoints, continue", "ServiceEntry", serviceEntry.Name)
			continue
		}

		// create a map of pod ips and their names from pods ips
		// Fetch pods using the selector
		// C
		podsInfo, err := r.listPods(ctx, istioOptimizer.Namespace, labels.SelectorFromSet(serviceEntry.Spec.Endpoints[0].Labels))
		if err != nil {
			logger.Error(err, "Failed to list Pods", "Namespace", istioOptimizer.Namespace)
			return ctrl.Result{}, err
		}
		logger.V(1).Info("Pods fetched", "Pods", podsInfo)
		// Get the metrics from VictoriaMetrics for the service and protocol
		podsMetrics, err := r.getPodMetrics(ctx, istioOptimizer.Name, istioOptimizer.Namespace, podsInfo)
		if err != nil {
			// If there is a problem with pulling the metrics from VictoriaMetrics, log an error and continue to the next port
			customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "get_metrics_from_vm", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
			err := r.fallbackStrategy(ctx, istioOptimizer, objectKey, servicePort, serviceEntryWeightsMap, podsInfo)
			if err != nil {
				customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "fallback_strategy", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
				return ctrl.Result{}, err
			}
			logger.Info("continue to the next port if there is", "service.Name", istioOptimizer.Name, "port", servicePort.Number)
			continue
		}

		// Update pod metrics based on the response from VictoriaMetrics
		err = r.updatePodMetrics(ctx, &podsMetrics)
		if err != nil {
			// If there is a problem with calculating the Alpha,Distance,Multipliar from VictoriaMetrics, log an error and continue to the next port
			logger.Error(err, "Error calculating the Alpha,Distance,Multipliar based on the response from VictoriaMetrics, continue to the next port if there is", "service.Name", istioOptimizer.Name, "port", servicePort.Number)
			customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "update_pod_metrics", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
			continue
		}

		// Fetch the WeightOptimizer for the port
		weightOptimizer, err := r.ensureWeightOptimizer(ctx, istioOptimizer, objectKey, serviceEntryWeightsMap)
		if err != nil {
			logger.Error(err, "Failed to ensure WeightOptimizer is available")
			return ctrl.Result{}, err
		}

		// Calculate the new weights based on the metrics
		updatedWeightOptimizer, totalWeight, err := r.calculateNewWeights(ctx, podsMetrics, serviceEntryWeightsMap, objectKey, istioOptimizer.Namespace, weightOptimizer)
		if err != nil {
			logger.Error(err, "Error calculating new weights, continue to the next port if there is", "service.Name", istioOptimizer.Name, "port", servicePort.Number)
			customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "calculate_new_weights", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
			continue
		}

		// Update the WeightOptimizer resource with the updated weights and metrics
		if err := r.Update(ctx, updatedWeightOptimizer); err != nil {
			logger.Error(err, "Error updating WeightOptimizer, retry reconcile", "service.Name", istioOptimizer.Name, "port", servicePort.Number)
			return ctrl.Result{}, err
		}

		// Update the metrics for the service
		r.updateMetrics(updatedWeightOptimizer, totalWeight)
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
	CpuTime, err := r.getVMCPUQueryMetric(ctx, query)
	if err != nil {
		return map[string]*PodCPUMetrics{}, err
	}
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
	podList := &corev1.PodList{}
	listOpts := &client.ListOptions{Namespace: namespace, LabelSelector: selector}
	if err := r.Client.List(ctx, podList, listOpts); err != nil {
		return nil, err
	}
	var podsInfo []PodInfo
	for _, pod := range podList.Items {
		podsInfo = append(podsInfo, PodInfo{
			PodAddress: pod.Status.PodIP,
			PodName:    pod.Name,
			CPUTime:    0.0,
		})
	}
	return podsInfo, nil
}

// fallbackStrategy checks if a fallback condition is met and resets weights if necessary.
func (r *WeightOptimizerReconciler) fallbackStrategy(ctx context.Context, istioOptimizer *optimizationv1alpha1.IstioAdaptiveRequestOptimizer, objectKey client.ObjectKey, servicePort optimizationv1alpha1.ServicePort, serviceEntryWeightsMap map[string]uint32, podsInfo []PodInfo) error {
	logger := log.FromContext(ctx).WithName(r.LoggerName).WithValues("service.name", istioOptimizer.Name, "service.namespace", istioOptimizer.Namespace, "port", servicePort.Number)
	logger.Info("Initiating fallback strategy check")

	weightOptimizer, err := r.ensureWeightOptimizer(ctx, istioOptimizer, objectKey, serviceEntryWeightsMap)
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
	if err := r.resetWeights(ctx, weightOptimizer, podsInfo); err != nil {
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
func (r *WeightOptimizerReconciler) resetWeights(ctx context.Context, weightOptimizer *optimizationv1alpha1.WeightOptimizer, podsInfo []PodInfo) error {
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
		})
	}
	if err := r.Update(ctx, weightOptimizer); err != nil {
		logger.Error(err, "Failed to apply changes to WeightOptimizer")
		return err
	}
	return nil
}

func (r *WeightOptimizerReconciler) ensureWeightOptimizer(ctx context.Context, istioOptimizer *optimizationv1alpha1.IstioAdaptiveRequestOptimizer, objectKey client.ObjectKey, serviceEntryWeightsMap map[string]uint32) (*optimizationv1alpha1.WeightOptimizer, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)

	// Fetch the WeightOptimizer for the port
	weightOptimizer := &optimizationv1alpha1.WeightOptimizer{}
	err := r.Get(ctx, objectKey, weightOptimizer)
	if err != nil {
		if errors.IsNotFound(err) {
			// If weightOptimizer not found, create a new instance
			logger.Info("weightOptimizer not found, creating a new one", "Namespace", istioOptimizer.Namespace, "Name", objectKey.Name)
			weightOptimizer, err = r.createWeightOptimizer(ctx, nil, objectKey, istioOptimizer.Namespace, *istioOptimizer, serviceEntryWeightsMap)
			if err != nil {
				logger.Error(err, "Failed to create WeightOptimizer", "Namespace", istioOptimizer.Namespace, "Name", objectKey.Name)
				customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "create_weight_optimizer", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
				return nil, err
			}
		} else {
			logger.Error(err, "Failed to fetch WeightOptimizer", "Namespace", istioOptimizer.Namespace, "Name", objectKey.Name)
			customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "fetch_weight_optimizer", "name": istioOptimizer.Name, "namespace": istioOptimizer.Namespace}).Inc()
			return nil, err
		}
	}
	logger.V(1).Info("Fetched or created WeightOptimizer", "Namespace", istioOptimizer.Namespace, "Name", objectKey.Name, "Spec", weightOptimizer.Spec)
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
	logger.V(1).Info("averageCPU", "averageCPU", averageCPU)
	logger.V(1).Info("standardDeviationCPU", "standardDeviationCPU", standardDeviationCPU)
	for _, ep := range *podsMetrics {
		if ep.CPUTime == 0.0 {
			// Pods without CPU metrics will have the averageCPU, and are not optimized
			ep.CPUTime = averageCPU
		}
		cpuDistance := ep.CPUTime - averageCPU
		if math.Abs(cpuDistance) < r.MinOptimizeCpuDistance {
			// Don't make changes if the distance is less than the minimum
			ep.Alpha = 0
		} else {
			ep.Alpha = cpuDistance * r.CpuDistanceMultiplier
		}
		ep.Multiplier = 1 - ep.Alpha
		logger.V(1).Info("Updated pod metrics", "PodName", ep.PodName, "PodAddress", ep.PodAddress, "cpuDistance", cpuDistance, "cpuAlpha", ep.Alpha, "cpuMultiplier", ep.Multiplier)

		// Directly modify each element in the slice
		ep.Average = averageCPU
		ep.Distance = cpuDistance
		ep.StandardDeviation = standardDeviationCPU
		if math.IsNaN(ep.Multiplier) {
			ep.Multiplier = 1
		}
	}
	return nil
}

func (r *WeightOptimizerReconciler) getServiceEntryWeightMap(serviceEntry *istionetworkingv1beta1.ServiceEntry) (map[string]uint32, error) {
	// Change it to return a map of WorkloadEntry

	weightsMap := make(map[string]uint32)
	for _, ep := range serviceEntry.Spec.Endpoints {
		weightsMap[ep.Address] = ep.Weight
	}
	return weightsMap, nil
}

func (r *WeightOptimizerReconciler) getWeightOptimizerMap(weightOptimizer *optimizationv1alpha1.WeightOptimizer) (map[string]optimizationv1alpha1.Endpoint, error) {
	weightsMap := make(map[string]optimizationv1alpha1.Endpoint)
	for _, ep := range weightOptimizer.Spec.Endpoints {
		weightsMap[ep.IP] = ep
	}
	return weightsMap, nil
}

func (r *WeightOptimizerReconciler) calculateNewWeights(ctx context.Context, podsMetrics map[string]*PodMetrics, serviceEntryWeightsMap map[string]uint32, objectKey client.ObjectKey, namespace string, weightOptimizer *optimizationv1alpha1.WeightOptimizer) (*optimizationv1alpha1.WeightOptimizer, float64, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)

	// create a map of the weights of the endpoints from the WeightOptimizer
	weightOptimizerMap, err := r.getWeightOptimizerMap(weightOptimizer)

	if err != nil {
		logger.Error(err, "Error creating a weightOptimizerMap", "WeightOptimizer", weightOptimizer.Name)
		return weightOptimizer, 0.0, err
	}

	weightsMap := make(map[string]uint32)
	filteredEndpoints := []optimizationv1alpha1.Endpoint{}
	// Iterate over the podsMetrics and keep the ones that exists in the WeightOptimizer and in the ServiceEntry
	// When a pod is no longer shown in VictoriaMetrics response, it won't be added to the filteredEndpoints and will be removed from the WeightOptimizer
	// When a pod is no longer exists in the ServiceEntry, it won't be added to the filteredEndpoints and it will be removed from the WeightOptimizer
	// When a pod exists in the VictoriaMetrics and the ServiceEntry but not in the WeightOptimizer, it will be added to the WeightOptimizer
	for _, result := range podsMetrics {
		// Check if the endpoint exists in the ServiceEntry
		currentWeight, ok := serviceEntryWeightsMap[result.PodAddress]
		if !ok {
			logger.Info("Endpoint exists in VictoriaMetrics response but not found in serviceEntryWeightsMap - don't optimize it and continue", "IP", result.PodAddress)
			continue
		}
		// Check if the endpoint exists in the WeightOptimizer
		weightOptimizerEndpoint, ok := weightOptimizerMap[result.PodAddress]
		if !ok {
			logger.Info("Endpoint exists in VictoriaMetrics response but not found in WeightOptimizer - adding it", "IP", result.PodAddress)
			weightOptimizerEndpoint = optimizationv1alpha1.Endpoint{
				ServiceName:      weightOptimizer.Name,
				ServiceNamespace: namespace,
				IP:               result.PodAddress,
				Name:             result.PodName,
				Multiplier:       result.Multiplier,
				Distance:         result.Distance,
				Alpha:            result.Alpha,
				Weight:           currentWeight,
				Optimized:        false,
				LastOptimized:    metav1.Time{Time: time.Now()},
			}
		}
		// The pod exists in VictoriaMetrics response and in the ServiceEntry and in the WeightOptimizer
		// Calculate the new weight based on the existing weight and the multiplier
		updatedWeight := r.calculateNewWeight(currentWeight, weightOptimizerEndpoint.Multiplier)
		logger.V(1).Info("Updated weight", "IP", weightOptimizerEndpoint.IP, "CurrentWeight", currentWeight, "NewWeight", updatedWeight)
		// Add the updated weight to the weightsMap and the filteredEndpoints
		weightsMap[result.PodAddress] = updatedWeight
		filteredEndpoints = append(filteredEndpoints, weightOptimizerEndpoint)
	}

	// Iterate over the endpoints in the WeightOptimizer and remove the metrics of the ones that are no longer exists in the VictoriaMetrics response
	for _, weightOptimizerEndpoint := range weightOptimizer.Spec.Endpoints {
		found := false
		for _, filteredEndpoint := range filteredEndpoints {
			if weightOptimizerEndpoint.IP == filteredEndpoint.IP {
				found = true
				break
			}
		}
		if !found {
			logger.V(1).Info("Endpoint exists in WeightOptimizer but not found in filteredEndpoint - deleting the metric", "IP", weightOptimizerEndpoint.IP)
			// Remove the metrics of the endpoint that is no longer exists in the WeightOptimizer
			removedMetrics := helpers.CleanupPodMetrics(weightOptimizerEndpoint.ServiceName, weightOptimizerEndpoint.ServiceNamespace, weightOptimizerEndpoint.Name, weightOptimizerEndpoint.IP)
			logger.V(1).Info("Number of metrics that removed is", "IP", weightOptimizerEndpoint.IP, "RemovedMetrics", removedMetrics)
		}
	}

	// Update the WeightOptimizer with the filteredEndpoints
	weightOptimizer.Spec.Endpoints = filteredEndpoints

	// Calculate the median weight
	medianWeight := getMedianWeight(weightsMap)
	logger.V(1).Info("Median weight", "MedianWeight", medianWeight)

	// Adjust weights to ensure the median weight is at the middle of the weight range
	adjustedWeightsMap := r.adjustWeightsToMedian(weightsMap, medianWeight)
	logger.V(1).Info("Adjusted weights", "AdjustedWeights", adjustedWeightsMap)

	totalWeight := 0.0
	for i, weightOptimizerEndpoint := range weightOptimizer.Spec.Endpoints {
		weightOptimizer.Spec.Endpoints[i].Weight = adjustedWeightsMap[weightOptimizerEndpoint.IP]
		weightOptimizer.Spec.Endpoints[i].Multiplier = podsMetrics[weightOptimizerEndpoint.IP].Multiplier
		weightOptimizer.Spec.Endpoints[i].Alpha = podsMetrics[weightOptimizerEndpoint.IP].Alpha
		weightOptimizer.Spec.Endpoints[i].Distance = podsMetrics[weightOptimizerEndpoint.IP].Distance
		totalWeight += float64(weightOptimizerEndpoint.Weight)
	}
	return weightOptimizer, totalWeight, nil
}

// constructWeightOptimizer creates a new WeightOptimizer instance based on the processed metrics.
func (r *WeightOptimizerReconciler) createWeightOptimizer(ctx context.Context, podsMetrics map[string]*PodMetrics, objectKey client.ObjectKey, namespace string, istioOptimizer optimizationv1alpha1.IstioAdaptiveRequestOptimizer, serviceEntryWeightsMap map[string]uint32) (*optimizationv1alpha1.WeightOptimizer, error) {
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
	// Iterate over the podsMetrics and add the pods that exists in the ServiceEntry
	// If the endpoint is not found in the ServiceEntry, log a warning and continue without that pod
	for _, result := range podsMetrics {
		weight, ok := serviceEntryWeightsMap[result.PodAddress]
		if !ok {
			logger.Info("Endpoint IP found in VictoriaMetrics but not found in serviceEntryWeightsMap - can't set initial weight to weightOptimizer, continue without that pod", "IP", result.PodAddress)
			continue
		}
		// Add the endpoint to the WeightOptimizer
		weightOptimizer.Spec.Endpoints = append(weightOptimizer.Spec.Endpoints, optimizationv1alpha1.Endpoint{
			ServiceName:      objectKey.Name,
			ServiceNamespace: namespace,
			IP:               result.PodAddress,
			Name:             result.PodName,
			Multiplier:       result.Multiplier,
			Distance:         result.Distance,
			Alpha:            result.Alpha,
			Weight:           weight,
			Optimized:        false,
			LastOptimized:    metav1.Time{Time: time.Now()},
		})
	}
	if err := r.Create(ctx, weightOptimizer); err != nil {
		logger.Error(err, "Failed to create weightOptimizer")
		return nil, err
	}
	return weightOptimizer, nil
}

func (r *WeightOptimizerReconciler) adjustWeightsToMedian(weightsMap map[string]uint32, medianWeight uint32) map[string]uint32 {
	if medianWeight == 0 {
		return weightsMap // No adjustment needed if median is 0
	}
	// No adjustment needed if median is already 1000
	middle := (r.MaximumWeight + r.MinimumWeight) / 2
	if medianWeight == uint32(middle) {
		return weightsMap
	}
	// Calculate adjustment ratio using float for precision, then adjust each weight
	adjustmentRatio := float64(middle) / float64(medianWeight)
	adjustedWeightsMap := make(map[string]uint32)
	for key, weight := range weightsMap {
		adjustedWeight := uint32(float64(weight) * adjustmentRatio)
		adjustedWeightsMap[key] = adjustedWeight
	}

	return adjustedWeightsMap
}

// helper function to calculate the median weight from a map of weights.
func getMedianWeight(weightsMap map[string]uint32) uint32 {
	weights := make([]uint32, 0, len(weightsMap))
	for _, weight := range weightsMap {
		weights = append(weights, weight)
	}
	// Sorting the slice of weights for median calculation
	sort.Slice(weights, func(i, j int) bool { return weights[i] < weights[j] })

	n := len(weights)
	var median uint32
	midIndex := n / 2

	if n == 0 {
		return 0 // If no weights, return 0
	}

	// Calculating median
	if n%2 == 0 {
		median = (weights[midIndex-1] + weights[midIndex]) / 2
	} else {
		median = weights[midIndex]
	}
	return median
}

// helper function to calculate the new weight based on the existing weight and a multiplier.
func (r *WeightOptimizerReconciler) calculateNewWeight(currentWeight uint32, multiplier float64) uint32 {
	// logger := log.FromContext(ctx).WithName(loggerName)
	newWeight := float64(currentWeight) * multiplier

	// Ensure the new weight is at least 100
	if newWeight < float64(r.MinimumWeight) {
		newWeight = float64(r.MinimumWeight)
	}
	// Ensure the new weight is at most 1995
	if newWeight > float64(r.MaximumWeight) {
		newWeight = float64(r.MaximumWeight)
	}
	return uint32(newWeight)
}

func (r *WeightOptimizerReconciler) updateMetrics(weightOptimizer *optimizationv1alpha1.WeightOptimizer, totalWeight float64) {
	// logger.Info("Updating Prometheus metrics", "ServiceEntry", serviceEntry.Name, "IP", ep.IP, "ServiceName", ep.WeightOptimizer.ServiceName, "ServiceNamespace", ep.WeightOptimizer.ServiceNamespace)
	for _, ep := range weightOptimizer.Spec.Endpoints {
		customMetrics.AlphaMetric.WithLabelValues(ep.Name, ep.IP, ep.ServiceName, ep.ServiceNamespace).Set(ep.Alpha)
		customMetrics.DistanceMetric.WithLabelValues(ep.Name, ep.IP, ep.ServiceName, ep.ServiceNamespace).Set(ep.Distance)
		customMetrics.MultiplierMetric.WithLabelValues(ep.Name, ep.IP, ep.ServiceName, ep.ServiceNamespace).Set(ep.Multiplier)
		customMetrics.ResponseTimeMetric.WithLabelValues(ep.Name, ep.IP, ep.ServiceName, ep.ServiceNamespace).Set(ep.ResponseTime)
		customMetrics.WeightMetric.WithLabelValues(ep.Name, ep.IP, ep.ServiceName, ep.ServiceNamespace).Set(float64(ep.Weight))
		customMetrics.NormalizedWeightMetric.WithLabelValues(ep.Name, ep.IP, ep.ServiceName, ep.ServiceNamespace).Set(float64(ep.Weight) / totalWeight)
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
