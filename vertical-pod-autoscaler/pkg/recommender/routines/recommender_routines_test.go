package routines

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/logic"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/model"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/util"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/test"
	"k8s.io/klog"
)

// getOOMParam defines a function return type to handle tests
// for both OOMBumpUpRatio and OOMMinBUmp
type getOOMParam func(float64) float64

var (
	testPodID1       = model.PodID{Namespace: "namespace-1", PodName: "pod-1"}
	testLabels       = map[string]string{"label-1": "value-1"}
	timeLayout       = "2006-01-02 15:04:05"
	testTimestamp, _ = time.Parse(timeLayout, "2021-08-11 14:53:05")
	testRequest      = model.Resources{
		model.ResourceCPU:    model.CPUAmountFromCores(0.5),
		model.ResourceMemory: model.MemoryAmountFromBytes(100),
	}
	testLimit = model.Resources{ // Setting limits to 3x of testRequest
		model.ResourceCPU:    model.CPUAmountFromCores(1.0),
		model.ResourceMemory: model.MemoryAmountFromBytes(300),
	}
	testUpdateModeAuto = vpa_types.UpdateModeAuto
	testSelectorStr    = "label-1 = value-1"
	testVpaID          = model.VpaID{
		Namespace: testPodID1.Namespace,
		VpaName:   "testVPA",
	}
)

func addTestMemorySample(cluster *model.ClusterState, container model.ContainerID, memoryBytes float64, timeStamp time.Time) error {
	sample := model.ContainerUsageSampleWithKey{
		Container: container,
		ContainerUsageSample: model.ContainerUsageSample{
			MeasureStart: timeStamp,
			Usage:        model.MemoryAmountFromBytes(memoryBytes),
			Request:      testRequest[model.ResourceMemory],
			Resource:     model.ResourceMemory,
		},
	}
	return cluster.AddSample(&sample)
}

func addTestCPUHistogram(samples []float64, weight float64, testTimeStamp time.Time, options util.HistogramOptions) util.Histogram {
	cpuHistogram := util.NewHistogram(options)
	for _, sample := range samples {
		cpuHistogram.AddSample(sample, weight, testTimestamp)
	}

	return cpuHistogram
}

func addTestMemoryHistogram(samples []float64, weight float64, timeStamp time.Time, options util.HistogramOptions) util.Histogram {
	memoryHistogram := util.NewHistogram(options)
	for _, sample := range samples {
		memoryHistogram.AddSample(sample, weight, testTimestamp)
	}

	return memoryHistogram
}

func surgeSampleBasedMemoryEstimation(memorySample float64, maxAllowedMemory float64,
	cpuHistogram util.Histogram, memoryPeaksHistogram util.Histogram,
	cpuPercentile float64, memPercentile float64, fn getOOMParam) (model.Resources, int) {
	surgeSamplesCount := 1
	var resourceEstimation model.Resources
	for {
		if memorySample < maxAllowedMemory {
			memorySample = fn(memorySample)
		}
		memoryPeaksHistogram.AddSample(memorySample, 1.0, testTimestamp)
		resourceEstimation = GetPercentileResourceEstimation(cpuPercentile, memPercentile,
			cpuHistogram, memoryPeaksHistogram)

		if resourceEstimation[model.ResourceMemory] >= model.ResourceAmount(memorySample) {
			break
		}
		surgeSamplesCount++
	}

	return resourceEstimation, surgeSamplesCount
}

func GetPercentileResourceEstimation(cpuPercentile float64, memPercentile float64,
	cpuHistogram util.Histogram, memoryHistogram util.Histogram) model.Resources {

	estimator := logic.NewPercentileEstimator(cpuPercentile, memPercentile)

	return estimator.GetResourceEstimation(
		&model.AggregateContainerState{
			AggregateCPUUsage:    cpuHistogram,
			AggregateMemoryPeaks: memoryHistogram,
		})
}

func getTestVPAObj(cluster *model.ClusterState, container model.ContainerID) *model.Vpa {
	vpa := test.VerticalPodAutoscaler().WithName(testVpaID.VpaName).
		WithNamespace(testVpaID.Namespace).WithContainer(container.ContainerName).WithUpdateMode(testUpdateModeAuto).Get()

	labelSelector, _ := metav1.ParseToLabelSelector(testSelectorStr)
	parsedSelector, _ := metav1.LabelSelectorAsSelector(labelSelector)
	err := cluster.AddOrUpdateVpa(vpa, parsedSelector)
	if err != nil {
		klog.Fatalf("AddOrUpdateVpa() failed: %v", err)
	}

	return cluster.Vpas[testVpaID]
}

