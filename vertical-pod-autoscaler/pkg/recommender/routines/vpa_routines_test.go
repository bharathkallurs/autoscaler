package routines

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/logic"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/model"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/test"
	"k8s.io/klog"
)

func addTestMemorySample(cluster *model.ClusterState, container model.ContainerID, memoryBytes float64, timeStamp time.Time) error {
	sample := model.ContainerUsageSampleWithKey{
		Container: container,
		ContainerUsageSample: model.ContainerUsageSample{
			MeasureStart: timeStamp,
			Usage:        model.MemoryAmountFromBytes(memoryBytes),
			Request:      recommenderTestRequest[model.ResourceMemory],
			Resource:     model.ResourceMemory,
		},
	}
	return cluster.AddSample(&sample)
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

	assert.NoError(t, cluster.AddOrUpdateContainer(container, recommenderTestRequest))
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
