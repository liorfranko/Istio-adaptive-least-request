package controller

// To allow Leader Election, the controller must have RBAC permissions to create and manage leases.
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

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
