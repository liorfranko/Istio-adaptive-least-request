package controller

import (
	"strings"

	istioClientV1 "istio.io/client-go/pkg/apis/networking/v1"

	customMetrics "istio-adaptive-least-request/internal/metrics"
)

func updateMetrics(serviceEntry *istioClientV1.ServiceEntry) {
	for _, workloadEntry := range serviceEntry.Spec.Endpoints {
		podAddress := workloadEntry.Address
		podZone := getLocalityForMetric(workloadEntry.Locality)
		podWeight := workloadEntry.Weight
		customMetrics.WeightMetric.WithLabelValues(serviceEntry.Namespace, serviceEntry.Name, podAddress, podZone).Set(float64(podWeight))
	}
}

func getLocalityForMetric(locality string) string {
	if locality == "" {
		return "global"
	}
	parts := strings.Split(locality, "/")
	if len(parts) == 2 {
		return parts[1]
	}
	return locality
}
