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
	"context"
	"github.com/prometheus/client_golang/prometheus"
	"istio-adaptive-least-request/helpers"
	customMetrics "istio-adaptive-least-request/metrics"
	istioClientV1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// EndpointReconciler reconciles a Endpoint object
type EndpointReconciler struct {
	client.Client
	Scheme                          *runtime.Scheme
	LoggerName                      string
	DryRun                          bool
	EndpointsAnnotationKey          *string
	ServiceEntryServiceNameLabelKey *string
	// Channel used to trigger reconciliation of ServiceEntry resources.
	ServiceEntryReconcileTriggerChannel chan event.GenericEvent
	NamespaceList                       []string
}

//+kubebuilder:rbac:groups=core,resources=endpoints,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=networking.istio.io,resources=serviceentries,verbs=get;list;watch
//+kubebuilder:rbac:groups=optimization.liorfranko.github.io,resources=istioadaptiverequestoptimizers,verbs=get;list;watch

func (r *EndpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	logger.V(1).Info("Reconcile Endpoint", "Endpoint.Namespace", req.Namespace, "Endpoint.Name", req.Name)

	endpoints := &corev1.Endpoints{}
	if err := r.Get(ctx, req.NamespacedName, endpoints); err != nil {
		logger.Error(err, "Failed to fetch Endpoints", "Namespace", req.Namespace, "Name", req.Name) // Update metric for fetch error
		customMetrics.ErrorMetrics.With(prometheus.Labels{"controller": r.LoggerName, "type": "fetching_endpoints", "name": req.Name, "namespace": req.Namespace}).Inc()
		return ctrl.Result{}, err
	}
	logger.V(1).Info("Endpoints fetched", "Subsets", endpoints.Subsets)

	var serviceEntry istioClientV1beta1.ServiceEntryList
	labelKey := *r.ServiceEntryServiceNameLabelKey
	if err := r.List(ctx, &serviceEntry, client.InNamespace(req.Namespace), client.MatchingLabels{labelKey: endpoints.Name}); err != nil {
		logger.Error(err, "Failed to list ServiceEntry", "Namespace", req.Namespace, "Name", req.Name)
		// Update metric for list error
		// metrics.ListServiceEntryErrors.Inc()
		return ctrl.Result{}, err
	}
	logger.V(1).Info("ServiceEntry fetched", "ServiceEntry", serviceEntry.Items)

	for _, se := range serviceEntry.Items {
		// Logic to compare endpoints and ServiceEntry addresses should be implemented here.

		// Trigger reconciliation for ServiceEntry if differences are found
		r.ServiceEntryReconcileTriggerChannel <- event.GenericEvent{
			Object: &istioClientV1beta1.ServiceEntry{
				ObjectMeta: metav1.ObjectMeta{
					Name:      se.Name,
					Namespace: se.Namespace,
				},
			},
		}
	}

	return ctrl.Result{}, nil
}

// checkNamespaceAndAnnotation checks if the namespace of the object is allowed
// and if the specified annotation exists and has the correct value.
func (r *EndpointReconciler) checkNamespaceAndAnnotation(obj client.Object) bool {
	annotationKey := *r.EndpointsAnnotationKey
	annotationValue := "true"

	// First, check if the namespace is in the allowed list.
	if !helpers.NamespaceInFilteredList(obj.GetNamespace(), r.NamespaceList) {
		return false
	}
	// Then, check if the annotation exists and has the correct value.
	annotations := obj.GetAnnotations()
	val, exists := annotations[annotationKey]
	return exists && val == annotationValue
}

// SetupWithManager sets up the controller with the Manager.
func (r *EndpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	namespaceAndAnnotationPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return r.checkNamespaceAndAnnotation(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return r.checkNamespaceAndAnnotation(e.ObjectNew)
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Endpoints{}).
		WithEventFilter(namespaceAndAnnotationPredicate).
		Complete(r)
}