// Verifies that recommendation does not occur and remains Nil
// if only memory samples are added onto the aggregate container state
// histogram. The variable TotalSamplesCount is counted only for CPU
// samples and a similar for memory should be introduced.
func TestRecommendationForZeroTotalSamplesCount(t *testing.T) {
	cluster := model.NewClusterState()
	cluster.AddOrUpdatePod(testPodID1, testLabels, apiv1.PodRunning)
	container := model.ContainerID{PodID: testPodID1, ContainerName: "app-A"}

	assert.NoError(t, cluster.AddOrUpdateContainer(container, testRequest))
	assert.NoError(t, addTestMemorySample(cluster, container, 2e9, testTimestamp))

	pod := cluster.Pods[testPodID1]
	aggregateStateKey := cluster.MakeAggregateStateKey(pod, "app-A")
	aggregateResources := cluster.GetAggregateStateMap()
	// Note: This returns TotalSamplesCount as 0 as we are only adding memory sample
	// This is handled in another test case (TestTotalSamplesCountMemorySamplesOnly).
	// Ignorning and continuing to seek recommendations for now. Fix it to equal to 1.
	assert.Equal(t, 0, aggregateResources[aggregateStateKey].TotalSamplesCount)

	recommender := logic.CreatePodResourceRecommender()
	vpaObj := getTestVPAObj(cluster, container)
	recommendedResources := recommender.GetRecommendedPodResources(GetContainerNameToAggregateStateMap(vpaObj))
	assert.NotEmpty(t, recommendedResources)

}

// Verifies the number of samples it takes for the current
// VPA recommender estimation to beat the current surge in usage.
// The surge is determined and samples w.r.t that is added using
// model.OOMBumpUpRatio
func TestAddMemorySurgeWithOOMBumpUpRatio(t *testing.T) {
	config := model.GetAggregationsConfig()
	maxAllowedMemory := 5e10
	cpuSamples := []float64{1.0, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0}
	cpuHistogram := addTestCPUHistogram(cpuSamples, 1.0, testTimestamp, config.CPUHistogramOptions)

	var memSamples []float64
	for i := 0; i < 20; i++ {
		memSamples = append(memSamples, 1e6)
	}
	memoryPeaksHistogram := addTestMemoryHistogram(memSamples, 1.0, testTimestamp, config.MemoryHistogramOptions)

	ResourcePercentile := 0.9 // Same as in podResourceRecommender for target estimator
	resourceEstimation := GetPercentileResourceEstimation(ResourcePercentile, ResourcePercentile,
		cpuHistogram, memoryPeaksHistogram)
	maxRelativeError := 0.05 // Allow 5% relative error to account for histogram rounding.
	assert.InEpsilon(t, 1e7, model.BytesFromMemoryAmount(resourceEstimation[model.ResourceMemory]), maxRelativeError)

	// Adding sudden surge sample to memory histogram
	memoryPeaksHistogram.AddSample(4e10, 1.0, testTimestamp)
	resourceEstimation = GetPercentileResourceEstimation(ResourcePercentile, ResourcePercentile,
		cpuHistogram, memoryPeaksHistogram)

	// still recommends 1e6 value as memory recommendation.
	// Note: VPA recommender doesn't immediately respond to
	// surge of memory resource
	assert.InEpsilon(t, 1e7, model.BytesFromMemoryAmount(resourceEstimation[model.ResourceMemory]), maxRelativeError)

	surgeSample := 4e10
	// add a closure to increase memorySample by 1.2 times as defined in model.OOMBumpUpRatio
	getOOMBumpUpRatio := func(memorySample float64) float64 { return memorySample * model.OOMBumpUpRatio }
	resourceEstimation, surgeSamplesCount := surgeSampleBasedMemoryEstimation(surgeSample, maxAllowedMemory,
		cpuHistogram, memoryPeaksHistogram, ResourcePercentile, ResourcePercentile, getOOMBumpUpRatio)

	assert.InEpsilon(t, 60080658359, model.BytesFromMemoryAmount(resourceEstimation[model.ResourceMemory]), maxRelativeError)
	assert.Greater(t, 3, surgeSamplesCount) // Fail if greater than 3 surge samples collected

}

