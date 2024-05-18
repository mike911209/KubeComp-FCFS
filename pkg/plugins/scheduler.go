package plugins

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

type CustomSchedulerArgs struct {
	Mode string `json:"mode"`
}

type CustomScheduler struct {
	handle framework.Handle
}

var _ framework.PreFilterPlugin = &CustomScheduler{}
var _ framework.ScorePlugin = &CustomScheduler{}

var fcfsQueue = list.New()

var CPUDevice = map[string]int64{
	"nvidia.com/mig-1g.5gb":  1,
	"nvidia.com/mig-1g.10gb": 1,
	"nvidia.com/mig-2g.10gb": 2,
	"nvidia.com/mig-3g.20gb": 3,
	"nvidia.com/mig-4g.20gb": 4,
	"nvidia.com/mig-7g.40gb": 7,
}
var MemDevice = map[string]int64{
	"nvidia.com/mig-1g.5gb":  5,
	"nvidia.com/mig-1g.10gb": 10,
	"nvidia.com/mig-2g.10gb": 20,
	"nvidia.com/mig-3g.20gb": 20,
	"nvidia.com/mig-4g.20gb": 20,
	"nvidia.com/mig-7g.40gb": 40,
}

// Name is the name of the plugin used in Registry and configurations.
const (
	Name     string = "CustomScheduler"
	nodeName string = "minikube"

	targetPodLabel       string = "targetPod"
	targetNamespaceLabel string = "targetNamespace"
	gpuResources         string = "nvidia.com/"
	preFilterStateKey           = "PreFilter" + Name
)

func (cs *CustomScheduler) Name() string {
	return Name
}

type PreFilterState struct {
	// the node that was labeled -> score plugin should give the highest score
	labelNode string
}

func (state *PreFilterState) Clone() framework.StateData {
	return state
}

func FindElement(name string, fcfsQueue *list.List) int {
	index := 0
	for e := fcfsQueue.Front(); e != nil; e = e.Next() {
		if e.Value == name {
			return index
		}
		index++
	}
	return -1
}

// New initializes and returns a new CustomScheduler plugin.
func New(obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	cs := CustomScheduler{}
	if obj != nil {
		args := obj.(*runtime.Unknown)
		var csArgs CustomSchedulerArgs
		if err := json.Unmarshal(args.Raw, &csArgs); err != nil {
			fmt.Printf("Error unmarshal: %v\n", err)
		}
	}
	cs.handle = h
	log.Printf("Custom scheduler was created!")

	return &cs, nil
}

// This function extracts used GPU slices in current node
// will return CPU and Mem resource left in current node
// and also return the number of each mig slice left un current node
func (cs *CustomScheduler) extractUsedGPU(node *framework.NodeInfo) (int64, int64, map[string]int64) {
	migSlicecnts := make(map[string]int64)

	// calculate available CPU and Mem resource
	var CPULeft, MemLeft int64 = 0, 0
	for resourceName, totalNum := range node.Allocatable.ScalarResources {
		if strings.HasPrefix(string(resourceName), gpuResources) {
			requestedNum := node.Requested.ScalarResources[resourceName]
			resourceName_ := resourceName.String()
			CPULeft += (totalNum - requestedNum) * CPUDevice[resourceName_]
			MemLeft += (totalNum - requestedNum) * MemDevice[resourceName_]
			migSlicecnts[resourceName_] += (totalNum - requestedNum)
		}
	}

	// for _, pod := range node.Pods {
	// 	// skip the terminated pod
	// 	if pod.Pod.Status.Phase == v1.PodSucceeded || pod.Pod.Status.Phase == v1.PodFailed {
	// 		continue
	// 	}
	// 	for _, c := range pod.Pod.Spec.Containers {
	// 		// loop through all resource request til the request is "nvidia.com/..."
	// 		for sliceName, sliceCnts := range c.Resources.Requests {
	// 			if strings.HasPrefix(string(sliceName), gpuResources) {
	// 				num, _ := sliceCnts.AsInt64()
	// 				CPULeft -= num * CPUDevice[sliceName.String()]
	// 				MemLeft -= num * MemDevice[sliceName.String()]
	// 				migSlicecnts[sliceName.String()] -= num
	// 			}
	// 		}
	// 	}
	// }
	return CPULeft, MemLeft, migSlicecnts
}

