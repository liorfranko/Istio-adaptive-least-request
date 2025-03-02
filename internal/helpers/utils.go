package helpers

import (
	"github.com/prometheus/client_golang/prometheus"
	customMetrics "istio-adaptive-least-request/internal/metrics"
	istioNetworkingV1 "istio.io/api/networking/v1"
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

func CleanupPodMetrics(serviceNamespace string, serviceName string, podIP string, locality string) int {
	// Define Prometheus metrics to be removed
	removedMetrics := 0
	metricsToRemove := []*prometheus.GaugeVec{
		customMetrics.WeightMetric,
	}
	for _, metricVec := range metricsToRemove {
		if !metricVec.Delete(prometheus.Labels{"service_namespace": serviceNamespace, "service_name": serviceName, "pod_ip": podIP, "locality": locality}) {
			continue
		}
		removedMetrics++
	}
	return removedMetrics
}

func Diff(desired []*istioNetworkingV1.WorkloadEntry, old []*istioNetworkingV1.WorkloadEntry) []*istioNetworkingV1.WorkloadEntry {
	var diff []*istioNetworkingV1.WorkloadEntry

	// Find elements in old that are not in desired (removals)
	for _, endpoint := range old {
		if !addressContains(desired, endpoint) {
			diff = append(diff, endpoint)
		}
	}

	// Find elements in desired that are not in old (additions)
	for _, endpoint := range desired {
		if !addressContains(old, endpoint) {
			diff = append(diff, endpoint)
		}
	}
	return diff
}

// Contains endpoints with address []*istioNetworkingV1.WorkloadEntry
func addressContains(items []*istioNetworkingV1.WorkloadEntry, item *istioNetworkingV1.WorkloadEntry) bool {
	for _, i := range items {
		if i.Address == item.Address {
			return true
		}
	}
	return false
}
