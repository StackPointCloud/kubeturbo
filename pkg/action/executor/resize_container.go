package executor

import (
	kclient "k8s.io/client-go/kubernetes"
	k8sapi "k8s.io/client-go/pkg/api/v1"

	"github.com/turbonomic/kubeturbo/pkg/action/util"
	"github.com/turbonomic/kubeturbo/pkg/discovery/monitoring/kubelet"
	idutil "github.com/turbonomic/kubeturbo/pkg/discovery/util"
	goutil "github.com/turbonomic/kubeturbo/pkg/util"
	"github.com/turbonomic/turbo-go-sdk/pkg/proto"

	"fmt"
	"github.com/golang/glog"
	"time"
)

type containerResizeSpec struct {
	// the new capacity of the resources
	NewCapacity k8sapi.ResourceList

	// index of Pod's containers
	Index int
}

type ContainerResizer struct {
	kubeClient        *kclient.Clientset
	kubeletClient     *kubelet.KubeletClient
	k8sVersion        string
	noneSchedulerName string

	spec *containerResizeSpec
	//a map for concurrent control of Actions
	lockMap *util.ExpirationMap
}

func NewContainerResizer(client *kclient.Clientset, kubeletClient *kubelet.KubeletClient, k8sver, noschedulerName string, lmap *util.ExpirationMap) *ContainerResizer {
	return &ContainerResizer{
		kubeClient:        client,
		kubeletClient:     kubeletClient,
		k8sVersion:        k8sver,
		noneSchedulerName: noschedulerName,
		lockMap:           lmap,
	}
}

// get node cpu frequency, in KHz;
func (r *ContainerResizer) getNodeCPUFrequency(host string) (uint64, error) {
	return r.kubeletClient.GetMachineCpuFrequency(host)
}

func (r *ContainerResizer) setCPUCapacity(cpuMhz float64, host string, rlist k8sapi.ResourceList) error {
	cpuFrequency, err := r.getNodeCPUFrequency(host)
	if err != nil {
		glog.Errorf("failed to get node[%s] cpu frequency: %v", host, err)
		return err
	}

	cpuQuantity, err := genCPUQuantity(cpuMhz, cpuFrequency)
	if err != nil {
		glog.Errorf("failed to generate CPU quantity: %v", err)
		return err
	}

	rlist[k8sapi.ResourceCPU] = cpuQuantity
	return nil
}

// get commodity type and new capacity, and convert it into a k8s.Quantity.
func (r *ContainerResizer) buildNewCapacity(pod *k8sapi.Pod, actionItem *proto.ActionItemDTO) (k8sapi.ResourceList, error) {
	result := make(k8sapi.ResourceList)

	comm := actionItem.GetNewComm()
	ctype := comm.GetCommodityType()
	amount := comm.GetCapacity()

	//TODO: currently only support resizeCapacity; resizeReservation may be supported later
	if amount < 1 {
		msg := ""
		if comm.GetReservation() > 0 {
			msg = fmt.Sprintf("resizeReservation() is not supported yet.")
		} else {
			msg = fmt.Sprintf("new capacity should be bigger than zero (current=%.4f)", amount)
		}

		glog.Error(msg)
		return result, fmt.Errorf(msg)
	}

	switch ctype {
	case proto.CommodityDTO_VCPU:
		host := pod.Spec.NodeName
		err := r.setCPUCapacity(amount, host, result)
		if err != nil {
			glog.Errorf("failed to build cpu.Capacity: %v", err)
			return result, err
		}
	case proto.CommodityDTO_VMEM:
		memory, err := genMemoryQuantity(amount)
		if err != nil {
			glog.Errorf("failed to build mem.Capacity: %v", err)
			return result, err
		}
		result[k8sapi.ResourceMemory] = memory
	default:
		err := fmt.Errorf("Unsupport Commodity type[%v]", ctype)
		glog.Error(err)
		return result, err
	}

	return result, nil
}

