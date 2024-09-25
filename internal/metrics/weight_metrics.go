package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// AlphaMetric tracks the alpha value for service entry endpoints, labeled by pod name and IP.
	AlphaMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "endpoint_alpha",
			Help: "Alpha value of a service entry endpoint.",
		},
		[]string{"pod_name", "pod_ip", "service_name", "service_namespace"}, // Label by pod name, IP, service name, and namespace
	)

	// DistanceMetric tracks the distance value for service entry endpoints, labeled by pod name and IP.
	DistanceMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "endpoint_distance",
			Help: "Distance value of a service entry endpoint.",
		},
		[]string{"pod_name", "pod_ip", "service_name", "service_namespace"}, // Label by pod name, IP, service name, and namespace
	)

	// MultiplierMetric tracks the multiplier for service entry endpoints, labeled by pod name and IP.
	MultiplierMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "endpoint_multiplier",
			Help: "Multiplier value of a service entry endpoint.",
		},
		[]string{"pod_name", "pod_ip", "service_name", "service_namespace"}, // Label by pod name, IP, service name, and namespace
	)

	// ResponseTimeMetric tracks the response time for service entry endpoints, labeled by pod name and IP.
	ResponseTimeMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "endpoint_response_time",
			Help: "Response time of a service entry endpoint in seconds.",
		},
		[]string{"pod_name", "pod_ip", "service_name", "service_namespace"}, // Label by pod name, IP, service name, and namespace
	)
	// WeightMetric tracks the weight for service entry endpoints, labeled by pod name and IP.
	WeightMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "endpoint_weight",
			Help: "Weight of a service entry endpoint.",
		},
		[]string{"pod_name", "pod_ip", "service_name", "service_namespace"}, // Label by pod name, IP, service name, and namespace
	)
	NormalizedWeightMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "endpoint_normalized_weight",
			Help: "Normalized weight of a service entry endpoint.",
		},
		[]string{"pod_name", "pod_ip", "service_name", "service_namespace"}, // Label by pod name, IP, service name, and namespace
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
	metrics.Registry.MustRegister(AlphaMetric, DistanceMetric, MultiplierMetric, ResponseTimeMetric, WeightMetric, NormalizedWeightMetric, ErrorMetrics, QueryLatencyMetric)
}
