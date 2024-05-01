package plugins

import (
	"context"
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	fakeframework "k8s.io/kubernetes/pkg/scheduler/framework/fake"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

func TestCustomScheduler_PreFilter(t *testing.T) {
	type TestPreFilterInput struct {
		ctx   context.Context
		state *framework.CycleState
		pod   *v1.Pod
	}
	tests := []struct {
		name string
		cs   *CustomScheduler
		args TestPreFilterInput
		want framework.Code
	}{
		{
			name: "pod is accepted",
			args: TestPreFilterInput{
				ctx:   context.Background(),
				state: nil,
				pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"podGroup":     "g1",
							"minAvailable": "1",
						},
					},
					Spec: v1.PodSpec{Containers: []v1.Container{}},
				},
			},
			want: framework.Success,
		},
		{
			name: "pod is just accepted",
			args: TestPreFilterInput{
				ctx:   context.Background(),
				state: nil,
				pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"podGroup":     "g1",
							"minAvailable": "3",
						},
					},
					Spec: v1.PodSpec{Containers: []v1.Container{}},
				},
			},
			want: framework.Success,
		},
		{
			name: "pod is rejected",
			args: TestPreFilterInput{
				ctx:   context.Background(),
				state: nil,
				pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"podGroup":     "g1",
							"minAvailable": "5",
						},
					},
					Spec: v1.PodSpec{Containers: []v1.Container{}},
				},
			},
			want: framework.Unschedulable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			podInformer := informerFactory.Core().V1().Pods()
			registeredPlugins := []st.RegisterPluginFunc{
				st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
				st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
			}

			fh, err := st.NewFramework(
				registeredPlugins,
				"default-scheduler",
				wait.NeverStop,
				frameworkruntime.WithClientSet(client),
				frameworkruntime.WithInformerFactory(informerFactory),
			)

			if err != nil {
				t.Fatalf("fail to create framework: %s", err)
			}

			cs := &CustomScheduler{
				handle: fh,
			}

			podList := []*v1.Pod{}
			for i := 0; i < 3; i++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: fmt.Sprintf("pod%d", i),
						Labels: map[string]string{
							"podGroup": "g1",
						},
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{},
					},
				}
				podList = append(podList, pod)
			}

			for _, p := range podList {
				podInformer.Informer().GetStore().Add(p)
			}
			fmt.Printf("finish adding")

			_, status := cs.PreFilter(tt.args.ctx, tt.args.state, tt.args.pod)

			if status.Code() != tt.want {
				t.Errorf("expected %v, got %v", tt.want, status.Code())
				return
			}
		})
	}
}

func makeNodeInfo(node string, milliCPU, memory int64) *framework.NodeInfo {
	ni := framework.NewNodeInfo()
	ni.SetNode(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: node},
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(milliCPU, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(memory, resource.BinarySI),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(milliCPU, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(memory, resource.BinarySI),
			},
		},
	})
	return ni
}

var _ framework.SharedLister = &fakeSharedLister{}

type fakeSharedLister struct {
	nodes []*framework.NodeInfo
}

func (f *fakeSharedLister) StorageInfos() framework.StorageInfoLister {
	return nil
}

func (f *fakeSharedLister) NodeInfos() framework.NodeInfoLister {
	return fakeframework.NodeInfoLister(f.nodes)
}
