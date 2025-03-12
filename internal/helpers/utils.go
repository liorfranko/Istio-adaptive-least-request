package helpers

import (
	"github.com/prometheus/client_golang/prometheus"
	istioNetworkingV1 "istio.io/api/networking/v1"

	customMetrics "istio-adaptive-least-request/internal/metrics"
)

// SafeDereferenceAppProtocol safely dereferences a pointer to a string (appProtocol).
// It returns the dereference string if it's not nil, or a default value (e.g., "TCP") if it's nil.
func SafeDereferenceAppProtocol(appProtocolPtr *string) string {
	if appProtocolPtr != nil {
		return *appProtocolPtr
	}
	return "TCP"
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

func diff1(dst, a, b []*istioNetworkingV1.WorkloadEntry) []*istioNetworkingV1.WorkloadEntry {
	for _, workloadEntry := range a {
		if !addressContains(b, workloadEntry) {
			dst = append(dst, workloadEntry)
		}
	}
	return dst
}

func Diff(dst, desired, actual []*istioNetworkingV1.WorkloadEntry) []*istioNetworkingV1.WorkloadEntry {
	// Find elements in actual that are not in desired (removals)
	dst = diff1(dst, actual, desired)
	// Find elements in desired that are not in actual (additions)
	dst = diff1(dst, desired, actual)
	return dst
}

func addressContains(workloadEntries []*istioNetworkingV1.WorkloadEntry, targetWorkloadEntry *istioNetworkingV1.WorkloadEntry) bool {
	for _, workloadEntry := range workloadEntries {
		if workloadEntry.Address == targetWorkloadEntry.Address {
			return true
		}
	}
	return false
}
