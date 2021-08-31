package routines

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/logic"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/model"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/util"
)

var (
	config                 = model.GetAggregationsConfig()
	testPodID1             = model.PodID{Namespace: "namespace-1", PodName: "pod-1"}
	testLabels             = map[string]string{"label-1": "value-1"}
	testCtrName            = "app-1"
	timeLayout             = "2006-01-02 15:04:05"
	testTimestamp, _       = time.Parse(timeLayout, "2021-08-11 14:53:05")
	maxRelativeError       = 0.05
	maxAllowedMemory       = 5e10
	recommenderTestRequest = model.Resources{
		model.ResourceCPU:    model.CPUAmountFromCores(0.5),
		model.ResourceMemory: model.MemoryAmountFromBytes(100),
	}
	recommenderTestLimit = model.Resources{
		model.ResourceCPU:    model.CPUAmountFromCores(1.0),
		model.ResourceMemory: model.MemoryAmountFromBytes(200),
	}
	testUpdateModeAuto = vpa_types.UpdateModeAuto
	testSelectorStr    = "label-1 = value-1"
	testVpaID          = model.VpaID{
		Namespace: testPodID1.Namespace,
		VpaName:   "testVPA",
	}
)

type resourceSample struct {
	value     float64
	weight    float64
	timeStamp time.Time
}

func addResourceSample(samples []resourceSample, histogram *util.Histogram) {
	for i := range samples {
		(*histogram).AddSample(samples[i].value, samples[i].weight, samples[i].timeStamp)
	}
}

func addTestCPUHistogram(cpuHistogram util.Histogram, cpuSamples []resourceSample) {
	addResourceSample(cpuSamples, &cpuHistogram)
}

func addTestMemoryHistogram(memoryHistogram util.Histogram, memorySamples []resourceSample) {
	addResourceSample(memorySamples, &memoryHistogram)
}

func getTestContainerStateMap(cpuHistogram util.Histogram, memoryHistogram util.Histogram) model.ContainerNameToAggregateStateMap {
	return model.ContainerNameToAggregateStateMap{
		testCtrName: &model.AggregateContainerState{
			AggregateCPUUsage:    cpuHistogram,
			AggregateMemoryPeaks: memoryHistogram,
		},
	}
}

func doTestRecommender(
	cpuHistogram, memoryHistogram util.Histogram,
	initialCPUSamples, initialMemorySamples []resourceSample,
	recommender logic.PodResourceRecommender,
	containerStateMap model.ContainerNameToAggregateStateMap,
	assertInitialRecommendations func(logic.RecommendedPodResources),
	doTestAction func(),
	assertPostTestActionRecommendations func(logic.RecommendedPodResources),
) {
	addTestCPUHistogram(cpuHistogram, initialCPUSamples)          // Add CPU histogram
	addTestMemoryHistogram(memoryHistogram, initialMemorySamples) // Add memory histogram

	// Get initial recommendation
	recommendedResources := recommender.GetRecommendedPodResources(containerStateMap)
	assertInitialRecommendations(recommendedResources) // Assert initial recommendation

	doTestAction() // Actual test logic

	// Get post test action recommendation
	recommendedResources = recommender.GetRecommendedPodResources(containerStateMap)
	assertPostTestActionRecommendations(recommendedResources) // Assert post test results
}