func (r *ContainerResizer) buildResizeAction(actionItem *proto.ActionItemDTO) (*containerResizeSpec, *k8sapi.Pod, error) {

	//1. get hosting Pod and containerIndex
	entity := actionItem.GetTargetSE()
	containerId := entity.GetId()

	podId, containerIndex, err := idutil.ParseContainerId(containerId)
	if err != nil {
		glog.Errorf("failed to parse podId to build resizeAction: %v", err)
		return nil, nil, err
	}

	podEntity := actionItem.GetHostedBySE()
	podId2 := podEntity.GetId()
	if podId2 != podId {
		err = fmt.Errorf("hosting pod(%s) Id mismatch [%v Vs. %v]", podEntity.GetDisplayName(), podId, podId2)
		glog.Error(err)
		return nil, nil, err
	}

	pod, err := util.GetPodFromUUID(r.kubeClient, podId)
	if err != nil {
		glog.Errorf("failed to get hosting Pod to build resizeAction: %v", err)
		return nil, nil, err
	}

	//2. build the new resource Capacity
	newCapacity, err := r.buildNewCapacity(pod, actionItem)
	if err != nil {
		glog.Errorf("failed to build NewCapacity to build resizeAction: %v", err)
		return nil, nil, err
	}

	//3. build the turboAction object
	resizeSpec := &containerResizeSpec{
		Index:       containerIndex,
		NewCapacity: newCapacity,
	}

	return resizeSpec, pod, nil
}

func (r *ContainerResizer) Execute(actionItem *proto.ActionItemDTO) error {
	if actionItem == nil {
		glog.Errorf("potential bug: actionItem is null.")
		return fmt.Errorf("ActionItem is null.")
	}

	//1. build turboAction
	spec, pod, err := r.buildResizeAction(actionItem)
	if err != nil {
		glog.Errorf("failed to execute container resize: %v", err)
		return fmt.Errorf("Failed.")
	}

	//2. execute the Action
	err = r.executeAction(spec, pod)
	if err != nil {
		glog.Errorf("failed to execute Action: %v", err)
		return fmt.Errorf("Failed.")
	}

	//3. check action result
	fullName := util.BuildIdentifier(pod.Namespace, pod.Name)
	glog.V(2).Infof("begin to check result of resizeContainer[%v].", fullName)
	if err = r.checkPod(pod); err != nil {
		glog.Errorf("failed to check pod[%v] for resize action: %v", fullName, err)
		return fmt.Errorf("Failed")
	}
	glog.V(2).Infof("Action resizeContainer[%v] succeeded.", fullName)

	return nil
}

func (r *ContainerResizer) executeAction(resizeSpec *containerResizeSpec, pod *k8sapi.Pod) error {
	//1. check
	if len(resizeSpec.NewCapacity) < 1 {
		glog.Warningf("Resize specification is empty.")
		return nil
	}

	//2. get parent controller
	fullName := util.BuildIdentifier(pod.Namespace, pod.Name)
	parentKind, parentName, err := util.GetPodParentInfo(pod)
	if err != nil {
		glog.Errorf("failed to get pod[%s] parent info: %v", fullName, err)
		return err
	}

	if parentKind == "" {
		err = r.resizeBarePodContainer(pod, resizeSpec.Index, resizeSpec.NewCapacity)
	} else {
		err = r.resizeControllerContainer(pod, parentKind, parentName, resizeSpec.Index, resizeSpec.NewCapacity)
	}

	if err != nil {
		glog.Errorf("resize Pod[%s]-%d container failed: %v", fullName, resizeSpec.Index)
	}

	return nil
}