// Find the node with least resource but sufficient enough for request
func (cs CustomScheduler) findBestNode(nodeList []*framework.NodeInfo, requestGPU map[string]int64) (*v1.Node, bool) {
	// obtain the request # resources of the pod
	var CPURequest, MemRequest int64 = 0, 0
	for sliceName, quantity := range requestGPU {
		CPURequest += CPUDevice[sliceName] * quantity
		MemRequest += MemDevice[sliceName] * quantity
	}

	// find the best node
	bestCPULeft, bestMemLeft := int64(math.MaxInt64), int64(math.MaxInt64)
	toBeReconfig := false // true -> to be reconfig; false -> no need to reconfig
	var bestNode *v1.Node = nil
	for _, node := range nodeList {
		CPULeft, MemLeft, migSliceCnts := cs.extractUsedGPU(node)
		// TODO the node with no need to reconfigure have higher scheduling priority
		if (CPULeft >= CPURequest && MemLeft >= MemRequest) && (CPULeft <= bestCPULeft && MemLeft <= bestMemLeft) {
			// if there exist available resource -> no need to reconfig
			for sliceName, sliceCnts := range requestGPU {
				if migSliceCnts[sliceName] < sliceCnts {
					toBeReconfig = true
					break
				}
			}
			bestCPULeft, bestMemLeft = CPULeft, MemLeft
			bestNode = node.Node()
		}
	}

	if bestNode == nil {
		log.Printf("No node with sufficient resources")
	} else {
		log.Printf("%s is the chosen node with CPU %d, Mem %d left", bestNode.Name, bestCPULeft, bestMemLeft)
	}

	return bestNode, toBeReconfig
}

// serving FCFS policy, filter out the node without sufficient resources
// label the node to be reconfigure
func (cs *CustomScheduler) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	log.Printf("Pod %s is in Prefilter phase.", pod.Name)

	// check if there exist waiting pod
	log.Printf("fcfsQueue.size: %d", fcfsQueue.Len())
	if fcfsQueue.Len() > 0 && pod.Name != fcfsQueue.Front().Value {
		// if the pod is not in queue
		if FindElement(pod.Name, fcfsQueue) == -1 {
			fcfsQueue.PushBack(pod.Name)
		}
		log.Printf("Pod %s is postponed in waiting queue.", pod.Name)
		return nil, framework.NewStatus(framework.Unschedulable, "push and wait in the FCFS queue")
	}

	// calculating request # resourcs
	log.Printf("calculating total CPU and Mem request")
	requestGPU := make(map[string]int64)

	// obtain resource request num of the pod
	for _, c := range pod.Spec.Containers {
		for sliceName, sliceCnts := range c.Resources.Requests {
			if strings.HasPrefix(string(sliceName), gpuResources) {
				num, _ := sliceCnts.AsInt64()
				log.Printf("Resource request: %s; num: %d", sliceName.String(), num)
				requestGPU[sliceName.String()] += num
			}
		}
	}

	// obtain list of node info
	nodeList, err := cs.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		log.Printf("Failed to get node status")
		return nil, framework.NewStatus(framework.Unschedulable, "failed to get node status")
	}

	// check if there exist node that satisfy the request
	// if found -> label the best node as "target" then return
	// if not   -> push into FCFSQueue
	bestNode, toBeReconfig := cs.findBestNode(nodeList, requestGPU)

	// not found
	if bestNode == nil {
		log.Printf("No existing node is qualified, push into waiting queue")
		// if the pod is not in queue -> push it!
		if fcfsQueue.Len() == 0 {
			fcfsQueue.PushBack(pod.Name)
		}
		return nil, framework.NewStatus(framework.Unschedulable, "No enough resources")
	}

	// found -> update node's label
	if toBeReconfig {
		log.Printf("Updating Node labels to targetPod and targetNamespace")
		bestNode.Labels[targetPodLabel] = pod.Name
		bestNode.Labels[targetNamespaceLabel] = pod.Namespace
		_, err = cs.handle.ClientSet().CoreV1().Nodes().Update(context.TODO(), bestNode, metav1.UpdateOptions{})
		if err != nil {
			log.Print(err)
			return nil, framework.NewStatus(framework.Error, "error updating node labels")
		}
	}

	if fcfsQueue.Len() > 0 && pod.Name == fcfsQueue.Front().Value {
		fcfsQueue.Remove(fcfsQueue.Front())
	}

	preFilterState := &PreFilterState{
		labelNode: bestNode.Name,
	}
	state.Write(framework.StateKey(preFilterStateKey), preFilterState)

	return nil, framework.NewStatus(framework.Success, "Found a node to schedule")
}

