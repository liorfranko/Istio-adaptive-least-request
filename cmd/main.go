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

package main

import (
	"crypto/tls"
	"flag"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/event"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	optimizationv1alpha1 "istio-adaptive-least-request/api/v1alpha1"
	"istio-adaptive-least-request/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(optimizationv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var endpointsAnnotationKey, endpointPodScrapeAnnotationKey string
	var serviceEntryLabelKey, serviceEntryServiceNameLabelKey string
	var namespaces string
	var vmdbUrl string
	var optimizeCycleTime int
	var minimumWeight, maximumWeight int
	var queryInterval, stepInterval string
	var minOptimizeCpuDistance, cpuDistanceMultiplier float64
	var newEndpointsPercentileWeight int

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metric endpoint binds to. "+
		"Use the port :8080. If not set, it will be 0 in order to disable the metrics server")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false,
		"If set the metrics endpoint is served securely")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&endpointsAnnotationKey, "endpoints-annotation", "istio.adaptive.request.optimizer/optimize", "The annotation to use for setting that the endpoints object is optimized")
	flag.StringVar(&endpointPodScrapeAnnotationKey, "endpoint-pod-scrape-annotation", "istio.adaptive.request.optimizer/scrape", "The annotation to use for setting that the endpoints object is to be scraped")
	flag.StringVar(&serviceEntryLabelKey, "serviceentry-label", "istio.adaptive.request.optimizer/optimize", "The label to use for setting that the ServiceEntry object is optimized")
	flag.StringVar(&serviceEntryServiceNameLabelKey, "serviceentry-service-name-label", "istio.adaptive.request.optimizer/service-name", "The label to use for setting the service name in the ServiceEntry object")
	flag.StringVar(&namespaces, "namespaces", "", "Comma-separated list of namespaces to watch")
	flag.StringVar(&queryInterval, "query-interval", "1m", "The time range over which to aggregate metrics when querying the VMDB service")
	flag.StringVar(&stepInterval, "step-interval", "20s", "The granularity of the data points returned by Prometheus when querying the VMDB service")
	flag.IntVar(&optimizeCycleTime, "optimize-cycle-time", 30, "The time in seconds to run the optimization cycle")
	flag.IntVar(&minimumWeight, "minimum-weight", 100, "The minimum weight for an endpoint to get, increasing this will make the split between the slowest and fastest endpoints smaller")
	flag.IntVar(&maximumWeight, "maximum-weight", 600, "The maximum weight to use for the endpoints, decreasing this will make the split between the slowest and fastest endpoints smaller")
	flag.Float64Var(&minOptimizeCpuDistance, "min-optimize-cpu-distance", 0.1, "The minimum distance between the CPU usage of the pods and the mean CPU of the service, below that value the optimization cycle will be skipped for that pods")
	flag.Float64Var(&cpuDistanceMultiplier, "cpu-distance-multiplier", 0.1, "The multiplier to use to convert the CPU distance to weight changes, the weight will be calculated as 1 - (cpuDistance * CpuDistanceMultiplier)")
	flag.IntVar(&newEndpointsPercentileWeight, "new-endpoints-percentile-weight", 50, "The percentile weight to use for the new endpoints, higher value means that new endpoints will start with a higher weight")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	var namespaceList []string
	if namespaces != "" {
		namespaceList = strings.Split(namespaces, ",")
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	tlsOpts := []func(*tls.Config){}
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: tlsOpts,
	})

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress:   metricsAddr,
			SecureServing: secureMetrics,
			TLSOpts:       tlsOpts,
		},
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "7d4baaac.liorfranko.github.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.IstioAdaptiveRequestOptimizerReconciler{
		Client:                          mgr.GetClient(),
		Scheme:                          mgr.GetScheme(),
		LoggerName:                      "IstioAdaptiveRequestOptimizer",
		EndpointsAnnotationKey:          &endpointsAnnotationKey,
		EndpointsPodScrapeAnnotationKey: &endpointPodScrapeAnnotationKey,
		ServiceEntryLabelKey:            &serviceEntryLabelKey,
		ServiceEntryServiceNameLabelKey: &serviceEntryServiceNameLabelKey,
		NamespaceList:                   namespaceList,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "IstioAdaptiveRequestOptimizer")
		os.Exit(1)
	}
	//Create the channel for triggering ServiceEntry reconciliation
	serviceEntryReconcileTriggerChannel := make(chan event.GenericEvent, 100)
	if err = (&controller.EndpointReconciler{
		Client:                              mgr.GetClient(),
		Scheme:                              mgr.GetScheme(),
		LoggerName:                          "EndpointController",
		EndpointsAnnotationKey:              &endpointsAnnotationKey,
		ServiceEntryReconcileTriggerChannel: serviceEntryReconcileTriggerChannel,
		ServiceEntryServiceNameLabelKey:     &serviceEntryServiceNameLabelKey,
		NamespaceList:                       namespaceList,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Endpoint")
		os.Exit(1)
	}
	if err = (&controller.WeightOptimizerReconciler{
		Client:                 mgr.GetClient(),
		Scheme:                 mgr.GetScheme(),
		LoggerName:             "WeightOptimizerController",
		VmdbUrl:                &vmdbUrl,
		NamespaceList:          namespaceList,
		RequeueAfter:           time.Duration(optimizeCycleTime),
		MaximumWeight:          maximumWeight,
		MinimumWeight:          minimumWeight,
		QueryInterval:          queryInterval,
		StepInterval:           stepInterval,
		MinOptimizeCpuDistance: minOptimizeCpuDistance,
		CpuDistanceMultiplier:  cpuDistanceMultiplier,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "WeightOptimizer")
		os.Exit(1)
	}
	if err = (&controller.ServiceEntryReconciler{
		Client:                              mgr.GetClient(),
		Scheme:                              mgr.GetScheme(),
		LoggerName:                          "ServiceEntryController",
		ServiceEntryReconcileTriggerChannel: serviceEntryReconcileTriggerChannel,
		ServiceEntryServiceNameLabelKey:     &serviceEntryServiceNameLabelKey,
		NamespaceList:                       namespaceList,
		NewEndpointsPercentileWeight:        newEndpointsPercentileWeight,
		MaximumWeight:                       maximumWeight,
		MinimumWeight:                       minimumWeight,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ServiceEntry")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
