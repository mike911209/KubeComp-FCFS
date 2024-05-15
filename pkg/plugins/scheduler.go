package plugins

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"log"
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

// var _ framework.ScorePlugin = &CustomScheduler{}
var fcfsQueue = list.New()

var CPUDevice = map[string]int64{
	"1g.5gb":  1,
	"1g.10gb": 1,
	"2g.10gb": 2,
	"3g.20gb": 3,
	"4g.20gb": 4,
	"7g.40gb": 7,
}
var MemDevice = map[string]int64{
	"1g.5gb":  5,
	"1g.10gb": 10,
	"2g.10gb": 20,
	"3g.20gb": 20,
	"4g.20gb": 20,
	"7g.40gb": 40,
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

// Name is the name of the plugin used in Registry and configurations.
const (
	Name     string = "CustomScheduler"
	nodeName string = "minikube"

	targetPodLabel       string = "targetPod"
	targetNamespaceLabel string = "targetNamespace"
	gpuResources         string = "nvidia.com/"
	CPUTotal             int64  = 7
	MemTotal             int64  = 40
)

func (cs *CustomScheduler) Name() string {
	return Name
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
func (cs *CustomScheduler) extractUsedGPU(nodeName string) (int64, int64) {
	// list all the pods scheduled on the node
	pods, err := cs.handle.ClientSet().CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
	})
	if err != nil {
		log.Fatal("Error listing pods on node %s: %v\n", nodeName, err)
	}

	// calculate CPU and Mem resource left in current node
	CPULeft, MemLeft := CPUTotal, MemTotal
	removeString := "nvidia.com/mig-"
	for _, pod := range pods.Items {
		// skip the terminated pod
		if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			continue
		}
		for _, c := range pod.Spec.Containers {
			// loop through all resource request til the request is "nvidia.com/..."
			for sliceName, sliceCnts := range c.Resources.Requests {
				if strings.HasPrefix(string(sliceName), gpuResources) {
					num, _ := sliceCnts.AsInt64()
					CPULeft -= num * CPUDevice[string(sliceName)[len(removeString):]]
					MemLeft -= num * MemDevice[string(sliceName)[len(removeString):]]
				}
			}
		}
	}
	return CPULeft, MemLeft
}

// Find the node with least resource but sufficient enough for request
func (cs CustomScheduler) findBestNode(nodeList []*framework.NodeInfo, CPURequest int64, MemRequest int64) (string, error) {
	var bestCPULeft, bestMemLeft int64 = CPUTotal, MemTotal
	bestNode := ""
	for _, node := range nodeList {
		CPULeft, MemLeft := cs.extractUsedGPU(node.Node().Name)
		if (CPULeft >= CPURequest && MemLeft >= MemRequest) && (CPULeft <= bestCPULeft && MemLeft <= bestMemLeft) {
			bestCPULeft, bestMemLeft = CPULeft, MemLeft
			bestNode = node.Node().Name
		}
	}

	if bestNode == "" {
		log.Printf("No node with sufficient resources")
		return bestNode, fmt.Errorf("No node with sufficient resources")
	}
	log.Printf("%s is the chosen node with CPU %d, Mem %d left", bestNode, bestCPULeft, bestMemLeft)
	return bestNode, nil
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

	log.Printf("Checking whether pod %s is qualified", pod.Name)

	// obtain node info
	nodeList, err := cs.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		log.Printf("Failed to get node status")
		return nil, framework.NewStatus(framework.Unschedulable, "failed to get node status")
	}

	// calculating request # resourcs
	log.Printf("calculating total CPU and Mem request")
	var CPURequest, MemRequest int64 = 0, 0
	removeString := "nvidia.com/mig-"
	for _, c := range pod.Spec.Containers {
		// loop through all resource request til the request is "nvidia.com/..."
		for sliceName, sliceCnts := range c.Resources.Requests {
			log.Printf("Resource request: %s", sliceName)
			if strings.HasPrefix(string(sliceName), gpuResources) {
				num, _ := sliceCnts.AsInt64()
				log.Printf("Resource request: %s; num: %d", string(sliceName)[len(removeString):], num)
				CPURequest += num * CPUDevice[string(sliceName)[len(removeString):]]
				MemRequest += num * MemDevice[string(sliceName)[len(removeString):]]
			}
		}
	}
	log.Printf("Pod %s require %d CPU and %d Mem", pod.Name, CPURequest, MemRequest)

	// check if some nodes satisfy the request
	// if found -> label the best node as "target" then return
	// if not   -> push into FCFSQueue
	bestNode, err := cs.findBestNode(nodeList, CPURequest, MemRequest)
	if err != nil {
		// no node satisfy the request
		log.Printf("No existing node is qualified, push into waiting queue")
		// if the pod is not in queue -> push it!
		if fcfsQueue.Len() == 0 {
			fcfsQueue.PushBack(pod.Name)
		}
		return nil, framework.NewStatus(framework.Unschedulable, "No enough resources")
	}

	// obtain node label list
	bestNode_, err := cs.handle.ClientSet().CoreV1().Nodes().Get(context.TODO(), bestNode, metav1.GetOptions{})
	if err != nil {
		return nil, framework.NewStatus(framework.Error, "error geting node Labels")
	}

	log.Printf("Updating Node labels to targetPod and targetNamespace")
	// update node labels as target
	bestNode_.Labels[targetPodLabel] = pod.Name
	bestNode_.Labels[targetNamespaceLabel] = pod.Namespace
	_, err = cs.handle.ClientSet().CoreV1().Nodes().Update(context.TODO(), bestNode_, metav1.UpdateOptions{})
	if err != nil {
		log.Print(err)
		return nil, framework.NewStatus(framework.Error, "error updating node labels")
	}

	log.Printf("Find existing node qualified, preFilter return Success")
	if fcfsQueue.Len() > 0 && pod.Name == fcfsQueue.Front().Value {
		fcfsQueue.Remove(fcfsQueue.Front())
	}

	return nil, framework.NewStatus(framework.Success, "Found a node to schedule")
}