// PreFilterExtensions returns a PreFilterExtensions interface if the plugin implements one.
func (cs *CustomScheduler) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

// Score invoked at the score extension point.
func (cs *CustomScheduler) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	// get preFilter state
	preFilterState, err := getPreFilterState(state)
	if err != nil {
		log.Printf("Error geting preFilterState from cycleState")
		return framework.MinNodeScore, framework.NewStatus(framework.Error, "not eligible due to failed to read from cycleState, return min score")
	}

	// return score based on preFilterState
	if nodeName == preFilterState.labelNode {
		log.Printf("Max score: %s is the node labeled in preFilter state", nodeName)
		return framework.MaxNodeScore, framework.NewStatus(framework.Success, "The node with label: max score")
	} else {
		log.Printf("Min score: %s is not the node labeled in preFilter state", nodeName)
		return framework.MinNodeScore, framework.NewStatus(framework.Success, "Other nodes: min score")
	}
}

func (cs *CustomScheduler) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	// Find highest and lowest scores.
	minScore, maxScore := getMinMaxScores(scores)

	// If all nodes were given the minimum score, return
	if minScore == framework.MinNodeScore && maxScore == framework.MinNodeScore {
		return nil
	}

	// Transform the highest to lowest score range to fit the framework's min to max node score range.
	oldRange := maxScore - minScore
	newRange := framework.MaxNodeScore - framework.MinNodeScore
	for i, nodeScore := range scores {
		if oldRange == 0 {
			scores[i].Score = framework.MaxNodeScore - (scores[i].Score - minScore)
		} else {
			scores[i].Score = ((nodeScore.Score - minScore) * newRange / oldRange) + framework.MinNodeScore
		}
	}

	return nil
}

// MinMax : get min and max scores from NodeScoreList
func getMinMaxScores(scores framework.NodeScoreList) (int64, int64) {
	var max int64 = math.MinInt64 // Set to min value
	var min int64 = math.MaxInt64 // Set to max value

	for _, nodeScore := range scores {
		if nodeScore.Score > max {
			max = nodeScore.Score
		}
		if nodeScore.Score < min {
			min = nodeScore.Score
		}
	}
	// return min and max scores
	return min, max
}

// ScoreExtensions of the Score plugin.
func (cs *CustomScheduler) ScoreExtensions() framework.ScoreExtensions {
	return cs
}

func getPreFilterState(cycleState *framework.CycleState) (*PreFilterState, error) {
	no, err := cycleState.Read(framework.StateKey(preFilterStateKey))
	if err != nil {
		// preFilterState doesn't exist, likely PreFilter wasn't invoked.
		return nil, fmt.Errorf("error reading %q from cycleState: %w", preFilterStateKey, err)
	}

	state, ok := no.(*PreFilterState)
	if !ok {
		return nil, fmt.Errorf("%+v  convert to NetworkOverhead.preFilterState error", no)
	}
	return state, nil
}