func TestAddMemorySurgeWithOOMBumpUpRatio(t *testing.T) {
	cpuHistogram := util.NewHistogram(config.CPUHistogramOptions)
	memoryHistogram := util.NewHistogram(config.MemoryHistogramOptions)
	surgeSamplesCount := 1

	var cpuSamples []resourceSample
	cpuCores := []float64{0.5, 0.5, 0.55, 0.55, 0.6, 0.6, 0.65, 0.65, 0.7, 0.7}
	cpuTimeStamp := testTimestamp
	for i := 0; i < 10; i++ {
		cpuTimeStamp = cpuTimeStamp.Add(1 * time.Minute)
		cpuSamples = append(cpuSamples, resourceSample{cpuCores[i], 1.0, cpuTimeStamp})
	}

	var memSamples []resourceSample
	memorySample := 1e6
	memTimeStamp := testTimestamp
	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			memorySample += float64(100 * i)
		} else {
			memorySample -= float64(100 * i)
		}

		memTimeStamp = memTimeStamp.Add(1 * time.Minute)
		memSamples = append(memSamples, resourceSample{memorySample, 1.0, memTimeStamp})
	}

	containerStateMap := getTestContainerStateMap(cpuHistogram, memoryHistogram)
	recommender := logic.CreatePodResourceRecommender()

	doTestRecommender(
		cpuHistogram,
		memoryHistogram,
		cpuSamples,
		memSamples,
		recommender,
		containerStateMap,
		func(recommendedResources logic.RecommendedPodResources) {
			assert.InEpsilon(t, 2.62144000e8, model.BytesFromMemoryAmount(recommendedResources[testCtrName].Target[model.ResourceMemory]), maxRelativeError)
		},
		func() {
			addTestMemoryHistogram(memoryHistogram,
				[]resourceSample{
					{4e10, 1.0, testTimestamp},
				})

			containerStateMap[testCtrName].AggregateCPUUsage = cpuHistogram
			containerStateMap[testCtrName].AggregateMemoryPeaks = memoryHistogram
			recommendedResources := recommender.GetRecommendedPodResources(containerStateMap)
			assert.InEpsilon(t, 2.62144000e8, model.BytesFromMemoryAmount(recommendedResources[testCtrName].Target[model.ResourceMemory]), maxRelativeError)

			surgeSample := resourceSample{
				value:     4e10,
				weight:    1.0,
				timeStamp: testTimestamp,
			}
			addTestMemoryHistogram(memoryHistogram, []resourceSample{surgeSample})

			for {
				if surgeSample.value < maxAllowedMemory {
					surgeSample.value *= model.OOMBumpUpRatio
				}
				addTestMemoryHistogram(memoryHistogram, []resourceSample{surgeSample})
				containerStateMap[testCtrName].AggregateCPUUsage = cpuHistogram
				containerStateMap[testCtrName].AggregateMemoryPeaks = memoryHistogram
				recommendedResources = recommender.GetRecommendedPodResources(containerStateMap)
				assert.NotEmpty(t, recommendedResources)

				if recommendedResources[testCtrName].Target[model.ResourceMemory] >= model.ResourceAmount(surgeSample.value) {
					break
				}
				surgeSamplesCount++
			}
		},
		func(recommendedResources logic.RecommendedPodResources) {
			assert.InEpsilon(t, 6.9092757112e10, model.BytesFromMemoryAmount(recommendedResources[testCtrName].Target[model.ResourceMemory]), maxRelativeError)
			assert.Greater(t, 3, surgeSamplesCount) // Fail if greater than 3 surge samples collected
		},
	)
}