// Verifies the number of samples it takes for the current
// VPA recommender estimation to beat the current surge in usage.
// The surge is determined and samples w.r.t that is added using
// model.OOMMinBumpUp
func TestAddMemorySurgeWithOOMMinBump(t *testing.T) {
	config := model.GetAggregationsConfig()
	maxAllowedMemory := 5e10
	cpuSamples := []float64{1.0, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0}
	cpuHistogram := addTestCPUHistogram(cpuSamples, 1.0, testTimestamp, config.CPUHistogramOptions)

	var memSamples []float64
	for i := 0; i < 40; i++ {
		memSamples = append(memSamples, 1e6)
	}
	memoryPeaksHistogram := addTestMemoryHistogram(memSamples, 1.0, testTimestamp, config.MemoryHistogramOptions)

	ResourcePercentile := 0.9 // Same as in podResourceRecommender for target estimator
	resourceEstimation := GetPercentileResourceEstimation(ResourcePercentile, ResourcePercentile,
		cpuHistogram, memoryPeaksHistogram)
	maxRelativeError := 0.05 // Allow 5% relative error to account for histogram rounding.
	assert.InEpsilon(t, 1e7, model.BytesFromMemoryAmount(resourceEstimation[model.ResourceMemory]), maxRelativeError)

	// Adding sudden surge sample to memory histogram
	memoryPeaksHistogram.AddSample(4e10, 1.0, testTimestamp)
	resourceEstimation = GetPercentileResourceEstimation(ResourcePercentile, ResourcePercentile,
		cpuHistogram, memoryPeaksHistogram)

	// still recommends 1e6 value as memory recommendation.
	// Note: VPA recommender doesn't immediately respond to
	// surge of memory resource
	assert.InEpsilon(t, 1e7, model.BytesFromMemoryAmount(resourceEstimation[model.ResourceMemory]), maxRelativeError)

	surgeSample := 4e10
	// add a closure to increase memorySample by 100MB as defined in model.OOMMinBumUp
	getOOMBumpUpRatio := func(memorySample float64) float64 { return memorySample + model.OOMMinBumpUp }
	resourceEstimation, surgeSamplesCount := surgeSampleBasedMemoryEstimation(surgeSample, maxAllowedMemory,
		cpuHistogram, memoryPeaksHistogram, ResourcePercentile, ResourcePercentile, getOOMBumpUpRatio)

	assert.InEpsilon(t, 40600322346, model.BytesFromMemoryAmount(resourceEstimation[model.ResourceMemory]), maxRelativeError)
	assert.Greater(t, 3, surgeSamplesCount) // Fail if greater than 3 surge samples collected

}

// Showcases the need of containerState.RecordOOM like function for
// recording CPU throttle information which should be added to CPU
// histogram if the CPU usage is constantly above current pod limits set.
// CPU throttle information recorded should later be bumping the CPU sample
// similar to containerState.RecordOOM to ensure higher recommendation is
// provided for preventing CPU starvation
func TestRecordCPUThrottlingInformation(t *testing.T) {
	config := model.GetAggregationsConfig()
	// include CPU samples greater than current limits to showcase throttle
	cpuSamples := []float64{0.9, 0.95, 1.0, 1.2, 1.3, 1.4, 1.5, 1.6, 1.8, 1.9}
	cpuHistogram := addTestCPUHistogram(cpuSamples, 1.0, testTimestamp, config.CPUHistogramOptions)

	var memSamples []float64
	for i := 0; i < 20; i++ {
		memSamples = append(memSamples, 1e6)
	}
	memoryPeaksHistogram := addTestMemoryHistogram(memSamples, 1.0, testTimestamp, config.MemoryHistogramOptions)

	ResourcePercentile := 0.9 // Same as in podResourceRecommender for target estimator
	resourceEstimation := GetPercentileResourceEstimation(ResourcePercentile, ResourcePercentile,
		cpuHistogram, memoryPeaksHistogram)

	resourceRecommendation := model.CoresFromCPUAmount(resourceEstimation[model.ResourceCPU])
	cpuRequestsToLimitsRatio := float64(testLimit[model.ResourceCPU] / testRequest[model.ResourceCPU])
	newCPULimitBasedOnRecommendation := cpuRequestsToLimitsRatio * float64(testLimit[model.ResourceCPU])
	// calculate 90% of post recommendation CPU limits value
	cpuThrottleLimit := 0.9 * model.CoresFromCPUAmount(model.ResourceAmount(newCPULimitBasedOnRecommendation))

	// assert if the current CPU recommendation >= 90% of CPU Limits post recommendation
	// This should hence show a requirement of RecordCPUThrottle like containerState.RecordOOM
	// which bumps the CPU sample by some value before adding it to the histogram
	assert.LessOrEqual(t, resourceRecommendation, cpuThrottleLimit)
}
