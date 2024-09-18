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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServicePort represents a single service port with its number and protocol.
type ServicePort struct {
	// Number is the port number of the service.
	Number uint32 `json:"number"`

	// Protocol specifies the protocol used by the port.
	// Only "HTTP", "gRPC", and "TCP" are supported.
	// +kubebuilder:validation:Enum=http;grpc
	Protocol string `json:"protocol"`

	// TargetPort is the target port number of the service.
	// +optional
	TargetPort uint32 `json:"targetPort,omitempty"`
}

// ServiceEntry represents a basic info of a service entry created by the IstioAdaptiveRequestOptimizer controller.
type ServiceEntry struct {
	// Name is the name of the service entry
	Name string `json:"name"`
	// Namespace is the namespace of the service entry
	Namespace string `json:"namespace"`

	// CreationTime is when the service entry was created
	CreationTime metav1.Time `json:"creationTime"`
}

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// IstioAdaptiveRequestOptimizerSpec defines the desired state of IstioAdaptiveRequestOptimizer
type IstioAdaptiveRequestOptimizerSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Name specifies the name of the service to optimize.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ServiceName string `json:"service_name"`

	// Namespace specifies the namespace of the service.
	// +optional
	ServiceNamespace string `json:"service_namespace"`

	// ServicePorts specifies a list of service ports, including port number and protocol. If empty, all ports are considered.
	// +optional
	ServicePorts []ServicePort `json:"service_ports,omitempty"`
}

// IstioAdaptiveRequestOptimizerStatus defines the observed state of IstioAdaptiveRequestOptimizer
type IstioAdaptiveRequestOptimizerStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// Conditions represent the latest available observations of an object's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// OptimizationStatus is a high-level summary of the optimization process
	// +optional
	OptimizationStatus string `json:"optimizationStatus,omitempty"`

	// LastOptimizedTime is the last time the optimization process was successfully completed.
	// +optional
	LastOptimizedTime *metav1.Time `json:"lastOptimizedTime,omitempty"`

	// ObservedGeneration reflects the generation observed by the controller managing this resource.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ServiceEntries contains all the service entries that were created as part of the optimization process
	// +optional
	ServiceEntries []ServiceEntry `json:"serviceEntries,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// IstioAdaptiveRequestOptimizer is the Schema for the istioadaptiverequestoptimizers API
type IstioAdaptiveRequestOptimizer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IstioAdaptiveRequestOptimizerSpec   `json:"spec,omitempty"`
	Status IstioAdaptiveRequestOptimizerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IstioAdaptiveRequestOptimizerList contains a list of IstioAdaptiveRequestOptimizer
type IstioAdaptiveRequestOptimizerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IstioAdaptiveRequestOptimizer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&IstioAdaptiveRequestOptimizer{}, &IstioAdaptiveRequestOptimizerList{})
}
