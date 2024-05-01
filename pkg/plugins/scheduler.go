package plugins

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"log"

	v1 "k8s.io/api/core/v1"
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
var fcfsQueue = list.New()

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
	Name              string = "CustomScheduler"
	groupNameLabel    string = "podGroup"
	minAvailableLabel string = "minAvailable"
	leastMode         string = "Least"
	mostMode          string = "Most"
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

// filter the pod if the pod in group is less than minAvailable
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

	log.Printf("calculating total request number of resourcs")
	var totalRequestNum int64 = 0
	for _, container := range pod.Spec.Containers {
		// obtain the request number of resources
		temp := container.Resources.Requests["example.com/foo"]
		requestNum, _ := temp.AsInt64()
		totalRequestNum += requestNum
	}
	log.Printf("Pod %s require %d resources", pod.Name, totalRequestNum)

	log.Printf("Checking if there exist a node satisfied the request amount")
	for _, node := range nodeList {
		resourceNum := node.Allocatable.ScalarResources["example.com/foo"]
		log.Printf("Resource num: %d", resourceNum)
		if totalRequestNum <= resourceNum {
			log.Printf("Find exist node qualified, preFilter return Success")
			if fcfsQueue.Len() > 0 && pod.Name == fcfsQueue.Front().Value {
				fcfsQueue.Remove(fcfsQueue.Front())
			}
			return nil, framework.NewStatus(framework.Success, "Found a node to schedule")
		}
	}

	log.Printf("No existing node is qualified, push into waiting queue")
	// if the pod is not in queue -> push it!
	if fcfsQueue.Len() == 0 {
		fcfsQueue.PushBack(pod.Name)
	}
	return nil, framework.NewStatus(framework.Unschedulable, "No enough resources")
}

// PreFilterExtensions returns a PreFilterExtensions interface if the plugin implements one.
func (cs *CustomScheduler) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}
