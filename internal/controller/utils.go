package controller

import (
	istioClientV1 "istio.io/client-go/pkg/apis/networking/v1"

	customMetrics "istio-adaptive-least-request/internal/metrics"
)

func updateMetrics(serviceEntry *istioClientV1.ServiceEntry) {
	for _, workloadEntry := range serviceEntry.Spec.Endpoints {
		podAddress := workloadEntry.Address
		podWeight := workloadEntry.Weight
		customMetrics.WeightMetric.WithLabelValues(serviceEntry.Namespace, serviceEntry.Name, podAddress).Set(float64(podWeight))
	}
}