func TestAddMemorySurgeWithOOMMinBump(t *testing.T) {
	cpuHistogram := util.NewHistogram(config.CPUHistogramOptions)
	memoryHistogram := util.NewHistogram(config.MemoryHistogramOptions)
	surgeSamplesCount := 1

	var cpuSamples []resourceSample
	cpuCores := []float64{0.5, 0.5, 0.55, 0.55, 0.6, 0.6, 0.65, 0.65, 0.7, 0.7}
	cpuTimeStamp := testTimestamp
	for i := 0; i < 10; i++ {
		cpuTimeStamp = cpuTimeStamp.Add(1 * time.Minute)
		cpuSamples = append(cpuSamples, resourceSample{cpuCores[i], 1.0, cpuTimeStamp})
	}

	var memSamples []resourceSample
	memorySample := 1e6
	memTimeStamp := testTimestamp
	for i := 0; i < 40; i++ {
		if i%2 == 0 {
			memorySample += float64(100 * i)
		} else {
			memorySample -= float64(100 * i)
		}

		memTimeStamp = memTimeStamp.Add(1 * time.Minute)
		memSamples = append(memSamples, resourceSample{memorySample, 1.0, memTimeStamp})
	}

	containerStateMap := getTestContainerStateMap(cpuHistogram, memoryHistogram)
	recommender := logic.CreatePodResourceRecommender()

	doTestRecommender(
		cpuHistogram,
		memoryHistogram,
		cpuSamples,
		memSamples,
		recommender,
		containerStateMap,
		func(recommendedResources logic.RecommendedPodResources) {
			assert.InEpsilon(t, 2.62144000e8, model.BytesFromMemoryAmount(recommendedResources[testCtrName].Target[model.ResourceMemory]), maxRelativeError)
		},
		func() {
			addTestMemoryHistogram(memoryHistogram,
				[]resourceSample{
					{4e10, 1.0, testTimestamp},
				})

			containerStateMap[testCtrName].AggregateCPUUsage = cpuHistogram
			containerStateMap[testCtrName].AggregateMemoryPeaks = memoryHistogram
			recommendedResources := recommender.GetRecommendedPodResources(containerStateMap)
			assert.InEpsilon(t, 2.62144000e8, model.BytesFromMemoryAmount(recommendedResources[testCtrName].Target[model.ResourceMemory]), maxRelativeError)

			surgeSample := resourceSample{
				value:     4e10,
				weight:    1.0,
				timeStamp: testTimestamp,
			}
			addTestMemoryHistogram(memoryHistogram, []resourceSample{surgeSample})

			for {
				if surgeSample.value < maxAllowedMemory {
					surgeSample.value += model.OOMMinBumpUp
				}
				addTestMemoryHistogram(memoryHistogram, []resourceSample{surgeSample})

				recommendedResources = recommender.GetRecommendedPodResources(containerStateMap)
				assert.NotEmpty(t, recommendedResources)

				if recommendedResources[testCtrName].Target[model.ResourceMemory] >= model.ResourceAmount(surgeSample.value) {
					break
				}

				surgeSamplesCount++
			}
		},
		func(recommendedResources logic.RecommendedPodResources) {
			assert.InEpsilon(t, 4.6690370697e10, model.BytesFromMemoryAmount(recommendedResources[testCtrName].Target[model.ResourceMemory]), maxRelativeError)
			assert.Greater(t, 3, surgeSamplesCount) // Fail if greater than 3 surge samples collected
		},
	)
}

func TestRecordCPUThrottlingInformation(t *testing.T) {
	var resourceRecommendation float64

	cpuHistogram := util.NewHistogram(config.CPUHistogramOptions)
	memoryHistogram := util.NewHistogram(config.MemoryHistogramOptions)

	var cpuSamples []resourceSample
	// samples resulting in a 90th percentile value of ~0.958
	cpuCores := []float64{0.8, 0.85, 0.88, 0.9, 0.9, 0.91, 0.92, 0.93, 0.94, 0.95}
	cpuTimeStamp := testTimestamp
	for i := 0; i < 10; i++ {
		cpuTimeStamp = cpuTimeStamp.Add(1 * time.Minute)
		cpuSamples = append(cpuSamples, resourceSample{cpuCores[i], 1.0, cpuTimeStamp})
	}

	var memSamples []resourceSample
	memorySample := 1e6
	memTimeStamp := testTimestamp
	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			memorySample += float64(100 * i)
		} else {
			memorySample -= float64(100 * i)
		}

		memTimeStamp = memTimeStamp.Add(1 * time.Minute)
		memSamples = append(memSamples, resourceSample{memorySample, 1.0, memTimeStamp})
	}

	containerStateMap := getTestContainerStateMap(cpuHistogram, memoryHistogram)
	recommender := logic.CreatePodResourceRecommender()

	doTestRecommender(
		cpuHistogram,
		memoryHistogram,
		cpuSamples,
		memSamples,
		recommender,
		containerStateMap,
		func(recommendedResources logic.RecommendedPodResources) {
			percentileValue := 0.9
			cpuSample90Percentile := cpuHistogram.Percentile(percentileValue)
			currentCPULimit90Percent := 0.9 * model.CoresFromCPUAmount(recommenderTestLimit[model.ResourceCPU])
			// showcase that current cpuSample90Percentile is already
			// greater than 90 percent of current CPU limit
			assert.LessOrEqual(t, cpuSample90Percentile, currentCPULimit90Percent)

			resourceRecommendation = model.CoresFromCPUAmount(recommendedResources[testCtrName].Target[model.ResourceCPU])

			// Check if 90th percentile CPU value is almost same as target resource recommendation
			maxErrorTolerance := 0.15 // margin estimation sets 15% tolerance to calculate overall recommendation
			assert.InEpsilon(t, cpuSample90Percentile, resourceRecommendation, maxErrorTolerance)

			// CPU resourceRecommendation is greater than 90% of current CPU limits
			assert.LessOrEqual(t, resourceRecommendation, currentCPULimit90Percent)
		},
		func() {},
		func(recommendedResources logic.RecommendedPodResources) {},
	)
}

