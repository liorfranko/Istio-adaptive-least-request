package controller

import (
	"context"
	"fmt"
	"github.com/montanaflynn/stats"
	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"testing"
)

func formatPodMetrics(podsMetrics map[string]*PodMetrics) string {
	result := ""
	for key, value := range podsMetrics {
		result += fmt.Sprintf("%s: {PodName: %s, PodAddress: %s, CPUTime: %.2f} ", key, value.PodName, value.PodAddress, value.CPUTime)
	}
	return result
}

func (r *WeightOptimizerReconciler) newCakeTrickTest(ctx context.Context, podsMetrics *map[string]*PodMetrics, serviceEntryWeightsMap map[string]uint32) (map[string]uint32, map[string]float64, error) {
	logger := log.FromContext(ctx).WithName(r.LoggerName)
	if len(*podsMetrics) == 0 {
		return nil, nil, fmt.Errorf("no metrics to process")
	}

	// Calculate total weight
	totalWeight := 0.0
	for _, weight := range serviceEntryWeightsMap {
		totalWeight += float64(weight)
	}
	logger.V(1).Info("Total weight", "totalWeight", totalWeight)
	fmt.Println("Total weight", totalWeight)
	// Calculate average CPU time
	var cpuTimes []float64
	for _, ep := range *podsMetrics {
		if ep.CPUTime != 0.0 {
			cpuTimes = append(cpuTimes, ep.CPUTime)
		}
		logger.V(1).Info("Collected Pod metrics", "PodName", ep.PodName, "PodAddress", ep.PodAddress, "CPUTime", ep.CPUTime, "Weight", serviceEntryWeightsMap[ep.PodName])
	}
	//print pods * (MinimumWeight) * 0.5
	fmt.Println("pods * (MinimumWeight)", float64(len(cpuTimes))*float64(50))
	// If totalWeight is less than pods * (MinimumWeight), then multiply totalWeight until its more of 2 times the maximum weights * number of pods. use len of cpu time
	if totalWeight < float64(len(cpuTimes))*float64(50) {
		totalWeight = float64(len(cpuTimes)) * float64(1000) * 2
		logger.V(1).Info("Total weight is less than minimum, setting it to 2 times the maximum weight * number of pods", "newTotalWeight", totalWeight, "minimumWeight", float64(len(cpuTimes))*float64(r.MinimumWeight)*0.5, "maximumWeight", r.MaximumWeight, "number of pods", len(cpuTimes))
		fmt.Println("Total weight is less than minimum, setting it to 2 times the maximum weight * number of pods", "newTotalWeight", totalWeight, "minimumWeight", float64(len(cpuTimes))*float64(r.MinimumWeight)*0.5, "maximumWeight", r.MaximumWeight, "number of pods", len(cpuTimes))
	}
	averageCPU, _ := stats.Mean(cpuTimes)
	logger.V(1).Info("averageCPU", "averageCPU", averageCPU)

	// Calculate X_sum
	var X_sum float64
	for _, ep := range *podsMetrics {
		if ep.CPUTime != 0.0 {
			X_sum += averageCPU / ep.CPUTime * float64(serviceEntryWeightsMap[ep.PodName])
		}
	}

	// Scaling factor for less aggressive changes
	scalingFactor := 0.20
	// Calculate average total weight based on the number of entries in the serviceEntryWeightsMap
	avgTotalWeight := totalWeight / float64(len(serviceEntryWeightsMap))
	// Redistribute weights and estimate new CPU times
	EstimatedCpuTimes := make(map[string]float64)
	for _, ep := range *podsMetrics {
		if ep.CPUTime != 0.0 {
			// Calculate new share and adjusted weight
			newShare := (averageCPU / ep.CPUTime) * float64(serviceEntryWeightsMap[ep.PodName]) / X_sum * totalWeight
			currentWeight := float64(serviceEntryWeightsMap[ep.PodName])
			adjustedWeight := currentWeight + scalingFactor*(newShare-currentWeight)
			// Avoid huge spikes by limiting weight changes

			// Calculate the distance between adjustedWeight and currentWeight, and the limit for adjustment
			distance := adjustedWeight - currentWeight
			maxAllowedDistance := avgTotalWeight / 10

			// If the distance exceeds the limit, cap the adjusted weight
			if distance > maxAllowedDistance {
				fmt.Println("Distance exceeds the allowable limit, adjusting weight", "PodName", ep.PodName, "PodAddress", ep.PodAddress, "CurrentWeight", currentWeight, "AdjustedWeight", adjustedWeight, "Distance", distance, "MaxAllowedDistance", maxAllowedDistance)

				// Limit the adjusted weight to avoid large spikes
				adjustedWeight = currentWeight + maxAllowedDistance
				fmt.Println("Adjusted weight after limiting", adjustedWeight)
			}

			// Log final adjusted weight before updating the map
			//fmt.Println("Updating service entry with final adjusted weight", "PodName", ep.PodName, "PodAddress", ep.PodAddress, "FinalAdjustedWeight", adjustedWeight)
			// Update the service entry weights map with the new adjusted weight
			serviceEntryWeightsMap[ep.PodName] = uint32(adjustedWeight)

			// Estimate new CPU time based on the adjusted weight
			if adjustedWeight != 0 {
				EstimatedCpuTime := ep.CPUTime * (adjustedWeight / currentWeight)
				EstimatedCpuTimes[ep.PodName] = EstimatedCpuTime
				logger.Info("New weight and CPU time", "PodName", ep.PodName, "PodAddress", ep.PodAddress, "NewWeight", adjustedWeight, "EstimatedCpuTime", EstimatedCpuTime)
			} else {
				EstimatedCpuTimes[ep.PodName] = ep.CPUTime
				logger.Info("Adjusted weight is zero, keeping original CPU time", "PodName", ep.PodName, "PodAddress", ep.PodAddress, "CPUTime", ep.CPUTime)
			}
		}
	}

	return serviceEntryWeightsMap, EstimatedCpuTimes, nil
}