func (r *ContainerResizer) resizeControllerContainer(pod *k8sapi.Pod, parentKind, parentName string, index int, capacity k8sapi.ResourceList) error {
	id := fmt.Sprintf("%s/%s-%d", pod.Namespace, pod.Name, index)
	glog.V(2).Infof("begin to resizeContainer[%s] parent=%s/%s.", id, parentKind, parentName)

	//1. set up
	highver := true
	if goutil.CompareVersion(r.k8sVersion, HigherK8sVersion) < 0 {
		highver = false
	}
	noexist := r.noneSchedulerName
	helper, err := NewSchedulerHelper(r.kubeClient, pod.Namespace, pod.Name, parentKind, parentName, noexist, highver)
	if err != nil {
		glog.Errorf("resizeContainer failed[%s]: failed to create helper: %v", id, err)
		return err
	}
	if err := helper.SetupLock(r.lockMap); err != nil {
		return err
	}

	//2. wait to get a lock of the parent object
	timeout := defaultWaitLockTimeOut
	interval := defaultWaitLockSleep
	err = goutil.RetryDuring(1000, timeout, interval, func() error {
		if !helper.Acquirelock() {
			return fmt.Errorf("TryLater")
		}
		return nil
	})
	if err != nil {
		glog.Errorf("resizeContainer failed[%s]: failed to acquire lock of parent[%s]", id, parentName)
		return err
	}
	glog.V(3).Infof("resizeContainer [%s]: got lock for parent[%s]", id, parentName)
	helper.KeepRenewLock()

	//3. defer function for cleanUp
	defer func() {
		helper.CleanUp()
		util.CleanPendingPod(r.kubeClient, pod.Namespace, noexist, parentKind, parentName, highver)
	}()

	//4. disable the scheduler of  the parentController
	preScheduler, err := helper.UpdateScheduler(noexist, defaultRetryLess)
	if err != nil {
		glog.Errorf("resizeContainer failed[%s]: failed to disable parentController-[%s]'s scheduler.", id, parentName)
		return fmt.Errorf("TryLater")
	}

	//5.resize Container and restore parent's scheduler
	helper.SetScheduler(preScheduler)
	err = resizeContainer(r.kubeClient, pod, index, capacity, defaultRetryLess)
	if err != nil {
		glog.Errorf("resizeContainer failed[%s]: %v", id, err)
		return fmt.Errorf("TryLater")
	}

	return nil
}

func (r *ContainerResizer) resizeBarePodContainer(pod *k8sapi.Pod, index int, capacity k8sapi.ResourceList) error {
	podkey := util.BuildIdentifier(pod.Namespace, pod.Name)
	// 1. setup lockHelper
	helper, err := util.NewLockHelper(podkey, r.lockMap)
	if err != nil {
		return err
	}

	// 2. wait to get a lock of current Pod
	timeout := defaultWaitLockTimeOut
	interval := defaultWaitLockSleep
	err = helper.Trylock(timeout, interval)
	if err != nil {
		glog.Errorf("resizeContainer failed[%s]: failed to acquire lock of pod[%s]", podkey)
		return err
	}
	defer helper.ReleaseLock()

	// 3. resize Pod.container
	helper.KeepRenewLock()
	err = resizeContainer(r.kubeClient, pod, index, capacity, defaultRetryMore)
	return err
}

// check the liveness of pod
// return (retry, error)
func doCheckPod(client *kclient.Clientset, namespace, name string) (bool, error) {
	pod, err := util.GetPod(client, namespace, name)
	if err != nil {
		return true, err
	}

	phase := pod.Status.Phase
	if phase == k8sapi.PodRunning {
		return false, nil
	}

	if pod.DeletionGracePeriodSeconds != nil {
		return false, fmt.Errorf("Pod is being deleted.")
	}

	return true, fmt.Errorf("pod is not in running phase[%v] yet.", phase)
}

func (r *ContainerResizer) checkPod(pod *k8sapi.Pod) error {
	retryNum := defaultRetryMore
	interval := defaultPodCreateSleep
	timeout := time.Duration(retryNum+1) * interval
	err := goutil.RetrySimple(retryNum, timeout, interval, func() (bool, error) {
		return doCheckPod(r.kubeClient, pod.Namespace, pod.Name)
	})

	if err != nil {
		return err
	}

	return nil
}