func TestRecommenderScaleUp(t *testing.T) {
	cpuHistogram := util.NewHistogram(config.CPUHistogramOptions)
	memoryHistogram := util.NewHistogram(config.MemoryHistogramOptions)
	weights := []float64{0.5, 0.5, 0.5, 0.7, 0.7, 0.7, 0.9, 0.9, 0.9, 0.9}

	var cpuSamples []resourceSample
	cpuCores := []float64{1.0, 1.1, 1.15, 1.2, 1.25, 1.3, 1.35, 1.4, 1.45, 1.5}
	cpuTimeStamp := testTimestamp
	for i := 0; i < 10; i++ {
		cpuTimeStamp = cpuTimeStamp.Add(1 * time.Minute)
		cpuSamples = append(cpuSamples, resourceSample{cpuCores[i], weights[i], cpuTimeStamp})
	}

	var memSamples []resourceSample
	memTimeStamp := testTimestamp
	memorySample := 1e6
	for i := 1; i <= 10; i++ {
		if i%2 == 0 {
			memorySample += float64(100 * i)
		} else {
			memorySample -= float64(100 * i)
		}

		memTimeStamp = memTimeStamp.Add(1 * time.Minute)
		memSamples = append(memSamples, resourceSample{memorySample, 1.0, memTimeStamp})
	}

	containerStateMap := getTestContainerStateMap(cpuHistogram, memoryHistogram)
	recommender := logic.CreatePodResourceRecommender()

	doTestRecommender(
		cpuHistogram,
		memoryHistogram,
		cpuSamples,
		memSamples,
		recommender,
		containerStateMap,
		func(recommendedResources logic.RecommendedPodResources) {
			assert.InEpsilon(t, 1.737, model.CoresFromCPUAmount(recommendedResources[testCtrName].Target[model.ResourceCPU]), maxRelativeError)
			assert.InEpsilon(t, 2.62144000e8, model.BytesFromMemoryAmount(recommendedResources[testCtrName].Target[model.ResourceMemory]), maxRelativeError)
		},
		func() {
			increasedCPUCores := []float64{1.5, 1.55, 1.6, 1.65, 1.7, 1.75, 1.75, 1.75, 1.75, 1.75}
			for i := 0; i < 10; i++ {
				cpuTimeStamp = cpuTimeStamp.Add(1 * time.Minute)
				cpuSamples = append(cpuSamples, resourceSample{increasedCPUCores[i], weights[i], cpuTimeStamp})
			}
			addTestCPUHistogram(cpuHistogram, cpuSamples)

			for i := 0; i < 10; i++ {
				memTimeStamp = memTimeStamp.Add(1 * time.Minute)
				memSamples = append(memSamples, resourceSample{float64(1e9 + i*100), weights[i], memTimeStamp})
			}
			addTestMemoryHistogram(memoryHistogram, memSamples)
		},
		func(recommendedResources logic.RecommendedPodResources) {
			assert.InEpsilon(t, 2.048, model.CoresFromCPUAmount(recommendedResources[testCtrName].Target[model.ResourceCPU]), maxRelativeError)
			assert.InEpsilon(t, 1.168723596e9, model.BytesFromMemoryAmount(recommendedResources[testCtrName].Target[model.ResourceMemory]), maxRelativeError)
			assert.InEpsilon(t, 0.025, model.CoresFromCPUAmount(recommendedResources[testCtrName].LowerBound[model.ResourceCPU]), maxRelativeError)
			assert.InEpsilon(t, 2.62144000e8, model.BytesFromMemoryAmount(recommendedResources[testCtrName].LowerBound[model.ResourceMemory]), maxRelativeError)
			assert.InEpsilon(t, 1e11, model.CoresFromCPUAmount(recommendedResources[testCtrName].UpperBound[model.ResourceCPU]), maxRelativeError)
			assert.InEpsilon(t, 1e14, model.BytesFromMemoryAmount(recommendedResources[testCtrName].UpperBound[model.ResourceMemory]), maxRelativeError)
		},
	)
}