func TestNewCakeTrick(t *testing.T) {
	ctx := context.TODO()
	reconciler := &WeightOptimizerReconciler{LoggerName: "TestLogger"}

	podsMetrics := map[string]*PodMetrics{
		"A": {PodName: "A", PodAddress: "1.1.1.1", CPUTime: 90, Weight: 100},
		"B": {PodName: "B", PodAddress: "2.2.2.2", CPUTime: 110, Weight: 100},
		"C": {PodName: "C", PodAddress: "3.3.3.3", CPUTime: 160, Weight: 800},
	}

	serviceEntryWeightsMap := map[string]uint32{
		"A": 100,
		"B": 120,
		"C": 800,
	}
	fmt.Printf("Initial serviceEntryWeightsMap: %v\n", serviceEntryWeightsMap)
	fmt.Printf("Initial podsMetrics: \n%s", formatPodMetrics(podsMetrics))
	for i := 0; i < 10; i++ {
		newWeightsMap, newCpuTimes, err := reconciler.newCakeTrickTest(ctx, &podsMetrics, serviceEntryWeightsMap)
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		fmt.Printf("Run %d - newWeightsMap: %v\n", i, newWeightsMap)
		fmt.Printf("Run %d - newCpuTimes: %v\n", i, newCpuTimes)

		// Update the pod metrics with the new CPU times for the next iteration
		for PodName, newCpuTime := range newCpuTimes {
			podsMetrics[PodName].CPUTime = newCpuTime
		}
		serviceEntryWeightsMap = newWeightsMap

	}

	// Check final state if necessary
	expectedWeightsMap := map[string]uint32{
		"A": 513,
		"B": 501,
		"C": 485,
	}

	expectedCpuTimes := map[string]float64{
		"A": 89.0,
		"B": 109.0,
		"C": 159.0,
	}

	finalWeightsMap, expectedCpuTimes, err := reconciler.newCakeTrickTest(ctx, &podsMetrics, serviceEntryWeightsMap)
	fmt.Println("Final newWeightsMap", finalWeightsMap)
	fmt.Println("expectedWeightsMap", expectedWeightsMap)
	fmt.Println("Final expectedCpuTimes", expectedCpuTimes)
	assert.NoError(t, err)

}