// PreFilterExtensions returns a PreFilterExtensions interface if the plugin implements one.
func (cs *CustomScheduler) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

// // score plugins
// // TODO: give the target node max score
// // Score invoked at the score extension point.
// func (cs *CustomScheduler) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
// 	nodeInfo, err := cs.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
// 	if err != nil {
// 		return 0, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
// 	}

// 	required_falcon_quantity := pod.Spec.Containers[0].Resources.Requests["falcon.com/gpu"]
// 	required_falcon, _ := required_falcon_quantity.AsInt64()
// 	local_falcon := (nodeInfo.Allocatable.ScalarResources["falcon.com/gpu"] - nodeInfo.Requested.ScalarResources["falcon.com/gpu"])

// 	var score int64 = 0
// 	if local_falcon > required_falcon {
// 		score = int64(required_falcon * 100 / local_falcon)
// 	} else if local_falcon == required_falcon {
// 		score = 100
// 	} else {
// 		score = local_falcon - required_falcon
// 	}
// 	log.Printf("%s has %d gpu, %s requires %d gpu -> score: %v\n", nodeName, local_falcon, pod.Name, required_falcon, score)
// 	return score, nil
// }

// func (cs *CustomScheduler) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
// 	// Find highest and lowest scores.
// 	var highest int64 = -math.MaxInt64
// 	var lowest int64 = math.MaxInt64
// 	for _, nodeScore := range scores {
// 		if nodeScore.Score > highest {
// 			highest = nodeScore.Score
// 		}
// 		if nodeScore.Score < lowest {
// 			lowest = nodeScore.Score
// 		}
// 	}

// 	// Transform the highest to lowest score range to fit the framework's min to max node score range.
// 	oldRange := highest - lowest
// 	newRange := framework.MaxNodeScore - framework.MinNodeScore
// 	for i, nodeScore := range scores {
// 		if oldRange == 0 {
// 			scores[i].Score = framework.MinNodeScore
// 		} else {
// 			scores[i].Score = ((nodeScore.Score - lowest) * newRange / oldRange) + framework.MinNodeScore
// 		}
// 	}

// 	return nil
// }

// // ScoreExtensions of the Score plugin.
// func (cs *CustomScheduler) ScoreExtensions() framework.ScoreExtensions {
// 	return cs
// }