func TestRecommenderScaleDown(t *testing.T) {
	cpuHistogram := util.NewHistogram(config.CPUHistogramOptions)
	memoryHistogram := util.NewHistogram(config.MemoryHistogramOptions)
	weights := []float64{0.5, 0.5, 0.5, 0.7, 0.7, 0.7, 0.9, 0.9, 0.9, 0.9}

	var cpuSamples []resourceSample
	cpuCores := []float64{0.9, 0.8, 0.7, 0.6, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5}
	cpuTimeStamp := testTimestamp
	for i := 0; i < 10; i++ {
		cpuTimeStamp = cpuTimeStamp.Add(1 * time.Minute)
		cpuSamples = append(cpuSamples, resourceSample{cpuCores[i], weights[i], cpuTimeStamp})
	}

	var memSamples []resourceSample
	memTimeStamp := testTimestamp
	memorySample := 1e5
	for i := 1; i <= 10; i++ {
		memorySample -= float64(i * 1000)
		memTimeStamp = memTimeStamp.Add(1 * time.Minute)
		memSamples = append(memSamples, resourceSample{memorySample, 1.0, memTimeStamp})
	}

	containerStateMap := getTestContainerStateMap(cpuHistogram, memoryHistogram)
	recommender := logic.CreatePodResourceRecommender()

	doTestRecommender(
		cpuHistogram,
		memoryHistogram,
		cpuSamples,
		memSamples,
		recommender,
		containerStateMap,
		func(recommendedResources logic.RecommendedPodResources) {
			assert.InEpsilon(t, 0.92, model.CoresFromCPUAmount(recommendedResources[testCtrName].Target[model.ResourceCPU]), maxRelativeError)
			assert.InEpsilon(t, 2.62144000e8, model.BytesFromMemoryAmount(recommendedResources[testCtrName].Target[model.ResourceMemory]), maxRelativeError)
		},
		func() {
			decreasedCPUCores := []float64{0.3, 0.3, 0.3, 0.2, 0.2, 0.2, 0.2, 0.1, 0.1, 0.05}
			recentWeights := []float64{0.9, 0.9, 0.9, 0.9, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0}
			for i := 0; i < 10; i++ {
				cpuTimeStamp = cpuTimeStamp.Add(1 * time.Minute)
				cpuSamples = append(cpuSamples, resourceSample{decreasedCPUCores[i], recentWeights[i], cpuTimeStamp})
			}
			addTestCPUHistogram(cpuHistogram, cpuSamples)

			for i := 0; i < 10; i++ {
				memTimeStamp = memTimeStamp.Add(1 * time.Minute)
				memSamples = append(memSamples, resourceSample{float64(1e5 - i*1000), weights[i], memTimeStamp})
			}
			addTestMemoryHistogram(memoryHistogram, memSamples)
		},
		func(recommendedResources logic.RecommendedPodResources) {
			assert.InEpsilon(t, 0.813, model.CoresFromCPUAmount(recommendedResources[testCtrName].Target[model.ResourceCPU]), maxRelativeError)
			assert.InEpsilon(t, 2.62144000e8, model.BytesFromMemoryAmount(recommendedResources[testCtrName].Target[model.ResourceMemory]), maxRelativeError)
			assert.InEpsilon(t, 0.025, model.CoresFromCPUAmount(recommendedResources[testCtrName].LowerBound[model.ResourceCPU]), maxRelativeError)
			assert.InEpsilon(t, 2.62144000e8, model.BytesFromMemoryAmount(recommendedResources[testCtrName].LowerBound[model.ResourceMemory]), maxRelativeError)
			assert.InEpsilon(t, 1e11, model.CoresFromCPUAmount(recommendedResources[testCtrName].UpperBound[model.ResourceCPU]), maxRelativeError)
			assert.InEpsilon(t, 1e14, model.BytesFromMemoryAmount(recommendedResources[testCtrName].UpperBound[model.ResourceMemory]), maxRelativeError)
		},
	)
}
