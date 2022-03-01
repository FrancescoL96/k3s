/*
Copyright 2019 The Kubernetes Authors.

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

package realtime

import (
	"context"
	"math"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
)

// RealTime is a plugin that checks if the RT utilization of the current node is lower than the threshold for the current pod.
type RealTime struct {
	handle framework.Handle
}

var _ framework.FilterPlugin = &RealTime{}
var _ framework.ScorePlugin = &RealTime{}
var _ framework.QueueSortPlugin = &RealTime{}

const (
	// Name is the name of the plugin used in the plugin registry and configurations.
	Name = names.RealTime

	// ErrReason returned when node name doesn't match.
	ErrReason = "node(s) didn't have enough RT resources"
)

// EventsToRegister returns the possible events that may make a Pod
// failed by this plugin schedulable.
func (pl *RealTime) EventsToRegister() []framework.ClusterEvent {
	return []framework.ClusterEvent{
		{Resource: framework.Node, ActionType: framework.Add},
	}
}

// Name returns name of the plugin. It is used in logs, etc.
func (pl *RealTime) Name() string {
	return Name
}

// Sort the Pod queue by criticality, if two pods have the same criticality, take the one with the earlier timestamp
// If the pod has no criticality, the RT one is scheduled first
// If both pods have no criticality, then we use priority
func (pl *RealTime) Less(pInfo1, pInfo2 *framework.QueuedPodInfo) bool {
	criticality_p1 := '!'
	for _, container := range pInfo1.PodInfo.Pod.Spec.Containers {
		if container.RealTime.Criticality == "A" {
			criticality_p1 = 'A'
		} else if container.RealTime.Criticality == "B" {
			criticality_p1 = 'B'
		} else if container.RealTime.Criticality == "C" {
			criticality_p1 = 'C'
		} else {
			criticality_p1 = 'N'
		}
	}
	criticality_p2 := '!'
	for _, container := range pInfo2.PodInfo.Pod.Spec.Containers {
		if container.RealTime.Criticality == "A" {
			criticality_p2 = 'A'
		} else if container.RealTime.Criticality == "B" {
			criticality_p2 = 'B'
		} else if container.RealTime.Criticality == "C" {
			criticality_p2 = 'C'
		} else {
			criticality_p2 = 'N'
		}
	}
	if criticality_p1 == 'N' && criticality_p2 == 'N' {
		p1 := corev1helpers.PodPriority(pInfo1.Pod)
		p2 := corev1helpers.PodPriority(pInfo2.Pod)
		return (p1 > p2) || (p1 == p2 && pInfo1.Timestamp.Before(pInfo2.Timestamp))
	} else if criticality_p1 != 'N' && criticality_p2 == 'N' {
		return true
	} else if criticality_p1 == 'N' && criticality_p2 != 'N' {
		return false
	}
	return (criticality_p1 > criticality_p2) || (criticality_p1 == criticality_p2 && pInfo1.Timestamp.Before(pInfo2.Timestamp))
}

// Filter invoked at the filter extension point.
func (pl *RealTime) Filter(ctx context.Context, _ *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	if nodeInfo.Node() == nil {
		return framework.NewStatus(framework.Error, "node not found")
	}
	if !Fits(pod, nodeInfo) {
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, ErrReason)
	}
	return nil
}

func (pl *RealTime) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	nodeInfo, _ := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	newNodeUT := GetNodeNewUT(pod, nodeInfo)
	thresholdUT := GetNodeThreshold(nodeInfo)
	nRank := (thresholdUT - newNodeUT) / thresholdUT
	criticality := "A"
	// Take the highest criticality for all the containers in the POD
	for _, container := range pod.Spec.Containers {
		if container.RealTime.Criticality == "B" {
			criticality = "B"
		} else if container.RealTime.Criticality == "C" {
			criticality = "C"
		}
	}
	if criticality == "A" {
		return 100 - int64(nRank*100), nil
	} else if criticality == "C" {
		return int64(nRank * 100), nil
	} else {
		nRank = 1 - nRank
		if nRank < 0.5 {
			return int64((0.9 + 0.2*nRank) * 100), nil
		} else {
			return int64(((math.Pow(nRank, 2) * (5.0 / 3.0)) - nRank*(9.0/2.0) + (17.0 / 6.0)) * 100), nil
		}
	}
}

func (pl *RealTime) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	// Scores are already normalized due to how they are constructed
	return nil
}

// ScoreExtensions of the Score plugin.
func (pl *RealTime) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

// Fits actually checks if the pod fits the node.
func Fits(pod *v1.Pod, nodeInfo *framework.NodeInfo) bool {
	// Check whether the node has the RealTimeCriticality label
	if val, ok := nodeInfo.Node().ObjectMeta.Labels["RealTimeCriticality"]; ok {
		// If the pod needs criticality C and the node does not have criticality C, then it is not schedulable
		// If the node isolates criticality C and the pod is not criticality C, then it is not schedulable
		// It's an XOR between the two conditions
		if (pod.Spec.Containers[0].RealTime.Criticality == "C") != (val == "C") {
			return false
		}
	} else { // If it does not have the RTc label, then it is not RT compliant
		return false
	}

	// Check the utilization
	newNodeUT := GetNodeNewUT(pod, nodeInfo)
	thresholdUT := GetNodeThreshold(nodeInfo)
	if newNodeUT > thresholdUT {
		return false
	}
	return true
}

func GetNodeThreshold(nodeInfo *framework.NodeInfo) float64 {
	resQuant := nodeInfo.Node().Status.Capacity[v1.ResourceCPU]
	resQuantInt, _ := resQuant.AsInt64()
	return 0.95 * float64(resQuantInt)
}

func GetNodeNewUT(pod *v1.Pod, nodeInfo *framework.NodeInfo) float64 {
	// We use the deadline to calculate the utilization instead of the period as a worst-case scenario
	nodeUTcurrent := 0.0
	for _, podInfo_in_node := range nodeInfo.Pods {
		for _, container := range podInfo_in_node.Pod.Spec.Containers {
			if container.RealTime.RTDeadline != 0 && container.RealTime.RTWcet != 0 {
				nodeUTcurrent += float64(container.RealTime.RTWcet) / float64(container.RealTime.RTDeadline)
			}
		}
	}
	// We do not need to check if the RT values are !=0 because this plugin is called only for RT deployments
	return nodeUTcurrent + float64(pod.Spec.Containers[0].RealTime.RTWcet)/float64(pod.Spec.Containers[0].RealTime.RTDeadline)
}

// New initializes a new plugin and returns it.
func New(_ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &RealTime{handle: h}, nil
}
