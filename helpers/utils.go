package helpers

import (
	"github.com/prometheus/client_golang/prometheus"
	customMetrics "istio-adaptive-least-request/metrics"
)

// ContainsString checks if a string is present in a slice of strings.
func ContainsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

// RemoveString removes a string from a slice of strings.
func RemoveString(slice []string, s string) []string {
	var result []string
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return result
}

// SafeDereferenceAppProtocol safely dereferences a pointer to a string (appProtocol).
// It returns the dereference string if it's not nil, or a default value (e.g., "TCP") if it's nil.
func SafeDereferenceAppProtocol(appProtocol *string) string {
	if appProtocol != nil {
		return *appProtocol
	}
	return "TCP" // or some default value if protocol isn't specified
}

func NamespaceInFilteredList(namespace string, filteredNamespaces []string) bool {
	for _, ns := range filteredNamespaces {
		if namespace == ns {
			return true
		}
	}
	return false
}

func CleanupPodMetrics(serviceName string, serviceNamespace string, podName string, podIP string) int {
	// Define Prometheus metrics to be removed
	removedMetrics := 0
	metricsToRemove := []*prometheus.GaugeVec{
		customMetrics.AlphaMetric,
		customMetrics.DistanceMetric,
		customMetrics.MultiplierMetric,
		customMetrics.WeightMetric,
		customMetrics.ResponseTimeMetric,
		customMetrics.NormalizedWeightMetric,
	}
	for _, metricVec := range metricsToRemove {
		if !metricVec.Delete(prometheus.Labels{"service_name": serviceName, "service_namespace": serviceNamespace, "pod_name": podName, "pod_ip": podIP}) {
			continue
		}
		removedMetrics++
	}
	return removedMetrics
}
