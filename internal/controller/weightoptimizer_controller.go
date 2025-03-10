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
	"bytes"
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
	istioapinetworkingv1 "istio.io/api/networking/v1"
	istionetworkingv1 "istio.io/client-go/pkg/apis/networking/v1"
	corev1 "k8s.io/api/core/v1"
	clientPkg "sigs.k8s.io/controller-runtime/pkg/client"

	api "istio-adaptive-least-request/api/v1alpha1"
	"istio-adaptive-least-request/internal/metrics"
)

type tVmDBItem struct {
	Metric struct {
		Pod string `json:"pod"`
	} `json:"metric"`
	Value []json.RawMessage `json:"value"`
}

type tPodMetrics struct {
	podName    string
	podAddress string
	cpuTime    float64
}

type tPodInfo struct {
	address string
	name    string
}

// getVMQueryMetric queries VictoriaMetrics for the given service and protocol and returns the response.
func getPodMetrics(
	ctx context.Context,
	logger logr.Logger,
	service string,
	namespace string,
	podsInfo []tPodInfo,
	queryInterval string,
	vmDbUrl string,
	stepInterval string,
) (map[string]*tPodMetrics, error) {
	cpuMetrics, err := getCPUMetrics(ctx, logger, service, namespace, queryInterval, vmDbUrl, stepInterval)
	if err != nil {
		return nil, err
	}
	logger.V(1).Info("cpuMetrics", "cpuMetrics", cpuMetrics)
	if len(cpuMetrics) == 0 {
		return nil, fmt.Errorf("no results when getting cpu usage for service %s", service)
	}
	podAddressToPodMetrics := make(map[string]*tPodInfo)
	for i := range podsInfo {
		podInfo := &podsInfo[i]
		podAddressToPodMetrics[podInfo.name] = podInfo
	}
	podMetricsMap := map[string]*tPodMetrics{}
	for podName, podInfo := range podAddressToPodMetrics {
		if cpuMetric, exists := cpuMetrics[podName]; exists {
			address := podInfo.address
			podMetricsMap[address] = &tPodMetrics{
				podName:    podName,
				podAddress: address,
				cpuTime:    cpuMetric,
			}
			logger.V(1).Info("Updated pod with CPU metrics",
				"podName", podName,
				"cpuTime", cpuMetric,
			)
		}
	}
	return podMetricsMap, nil
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
		`sum(rate(container_cpu_usage_seconds_total{namespace="%s",container="%s"}[%s])) by (pod)`,
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
	podCPUMetrics := make(map[string]float64)
	for _, v := range cpuTimes {
		cpuTimeBytes := bytes.Trim(v.Value[1], `"`)
		cpuTime, err := strconv.ParseFloat((string)(cpuTimeBytes), 64)
		if err != nil {
			return nil, fmt.Errorf("error parsing ResponseTime for pod %v: %w", v.Metric.Pod, err)
		}
		podName := v.Metric.Pod
		podCPUMetrics[podName] = cpuTime
	}
	return podCPUMetrics, nil
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

func addPods(logger logr.Logger, pods []corev1.Pod, podsInfo []tPodInfo) []tPodInfo {
	for i := range pods {
		pod := &pods[i]
		// Skip pods that are being deleted
		if pod.DeletionTimestamp.IsZero() {
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
			logger.V(1).Info("Skipping pod: not ready",
				"podName", pod.Name,
			)
			continue
		}
		address := pod.Status.PodIP
		if address == "" {
			logger.V(1).Info("Skipping pod: no IP address",
				"podName", pod.Name,
			)
			continue
		}
		podsInfo = append(podsInfo, tPodInfo{
			address: address,
			name:    pod.Name,
		})
		logger.V(1).Info("Added pod to list",
			"podName", pod.Name,
			"podIP", address,
		)
	}
	logger.Info("Finished listing pods",
		"totalPods", len(pods),
		"readyPods", len(podsInfo),
	)
	return podsInfo
}

func fallbackStrategy(
	ctx context.Context,
	logger logr.Logger,
	client clientPkg.Client,
	opt *api.IstioAdaptiveRequestOptimizer,
	serviceEntry *istionetworkingv1.ServiceEntry,
) error {
	logger.Info("Initiating fallback strategy check")
	if shouldSkipFallback(opt) {
		logger.Info("Recent optimization detected; skipping fallback strategy")
		return nil
	}
	if err := resetWeights(ctx, logger, client, serviceEntry); err != nil {
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
	return time.Since(optimizedTime.Time) < 5*time.Minute
}

func resetWeights(
	ctx context.Context,
	logger logr.Logger,
	client clientPkg.Client,
	serviceEntry *istionetworkingv1.ServiceEntry,
) error {
	for _, workloadEntry := range serviceEntry.Spec.Endpoints {
		logger.Info("Resetting endpoint to default values", "endpoint", workloadEntry.Address)
		workloadEntry.Weight = 300
	}
	if err := client.Update(ctx, serviceEntry); err != nil {
		logger.Error(err, "Failed to update serviceEntry")
		return err
	}
	return nil
}

func enrichPodMetrics(logger logr.Logger, podsMetrics map[string]*tPodMetrics) {
	if len(podsMetrics) == 0 {
		return
	}
	var cpuTimes []float64
	for _, cpuMetric := range podsMetrics {
		if cpuMetric.cpuTime != 0.0 {
			cpuTimes = append(cpuTimes, cpuMetric.cpuTime)
		}
	}
	averageCPU, _ := stats.Mean(cpuTimes)
	standardDeviationCPU, _ := stats.StdDevP(cpuTimes)
	logger.V(4).Info("averageCPU", "averageCPU", averageCPU)
	logger.V(4).Info("standardDeviationCPU", "standardDeviationCPU", standardDeviationCPU)
	for _, ep := range podsMetrics {
		if ep.cpuTime == 0.0 {
			// Pods without CPU metrics will have the averageCPU, and are not optimized
			ep.cpuTime = averageCPU
		}
		cpuDistance := ep.cpuTime - averageCPU
		logger.V(4).Info("CPU Distance", "CPU Distance", cpuDistance)
	}
}

func distributeWeightsBasedOnCPU(
	logger logr.Logger,
	podMetricsMap map[string]*tPodMetrics,
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

	groups := make(map[string][]*tPodMetrics, len(podMetricsMap))
	for address := range podAddressToWorkloadEntry {
		locality := podAddressToLocality[address]
		podMetric, ok := podMetricsMap[address]
		if !ok {
			podMetric = &tPodMetrics{
				podAddress: address,
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
			groupTotalWeight += float64(podAddressToWorkloadEntry[pm.podAddress].Weight)
		}

		// Check if group total weight is below minimum threshold
		minGroupTotal := float64(len(groupPods)) * 200.0
		if groupTotalWeight < minGroupTotal {
			for _, pm := range groupPods {
				podAddressToWorkloadEntry[pm.podAddress].Weight *= 5
			}
			continue
		}

		// Calculate average CPU for the group
		var cpuTimes []float64
		for _, pm := range groupPods {
			cpuTimes = append(cpuTimes, pm.cpuTime)
		}
		avgCPU, _ := stats.Mean(cpuTimes)
		if avgCPU < 0.20 {
			// Set all to maximum if insufficient data
			for _, pm := range groupPods {
				podAddressToWorkloadEntry[pm.podAddress].Weight = 1000
			}
			continue
		}
		// Calculate X_sum for the "cake" formula
		var xSum float64
		for _, pm := range groupPods {
			if pm.cpuTime > 0 {
				xSum += avgCPU / pm.cpuTime * float64(podAddressToWorkloadEntry[pm.podAddress].Weight)
			}
		}

		// Adjust weights for each pod in the group
		avgGroupWeight := groupTotalWeight / float64(len(groupPods))
		for _, pm := range groupPods {
			currentWeight := float64(podAddressToWorkloadEntry[pm.podAddress].Weight)
			if currentWeight == 0 {
				// TODO: its hack we need change the iteration to be based on the serviceEntryWeightsMap in next version.
				logger.Info("Weight is 0, skipping",
					"podAddress", pm.podAddress,
					"podName", pm.podName,
				)
				podAddressToWorkloadEntry[pm.podAddress].Weight = uint32(minimumWeight)
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
					"podAddress", pm.podAddress,
					"podName", pm.podName,
					"cpuTime", pm.cpuTime,
					"Weight", podAddressToWorkloadEntry[pm.podAddress].Weight,
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

			podAddressToWorkloadEntry[pm.podAddress].Weight = uint32(adjustedWeight)
		}
		// Normalize weights to keep the average weight equal to 1000
		var totalWeight float64
		for _, podMetrics := range groupPods {
			totalWeight += float64(podAddressToWorkloadEntry[podMetrics.podAddress].Weight)
		}
		normalizationFactor := (1000 * float64(len(groupPods))) / totalWeight
		for _, podMetrics := range groupPods {
			weight := uint32(float64(podAddressToWorkloadEntry[podMetrics.podAddress].Weight) * normalizationFactor)
			podAddressToWorkloadEntry[podMetrics.podAddress].Weight = weight
		}

	}
}
