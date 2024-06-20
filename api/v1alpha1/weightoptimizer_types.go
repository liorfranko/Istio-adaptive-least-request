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

// Endpoint defines the desired state of Endpoint
type Endpoint struct {
	ServiceName      string      `json:"serviceName"`
	ServiceNamespace string      `json:"serviceNamespace"`
	IP               string      `json:"ip"`
	Name             string      `json:"name"`
	Weight           uint32      `json:"weight"`
	Multiplier       float64     `json:"multiplier"`
	ResponseTime     float64     `json:"responseTime"`
	Distance         float64     `json:"distance"`
	Alpha            float64     `json:"alpha"`
	Optimized        bool        `json:"optimized"`
	LastOptimized    metav1.Time `json:"lastOptimized"`
}

type WeightOptimizerSpec struct {
	Endpoints []Endpoint `json:"endpoints,omitempty"`
}

// WeightOptimizerStatus defines the observed state of WeightOptimizer
type WeightOptimizerStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced

// WeightOptimizer is the Schema for the weightoptimizers API
type WeightOptimizer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WeightOptimizerSpec   `json:"spec,omitempty"`
	Status WeightOptimizerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WeightOptimizerList contains a list of WeightOptimizer
type WeightOptimizerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WeightOptimizer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WeightOptimizer{}, &WeightOptimizerList{})
}
