package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// WeightMetric tracks the weight for service entry endpoints, labeled by pod name and IP.
	WeightMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "endpoint_weight",
			Help: "Weight of a service entry endpoint.",
		},
		[]string{"service_namespace", "service_name", "pod_ip", "locality"}, // Label by pod name, IP, service name, and namespace
	)
	// ErrorMetrics tracks various error occurrences within the reconciler
	ErrorMetrics = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "reconciler_errors",
			Help: "Counts of various errors that occur within the reconciler.",
		},
		[]string{"controller", "type", "name", "namespace"}, // Differentiate by error type and associated service details
	)

	// QueryLatencyMetric tracks the time it takes to get a response from VictoriaMetrics for a service.
	QueryLatencyMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "query_latency",
			Help: "Time to get response from VictoriaMetrics for a service in seconds.",
		},
		[]string{"service_name", "service_namespace"}, // Label by service name and namespace
	)
)

func init() {
	// Register custom metrics with Prometheus's default registry
	metrics.Registry.MustRegister(WeightMetric, ErrorMetrics, QueryLatencyMetric)
}
