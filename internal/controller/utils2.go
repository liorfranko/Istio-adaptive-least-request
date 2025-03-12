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
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	istioNetworkingV1 "istio.io/api/networking/v1"
	istioClientV1 "istio.io/client-go/pkg/apis/networking/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"istio-adaptive-least-request/internal/helpers"
	customMetrics "istio-adaptive-least-request/internal/metrics"
)

type tKeyToMuxKey struct {
	Namespace string
	Name      string
}

type tMarkMux struct {
	mux     sync.Mutex
	touched int64
}

func (m *tMarkMux) checkTouchedAndReset() bool {
	return atomic.SwapInt64(&m.touched, 0) > 0
}

var (
	keyToMuxMux sync.Mutex
	keyToMux    = make(map[tKeyToMuxKey]*tMarkMux)
)

func getMux(namespace, name string) *tMarkMux {
	key := tKeyToMuxKey{
		Namespace: namespace,
		Name:      name,
	}
	keyToMuxMux.Lock()
	defer keyToMuxMux.Unlock()
	markMux, ok := keyToMux[key]
	if !ok {
		markMux = new(tMarkMux)
		keyToMux[key] = markMux
	}
	return markMux
}

func tryLock(namespace, name string) (bool, func(), func() bool) {
	markMux := getMux(namespace, name)
	if !markMux.mux.TryLock() {
		atomic.AddInt64(&markMux.touched, 1)
		return false, nil, nil
	}
	return true, markMux.mux.Unlock, markMux.checkTouchedAndReset
}

func handleEndpointUpdate(
	ctx context.Context,
	logger logr.Logger,
	c client.Client,
	initialWeight uint32,
	serviceEntry *istioClientV1.ServiceEntry,
	localityEnabled bool,
) ([]*istioNetworkingV1.WorkloadEntry, bool, error) {
	ok, unlock, checkTouchedAndReset := tryLock(serviceEntry.Namespace, serviceEntry.Name)
	if !ok {
		return nil, false, nil
	}
	defer unlock()
	var endpointSlices discoveryv1.EndpointSliceList
	labelSelector := client.MatchingLabels{
		discoveryv1.LabelServiceName: serviceEntry.Name,
	}
	if err := c.List(ctx, &endpointSlices, client.InNamespace(serviceEntry.Namespace), labelSelector); err != nil {
		logger.Error(err, "Failed to list EndpointSlices for service",
			"Namespace", serviceEntry.Namespace,
			"Name", serviceEntry.Name,
		)
		return nil, checkTouchedAndReset(), err
	}
	if len(endpointSlices.Items) == 0 {
		logger.Info("No EndpointSlices found for service",
			"Namespace", serviceEntry.Namespace,
			"Name", serviceEntry.Name,
		)
		return nil, checkTouchedAndReset(), nil // No endpoints to process
	}
	addressToWorkloadEntry := make(map[string]*istioNetworkingV1.WorkloadEntry)
	for _, workloadEntry := range serviceEntry.Spec.Endpoints {
		addressToWorkloadEntry[workloadEntry.Address] = workloadEntry
	}
	existAddresses := make(map[string]struct{})
	newWorkloadEntries := make([]*istioNetworkingV1.WorkloadEntry, 0)
	for i := range endpointSlices.Items {
		endpointSlice := &endpointSlices.Items[i]
		if !endpointSlice.DeletionTimestamp.IsZero() {
			// Skip deleted EndpointSlices
			continue
		}
		for _, endpoint := range endpointSlice.Endpoints {
			if readyPtr := endpoint.Conditions.Ready; readyPtr == nil || !*readyPtr {
				logger.Info("Endpoint is not ready", "Endpoint", endpoint)
				continue
			}
			var address string
			if addresses := endpoint.Addresses; len(addresses) > 0 {
				address = addresses[0]
			}
			if address == "" {
				// TODO(romang): check why this happens, if it is
				continue
			}
			if _, ok := existAddresses[address]; ok {
				// Skip duplicate endpoints
				// https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/#duplicate-endpoints
				continue
			}
			existAddresses[address] = struct{}{}
			workloadEntry, ok := addressToWorkloadEntry[address]
			if !ok {
				workloadEntry = &istioNetworkingV1.WorkloadEntry{
					Address: address,
					Weight:  initialWeight,
				}
			}
			var locality string
			if localityEnabled {
				if zonePtr := endpoint.Zone; zonePtr != nil {
					locality = *zonePtr
					workloadEntry.Locality = locality[:len(locality)-1] + "/" + locality
				}
			}
			newWorkloadEntries = append(newWorkloadEntries, workloadEntry)
		}
	}

	var coreService corev1.Service
	objectKey := client.ObjectKey{
		Namespace: serviceEntry.Namespace,
		Name:      serviceEntry.Name,
	}
	if err := c.Get(ctx, objectKey, &coreService); err != nil {
		logger.Error(err, "Failed to fetch Service.")
		return nil, checkTouchedAndReset(), err
	}
	serviceEntry.Spec.Ports = appendCoreServicePortsToIstioServicePorts(serviceEntry.Spec.Ports[:0], coreService.Spec.Ports)

	// Check if updates are required based on endpoint changes.
	//if !(addressesChanged(serviceEntry.Spec.Endpoints, newWorkloadEntries) || checkPortsChanged(serviceEntry.Spec.Ports, service.Spec.ServicePorts)) {
	//	logger.Info("No changes detected", "ServiceEntry", serviceEntry.Name)
	//	return nil, err // No changes, no need to update.
	//}
	workloadEntriesDiff := helpers.Diff(nil, newWorkloadEntries, serviceEntry.Spec.Endpoints)
	//logger.Info("Detected endpoint changes", "ServiceEntry", serviceEntry.Name)

	if len(workloadEntriesDiff) == 0 {
		logger.Info("No changes detected", "ServiceEntry", serviceEntry.Name)
		return nil, checkTouchedAndReset(), nil // No changes, no need to update.
	}
	// Update the ServiceEntry with the newly merged endpoints.
	serviceEntry.Spec.Endpoints = newWorkloadEntries
	if err := c.Update(ctx, serviceEntry); err != nil {
		logger.Error(err, "Failed to update ServiceEntry", "ServiceEntry", serviceEntry.Name)
		return nil, checkTouchedAndReset(), err // Return the error to retry
	}
	updateMetrics(serviceEntry)
	logger.Info("ServiceEntry updated handleEndpointUpdate with new weights", "ServiceEntry", serviceEntry.Name, "ServiceEntry.Spec.Endpoints", serviceEntry.Spec.Endpoints)
	return workloadEntriesDiff, checkTouchedAndReset(), nil
}

func updateMetrics(serviceEntry *istioClientV1.ServiceEntry) {
	for _, workloadEntry := range serviceEntry.Spec.Endpoints {
		podAddress := workloadEntry.Address
		podZone := getLocalityForMetric(workloadEntry.Locality)
		podWeight := workloadEntry.Weight
		customMetrics.WeightMetric.WithLabelValues(serviceEntry.Namespace, serviceEntry.Name, podAddress, podZone).Set(float64(podWeight))
	}
}

func cleanupPodMetrics(oldWorkloadEntries []*istioNetworkingV1.WorkloadEntry, serviceEntryNamespace, serviceEntryName string) {
	for _, workloadEntry := range oldWorkloadEntries {
		podAddress := workloadEntry.Address
		podZone := getLocalityForMetric(workloadEntry.Locality)
		helpers.CleanupPodMetrics(serviceEntryNamespace, serviceEntryName, podAddress, podZone)
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
