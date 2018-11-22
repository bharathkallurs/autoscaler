/*
Copyright 2016 The Kubernetes Authors.

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
	ctx "context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiserverconfig "k8s.io/apiserver/pkg/apis/config"
	kube_flag "k8s.io/apiserver/pkg/util/flag"
	cloudBuilder "github.com/gardener/autoscaler/cluster-autoscaler/cloudprovider/builder"
	"github.com/gardener/autoscaler/cluster-autoscaler/config"
	"github.com/gardener/autoscaler/cluster-autoscaler/core"
	"github.com/gardener/autoscaler/cluster-autoscaler/estimator"
	"github.com/gardener/autoscaler/cluster-autoscaler/expander"
	"github.com/gardener/autoscaler/cluster-autoscaler/metrics"
	"github.com/gardener/autoscaler/cluster-autoscaler/utils/errors"
	kube_util "github.com/gardener/autoscaler/cluster-autoscaler/utils/kubernetes"
	"github.com/gardener/autoscaler/cluster-autoscaler/utils/units"
	kube_client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/kubernetes/pkg/client/leaderelectionconfig"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/pflag"
)

// MultiStringFlag is a flag for passing multiple parameters using same flag
type MultiStringFlag []string

// String returns string representation of the node groups.
func (flag *MultiStringFlag) String() string {
	return "[" + strings.Join(*flag, " ") + "]"
}

// Set adds a new configuration.
func (flag *MultiStringFlag) Set(value string) error {
	*flag = append(*flag, value)
	return nil
}

func multiStringFlag(name string, usage string) *MultiStringFlag {
	value := new(MultiStringFlag)
	flag.Var(value, name, usage)
	return value
}

var (
	clusterName            = flag.String("cluster-name", "", "Autoscaled cluster name, if available")
	address                = flag.String("address", ":8085", "The address to expose prometheus metrics.")
	kubernetes             = flag.String("kubernetes", "", "Kubernetes master location. Leave blank for default")
	kubeConfigFile         = flag.String("kubeconfig", "", "Path to kubeconfig file with authorization and master location information.")
	cloudConfig            = flag.String("cloud-config", "", "The path to the cloud provider configuration file.  Empty string for no configuration file.")
	namespace              = flag.String("namespace", "kube-system", "Namespace in which cluster-autoscaler run.")
	scaleDownEnabled       = flag.Bool("scale-down-enabled", true, "Should CA scale down the cluster")
	scaleDownDelayAfterAdd = flag.Duration("scale-down-delay-after-add", 10*time.Minute,
		"How long after scale up that scale down evaluation resumes")
	scaleDownDelayAfterDelete = flag.Duration("scale-down-delay-after-delete", *scanInterval,
		"How long after node deletion that scale down evaluation resumes, defaults to scanInterval")
	scaleDownDelayAfterFailure = flag.Duration("scale-down-delay-after-failure", 3*time.Minute,
		"How long after scale down failure that scale down evaluation resumes")
	scaleDownUnneededTime = flag.Duration("scale-down-unneeded-time", 10*time.Minute,
		"How long a node should be unneeded before it is eligible for scale down")
	scaleDownUnreadyTime = flag.Duration("scale-down-unready-time", 20*time.Minute,
		"How long an unready node should be unneeded before it is eligible for scale down")
	scaleDownUtilizationThreshold = flag.Float64("scale-down-utilization-threshold", 0.5,
		"Node utilization level, defined as sum of requested resources divided by capacity, below which a node can be considered for scale down")
	scaleDownNonEmptyCandidatesCount = flag.Int("scale-down-non-empty-candidates-count", 30,
		"Maximum number of non empty nodes considered in one iteration as candidates for scale down with drain."+
			"Lower value means better CA responsiveness but possible slower scale down latency."+
			"Higher value can affect CA performance with big clusters (hundreds of nodes)."+
			"Set to non posistive value to turn this heuristic off - CA will not limit the number of nodes it considers.")
	scaleDownCandidatesPoolRatio = flag.Float64("scale-down-candidates-pool-ratio", 0.1,
		"A ratio of nodes that are considered as additional non empty candidates for"+
			"scale down when some candidates from previous iteration are no longer valid."+
			"Lower value means better CA responsiveness but possible slower scale down latency."+
			"Higher value can affect CA performance with big clusters (hundreds of nodes)."+
			"Set to 1.0 to turn this heuristics off - CA will take all nodes as additional candidates.")
	scaleDownCandidatesPoolMinCount = flag.Int("scale-down-candidates-pool-min-count", 50,
		"Minimum number of nodes that are considered as additional non empty candidates"+
			"for scale down when some candidates from previous iteration are no longer valid."+
			"When calculating the pool size for additional candidates we take"+
			"max(#nodes * scale-down-candidates-pool-ratio, scale-down-candidates-pool-min-count).")
	scanInterval      = flag.Duration("scan-interval", 10*time.Second, "How often cluster is reevaluated for scale up or down")
	maxNodesTotal     = flag.Int("max-nodes-total", 0, "Maximum number of nodes in all node groups. Cluster autoscaler will not grow the cluster beyond this number.")
	coresTotal        = flag.String("cores-total", minMaxFlagString(0, config.DefaultMaxClusterCores), "Minimum and maximum number of cores in cluster, in the format <min>:<max>. Cluster autoscaler will not scale the cluster beyond these numbers.")
	memoryTotal       = flag.String("memory-total", minMaxFlagString(0, config.DefaultMaxClusterMemory), "Minimum and maximum number of gigabytes of memory in cluster, in the format <min>:<max>. Cluster autoscaler will not scale the cluster beyond these numbers.")
	gpuTotal          = multiStringFlag("gpu-total", "Minimum and maximum number of different GPUs in cluster, in the format <gpu_type>:<min>:<max>. Cluster autoscaler will not scale the cluster beyond these numbers. Can be passed multiple times. CURRENTLY THIS FLAG ONLY WORKS ON GKE.")
	cloudProviderFlag = flag.String("cloud-provider", cloudBuilder.DefaultCloudProvider,
		"Cloud provider type. Available values: ["+strings.Join(cloudBuilder.AvailableCloudProviders, ",")+"]")
	maxEmptyBulkDeleteFlag     = flag.Int("max-empty-bulk-delete", 10, "Maximum number of empty nodes that can be deleted at the same time.")
	maxGracefulTerminationFlag = flag.Int("max-graceful-termination-sec", 10*60, "Maximum number of seconds CA waits for pod termination when trying to scale down a node.")
	maxTotalUnreadyPercentage  = flag.Float64("max-total-unready-percentage", 45, "Maximum percentage of unready nodes in the cluster.  After this is exceeded, CA halts operations")
	okTotalUnreadyCount        = flag.Int("ok-total-unready-count", 3, "Number of allowed unready nodes, irrespective of max-total-unready-percentage")
	maxNodeProvisionTime       = flag.Duration("max-node-provision-time", 15*time.Minute, "Maximum time CA waits for node to be provisioned")
	nodeGroupsFlag             = multiStringFlag(
		"nodes",
		"sets min,max size and other configuration data for a node group in a format accepted by cloud provider. Can be used multiple times. Format: <min>:<max>:<other...>")
	nodeGroupAutoDiscoveryFlag = multiStringFlag(
		"node-group-auto-discovery",
		"One or more definition(s) of node group auto-discovery. "+
			"A definition is expressed `<name of discoverer>:[<key>[=<value>]]`. "+
			"The `aws` and `gce` cloud providers are currently supported. AWS matches by ASG tags, e.g. `asg:tag=tagKey,anotherTagKey`. "+
			"GCE matches by IG name prefix, and requires you to specify min and max nodes per IG, e.g. `mig:namePrefix=pfx,min=0,max=10` "+
			"Can be used multiple times.")

	estimatorFlag = flag.String("estimator", estimator.BinpackingEstimatorName,
		"Type of resource estimator to be used in scale up. Available values: ["+strings.Join(estimator.AvailableEstimators, ",")+"]")

	expanderFlag = flag.String("expander", expander.RandomExpanderName,
		"Type of node group expander to be used in scale up. Available values: ["+strings.Join(expander.AvailableExpanders, ",")+"]")

	writeStatusConfigMapFlag         = flag.Bool("write-status-configmap", true, "Should CA write status information to a configmap")
	maxInactivityTimeFlag            = flag.Duration("max-inactivity", 10*time.Minute, "Maximum time from last recorded autoscaler activity before automatic restart")
	maxFailingTimeFlag               = flag.Duration("max-failing-time", 15*time.Minute, "Maximum time from last recorded successful autoscaler run before automatic restart")
	balanceSimilarNodeGroupsFlag     = flag.Bool("balance-similar-node-groups", false, "Detect similar node groups and balance the number of nodes between them")
	nodeAutoprovisioningEnabled      = flag.Bool("node-autoprovisioning-enabled", false, "Should CA autoprovision node groups when needed")
	maxAutoprovisionedNodeGroupCount = flag.Int("max-autoprovisioned-node-group-count", 15, "The maximum number of autoprovisioned groups in the cluster.")

	unremovableNodeRecheckTimeout = flag.Duration("unremovable-node-recheck-timeout", 5*time.Minute, "The timeout before we check again a node that couldn't be removed before")
	expendablePodsPriorityCutoff  = flag.Int("expendable-pods-priority-cutoff", -10, "Pods with priority below cutoff will be expendable. They can be killed without any consideration during scale down and they don't cause scale up. Pods with null priority (PodPriority disabled) are non expendable.")
	regional                      = flag.Bool("regional", false, "Cluster is regional.")
)

func createAutoscalingOptions() config.AutoscalingOptions {
	minCoresTotal, maxCoresTotal, err := parseMinMaxFlag(*coresTotal)
	if err != nil {
		glog.Fatalf("Failed to parse flags: %v", err)
	}
	minMemoryTotal, maxMemoryTotal, err := parseMinMaxFlag(*memoryTotal)
	if err != nil {
		glog.Fatalf("Failed to parse flags: %v", err)
	}
	// Convert memory limits to bytes.
	minMemoryTotal = minMemoryTotal * units.Gigabyte
	maxMemoryTotal = maxMemoryTotal * units.Gigabyte

	parsedGpuTotal, err := parseMultipleGpuLimits(*gpuTotal)
	if err != nil {
		glog.Fatalf("Failed to parse flags: %v", err)
	}

	return config.AutoscalingOptions{
		CloudConfig:                      *cloudConfig,
		CloudProviderName:                *cloudProviderFlag,
		NodeGroupAutoDiscovery:           *nodeGroupAutoDiscoveryFlag,
		MaxTotalUnreadyPercentage:        *maxTotalUnreadyPercentage,
		OkTotalUnreadyCount:              *okTotalUnreadyCount,
		EstimatorName:                    *estimatorFlag,
		ExpanderName:                     *expanderFlag,
		MaxEmptyBulkDelete:               *maxEmptyBulkDeleteFlag,
		MaxGracefulTerminationSec:        *maxGracefulTerminationFlag,
		MaxNodeProvisionTime:             *maxNodeProvisionTime,
		MaxNodesTotal:                    *maxNodesTotal,
		MaxCoresTotal:                    maxCoresTotal,
		MinCoresTotal:                    minCoresTotal,
		MaxMemoryTotal:                   maxMemoryTotal,
		MinMemoryTotal:                   minMemoryTotal,
		GpuTotal:                         parsedGpuTotal,
		NodeGroups:                       *nodeGroupsFlag,
		ScaleDownDelayAfterAdd:           *scaleDownDelayAfterAdd,
		ScaleDownDelayAfterDelete:        *scaleDownDelayAfterDelete,
		ScaleDownDelayAfterFailure:       *scaleDownDelayAfterFailure,
		ScaleDownEnabled:                 *scaleDownEnabled,
		ScaleDownUnneededTime:            *scaleDownUnneededTime,
		ScaleDownUnreadyTime:             *scaleDownUnreadyTime,
		ScaleDownUtilizationThreshold:    *scaleDownUtilizationThreshold,
		ScaleDownNonEmptyCandidatesCount: *scaleDownNonEmptyCandidatesCount,
		ScaleDownCandidatesPoolRatio:     *scaleDownCandidatesPoolRatio,
		ScaleDownCandidatesPoolMinCount:  *scaleDownCandidatesPoolMinCount,
		WriteStatusConfigMap:             *writeStatusConfigMapFlag,
		BalanceSimilarNodeGroups:         *balanceSimilarNodeGroupsFlag,
		ConfigNamespace:                  *namespace,
		ClusterName:                      *clusterName,
		NodeAutoprovisioningEnabled:      *nodeAutoprovisioningEnabled,
		MaxAutoprovisionedNodeGroupCount: *maxAutoprovisionedNodeGroupCount,
		UnremovableNodeRecheckTimeout:    *unremovableNodeRecheckTimeout,
		ExpendablePodsPriorityCutoff:     *expendablePodsPriorityCutoff,
		Regional:                         *regional,
	}
}

func getKubeConfig() *rest.Config {
	if *kubeConfigFile != "" {
		glog.V(1).Infof("Using kubeconfig file: %s", *kubeConfigFile)
		// use the current context in kubeconfig
		config, err := clientcmd.BuildConfigFromFlags("", *kubeConfigFile)
		if err != nil {
			glog.Fatalf("Failed to build config: %v", err)
		}
		return config
	}
	url, err := url.Parse(*kubernetes)
	if err != nil {
		glog.Fatalf("Failed to parse Kubernetes url: %v", err)
	}

	kubeConfig, err := config.GetKubeClientConfig(url)
	if err != nil {
		glog.Fatalf("Failed to build Kubernetes client configuration: %v", err)
	}

	return kubeConfig
}

func createKubeClient(kubeConfig *rest.Config) kube_client.Interface {
	return kube_client.NewForConfigOrDie(kubeConfig)
}

func registerSignalHandlers(autoscaler core.Autoscaler) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, os.Kill, syscall.SIGTERM, syscall.SIGQUIT)
	glog.V(1).Info("Registered cleanup signal handler")

	go func() {
		<-sigs
		glog.V(1).Info("Received signal, attempting cleanup")
		autoscaler.ExitCleanUp()
		glog.V(1).Info("Cleaned up, exiting...")
		glog.Flush()
		os.Exit(0)
	}()
}

func buildAutoscaler() (core.Autoscaler, error) {
	// Create basic config from flags.
	autoscalingOptions := createAutoscalingOptions()
	kubeClient := createKubeClient(getKubeConfig())
	opts := core.AutoscalerOptions{
		AutoscalingOptions: autoscalingOptions,
		KubeClient:         kubeClient,
	}

	// This metric should be published only once.
	metrics.UpdateNapEnabled(autoscalingOptions.NodeAutoprovisioningEnabled)

	// Create autoscaler.
	return core.NewAutoscaler(opts)
}

func run(healthCheck *metrics.HealthCheck) {
	metrics.RegisterAll()

	autoscaler, err := buildAutoscaler()
	if err != nil {
		glog.Fatalf("Failed to create autoscaler: %v", err)
	}

	// Register signal handlers for graceful shutdown.
	registerSignalHandlers(autoscaler)

	// Start updating health check endpoint.
	healthCheck.StartMonitoring()

	// Autoscale ad infinitum.
	for {
		select {
		case <-time.After(*scanInterval):
			{
				loopStart := time.Now()
				metrics.UpdateLastTime(metrics.Main, loopStart)
				healthCheck.UpdateLastActivity(loopStart)

				err := autoscaler.RunOnce(loopStart)
				if err != nil && err.Type() != errors.TransientError {
					metrics.RegisterError(err)
				} else {
					healthCheck.UpdateLastSuccessfulRun(time.Now())
				}

				metrics.UpdateDurationFromStart(metrics.Main, loopStart)
			}
		}
	}
}

func main() {
	leaderElection := defaultLeaderElectionConfiguration()
	leaderElection.LeaderElect = true

	leaderelectionconfig.BindFlags(&leaderElection, pflag.CommandLine)
	kube_flag.InitFlags()
	healthCheck := metrics.NewHealthCheck(*maxInactivityTimeFlag, *maxFailingTimeFlag)

	//glog.V(1).Infof("Cluster Autoscaler %s", ClusterAutoscalerVersion)

	correctEstimator := false
	for _, availableEstimator := range estimator.AvailableEstimators {
		if *estimatorFlag == availableEstimator {
			correctEstimator = true
		}
	}
	if !correctEstimator {
		glog.Fatalf("Unrecognized estimator: %v", *estimatorFlag)
	}

	go func() {
		http.Handle("/metrics", prometheus.Handler())
		http.Handle("/health-check", healthCheck)
		err := http.ListenAndServe(*address, nil)
		glog.Fatalf("Failed to start metrics: %v", err)
	}()

	if !leaderElection.LeaderElect {
		run(healthCheck)
	} else {
		id, err := os.Hostname()
		if err != nil {
			glog.Fatalf("Unable to get hostname: %v", err)
		}

		kubeClient := createKubeClient(getKubeConfig())

		// Validate that the client is ok.
		_, err = kubeClient.CoreV1().Nodes().List(metav1.ListOptions{})
		if err != nil {
			glog.Fatalf("Failed to get nodes from apiserver: %v", err)
		}

		lock, err := resourcelock.New(
			leaderElection.ResourceLock,
			*namespace,
			"cluster-autoscaler",
			kubeClient.CoreV1(),
			resourcelock.ResourceLockConfig{
				Identity:      id,
				EventRecorder: kube_util.CreateEventRecorder(kubeClient),
			},
		)
		if err != nil {
			glog.Fatalf("Unable to create leader election lock: %v", err)
		}

		leaderelection.RunOrDie(ctx.TODO(), leaderelection.LeaderElectionConfig{
			Lock:          lock,
			LeaseDuration: leaderElection.LeaseDuration.Duration,
			RenewDeadline: leaderElection.RenewDeadline.Duration,
			RetryPeriod:   leaderElection.RetryPeriod.Duration,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(_ ctx.Context) {
					// Since we are committing a suicide after losing
					// mastership, we can safely ignore the argument.
					run(healthCheck)
				},
				OnStoppedLeading: func() {
					glog.Fatalf("lost master")
				},
			},
		})
	}
}

func defaultLeaderElectionConfiguration() apiserverconfig.LeaderElectionConfiguration {
	return apiserverconfig.LeaderElectionConfiguration{
		LeaderElect:   false,
		LeaseDuration: metav1.Duration{Duration: defaultLeaseDuration},
		RenewDeadline: metav1.Duration{Duration: defaultRenewDeadline},
		RetryPeriod:   metav1.Duration{Duration: defaultRetryPeriod},
		ResourceLock:  resourcelock.EndpointsResourceLock,
	}
}

const (
	defaultLeaseDuration = 15 * time.Second
	defaultRenewDeadline = 10 * time.Second
	defaultRetryPeriod   = 2 * time.Second
)

func parseMinMaxFlag(flag string) (int64, int64, error) {
	tokens := strings.SplitN(flag, ":", 2)
	if len(tokens) != 2 {
		return 0, 0, fmt.Errorf("wrong nodes configuration: %s", flag)
	}

	min, err := strconv.ParseInt(tokens[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to set min size: %s, expected integer, err: %v", tokens[0], err)
	}

	max, err := strconv.ParseInt(tokens[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to set max size: %s, expected integer, err: %v", tokens[1], err)
	}

	err = validateMinMaxFlag(min, max)
	if err != nil {
		return 0, 0, err
	}

	return min, max, nil
}

func validateMinMaxFlag(min, max int64) error {
	if min < 0 {
		return fmt.Errorf("min size must be greater or equal to  0")
	}
	if max < min {
		return fmt.Errorf("max size must be greater or equal to min size")
	}
	return nil
}

func minMaxFlagString(min, max int64) string {
	return fmt.Sprintf("%v:%v", min, max)
}

func parseMultipleGpuLimits(flags MultiStringFlag) ([]config.GpuLimits, error) {
	parsedFlags := make([]config.GpuLimits, 0, len(flags))
	for _, flag := range flags {
		parsedFlag, err := parseSingleGpuLimit(flag)
		if err != nil {
			return nil, err
		}
		parsedFlags = append(parsedFlags, parsedFlag)
	}
	return parsedFlags, nil
}

func parseSingleGpuLimit(limits string) (config.GpuLimits, error) {
	parts := strings.Split(limits, ":")
	if len(parts) != 3 {
		return config.GpuLimits{}, fmt.Errorf("Incorrect gpu limit specification: %v", limits)
	}
	gpuType := parts[0]
	minVal, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return config.GpuLimits{}, fmt.Errorf("Incorrect gpu limit - min is not integer: %v", limits)
	}
	maxVal, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return config.GpuLimits{}, fmt.Errorf("Incorrect gpu limit - max is not integer: %v", limits)
	}
	if minVal < 0 {
		return config.GpuLimits{}, fmt.Errorf("Incorrect gpu limit - min is less than 0; %v", limits)
	}
	if maxVal < 0 {
		return config.GpuLimits{}, fmt.Errorf("Incorrect gpu limit - max is less than 0; %v", limits)
	}
	if minVal > maxVal {
		return config.GpuLimits{}, fmt.Errorf("Incorrect gpu limit - min is greater than max; %v", limits)
	}
	parsedGpuLimits := config.GpuLimits{
		GpuType: gpuType,
		Min:     minVal,
		Max:     maxVal,
	}
	return parsedGpuLimits, nil
}
