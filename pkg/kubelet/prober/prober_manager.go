/*
Copyright 2015 The Kubernetes Authors.

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

package prober

import (
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"k8s.io/component-base/metrics"
	"k8s.io/klog/v2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/status"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	kubeutil "k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/utils/clock"
)

// ProberResults stores the cumulative number of a probe by result as prometheus metrics.
var ProberResults = metrics.NewCounterVec(
	&metrics.CounterOpts{
		Subsystem:      "prober",
		Name:           "probe_total",
		Help:           "Cumulative number of a liveness, readiness or startup probe for a container by result.",
		StabilityLevel: metrics.ALPHA,
	},
	[]string{"probe_type",
		"result",
		"container",
		"pod",
		"namespace",
		"pod_uid"},
)

// ProberDuration stores the duration of a successful probe lifecycle by result as prometheus metrics.
var ProberDuration = metrics.NewHistogramVec(
	&metrics.HistogramOpts{
		Subsystem:      "prober",
		Name:           "probe_duration_seconds",
		Help:           "Duration in seconds for a probe response.",
		StabilityLevel: metrics.ALPHA,
	},
	[]string{"probe_type",
		"container",
		"pod",
		"namespace"},
)

// Manager manages pod probing. It creates a probe "worker" for every container that specifies a
// probe (AddPod). The worker periodically probes its assigned container and caches the results. The
// manager use the cached probe results to set the appropriate Ready state in the PodStatus when
// requested (UpdatePodStatus). Updating probe parameters is not currently supported.
type Manager interface {
	// AddPod creates new probe workers for every container probe. This should be called for every
	// pod created.
	AddPod(pod *v1.Pod)

	// StopLivenessAndStartup handles stopping liveness and startup probes during termination.
	StopLivenessAndStartup(pod *v1.Pod)

	// RemovePod handles cleaning up the removed pod state, including terminating probe workers and
	// deleting cached results.
	RemovePod(pod *v1.Pod)

	// CleanupPods handles cleaning up pods which should no longer be running.
	// It takes a map of "desired pods" which should not be cleaned up.
	CleanupPods(desiredPods map[types.UID]sets.Empty)

	// UpdatePodStatus modifies the given PodStatus with the appropriate Ready state for each
	// container based on container running status, cached probe results and worker states.
	UpdatePodStatus(*v1.Pod, *v1.PodStatus)
}

// manger中包含了很多Manager，它们来自results.Manager和status.Manager
type manager struct {
	// Map of active workers for probes
	workers map[probeKey]*worker
	// Lock for accessing & mutating workers
	workerLock sync.RWMutex

	// The statusManager cache provides pod IP and container IDs for probing.
	statusManager status.Manager

	// readinessManager manages the results of readiness probes
	readinessManager results.Manager

	// livenessManager manages the results of liveness probes
	livenessManager results.Manager

	// startupManager manages the results of startup probes
	startupManager results.Manager

	// prober executes the probe actions.
	prober *prober

	start time.Time
}

// NewManager creates a Manager for pod probing.
func NewManager(
	statusManager status.Manager,
	livenessManager results.Manager,
	readinessManager results.Manager,
	startupManager results.Manager,
	runner kubecontainer.CommandRunner,
	recorder record.EventRecorder) Manager {

	prober := newProber(runner, recorder)
	return &manager{
		statusManager:    statusManager,
		prober:           prober,
		readinessManager: readinessManager,
		livenessManager:  livenessManager,
		startupManager:   startupManager,
		workers:          make(map[probeKey]*worker),
		start:            clock.RealClock{}.Now(),
	}
}

// Key uniquely identifying container probes
type probeKey struct {
	podUID        types.UID
	containerName string
	probeType     probeType
}

// Type of probe (liveness, readiness or startup)
type probeType int

const (
	liveness probeType = iota
	readiness
	startup

	probeResultSuccessful string = "successful"
	probeResultFailed     string = "failed"
	probeResultUnknown    string = "unknown"
)

// For debugging.
func (t probeType) String() string {
	switch t {
	case readiness:
		return "Readiness"
	case liveness:
		return "Liveness"
	case startup:
		return "Startup"
	default:
		return "UNKNOWN"
	}
}

func getRestartableInitContainers(pod *v1.Pod) []v1.Container {
	var restartableInitContainers []v1.Container
	for _, c := range pod.Spec.InitContainers {
		if kubetypes.IsRestartableInitContainer(&c) {
			restartableInitContainers = append(restartableInitContainers, c)
		}
	}
	return restartableInitContainers
}

// manager负责管理一堆workers，根据probkey进行索引
func (m *manager) AddPod(pod *v1.Pod) {
	m.workerLock.Lock()
	defer m.workerLock.Unlock()

	key := probeKey{podUID: pod.UID}
	for _, c := range append(pod.Spec.Containers, getRestartableInitContainers(pod)...) {
		key.containerName = c.Name
		//REVIEW - 根据Pod的spec中的生命来创建
		if c.StartupProbe != nil {
			key.probeType = startup //probekey的三个元素全部组装好了
			if _, ok := m.workers[key]; ok {
				klog.V(8).ErrorS(nil, "Startup probe already exists for container",
					"pod", klog.KObj(pod), "containerName", c.Name)
				return
			}
			w := newWorker(m, startup, pod, c)
			m.workers[key] = w
			go w.run() //分配好一个worker开启一个goroutine
		}

		if c.ReadinessProbe != nil {
			key.probeType = readiness
			if _, ok := m.workers[key]; ok {
				klog.V(8).ErrorS(nil, "Readiness probe already exists for container",
					"pod", klog.KObj(pod), "containerName", c.Name)
				return
			}
			w := newWorker(m, readiness, pod, c)
			m.workers[key] = w
			go w.run()
		}

		if c.LivenessProbe != nil {
			key.probeType = liveness
			if _, ok := m.workers[key]; ok {
				klog.V(8).ErrorS(nil, "Liveness probe already exists for container",
					"pod", klog.KObj(pod), "containerName", c.Name)
				return
			}
			w := newWorker(m, liveness, pod, c)
			m.workers[key] = w
			go w.run()
		}
	}
}

// 先关闭 pod 的 liveness 和 startup 探针，然后给 pod 发送 SIGTERM 信号，超时未退出则强制 SIGKILL 杀掉容器.
func (m *manager) StopLivenessAndStartup(pod *v1.Pod) {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()

	key := probeKey{podUID: pod.UID}
	for _, c := range pod.Spec.Containers {
		key.containerName = c.Name
		for _, probeType := range [...]probeType{liveness, startup} {
			key.probeType = probeType
			if worker, ok := m.workers[key]; ok {
				worker.stop() //worker可以直接stop
			}
		}
	}
}

// LINK - pkg/kubelet/kubelet.go
// 最后删除该 pod 所有类型的探针
func (m *manager) RemovePod(pod *v1.Pod) {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()

	key := probeKey{podUID: pod.UID}
	for _, c := range append(pod.Spec.Containers, getRestartableInitContainers(pod)...) {
		key.containerName = c.Name
		for _, probeType := range [...]probeType{readiness, liveness, startup} {
			key.probeType = probeType
			if worker, ok := m.workers[key]; ok {
				worker.stop()
			}
		}
	}
}

// kubelet 周期性执行 HandlePodCleanups 方法, 该方法会清理一波状态异常的容器, 同时也会清理探针.
func (m *manager) CleanupPods(desiredPods map[types.UID]sets.Empty) {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()
	for key, worker := range m.workers {
		//注意这里是!ok，所以不在desiredPods里面的会被stop
		if _, ok := desiredPods[key.podUID]; !ok {
			worker.stop()
		}
	}
}

// 这里是Container是否started，而不是Pod！！！！
func (m *manager) isContainerStarted(pod *v1.Pod, containerStatus *v1.ContainerStatus) bool {
	if containerStatus.State.Running == nil {
		return false
	}

	if result, ok := m.startupManager.Get(kubecontainer.ParseContainerID(containerStatus.ContainerID)); ok {
		return result == results.Success
	}

	// if there is a startup probe which hasn't run yet, the container is not
	// started.
	if _, exists := m.getWorker(pod.UID, containerStatus.Name, startup); exists {
		return false
	}

	// there is no startup probe, so the container is started.
	return true
}

func (m *manager) UpdatePodStatus(pod *v1.Pod, podStatus *v1.PodStatus) {
	//更新container的started和ready状态
	for i, c := range podStatus.ContainerStatuses {
		started := m.isContainerStarted(pod, &podStatus.ContainerStatuses[i])
		podStatus.ContainerStatuses[i].Started = &started

		if !started {
			continue
		}

		var ready bool
		if c.State.Running == nil {
			ready = false
		} else if result, ok := m.readinessManager.Get(kubecontainer.ParseContainerID(c.ContainerID)); ok && result == results.Success {
			ready = true
		} else {
			// The check whether there is a probe which hasn't run yet.
			w, exists := m.getWorker(pod.UID, c.Name, readiness)
			ready = !exists // no readinessProbe -> always ready
			if exists {
				// Trigger an immediate run of the readinessProbe to update ready state
				select {
				case w.manualTriggerCh <- struct{}{}:
				default: // Non-blocking.
					klog.InfoS("Failed to trigger a manual run", "probe", w.probeType.String())
				}
			}
		}
		podStatus.ContainerStatuses[i].Ready = ready
	}
	//initContainer在用完会被删除吗
	for i, c := range podStatus.InitContainerStatuses {
		started := m.isContainerStarted(pod, &podStatus.InitContainerStatuses[i])
		podStatus.InitContainerStatuses[i].Started = &started

		initContainer, ok := kubeutil.GetContainerByIndex(pod.Spec.InitContainers, podStatus.InitContainerStatuses, i)
		if !ok {
			klog.V(4).InfoS("Mismatch between pod spec and status, likely programmer error", "pod", klog.KObj(pod), "containerName", c.Name)
			continue
		}
		if !kubetypes.IsRestartableInitContainer(&initContainer) {
			if c.State.Terminated != nil && c.State.Terminated.ExitCode == 0 {
				podStatus.InitContainerStatuses[i].Ready = true
			}
			continue
		}

		if !started {
			continue
		}

		var ready bool
		if c.State.Running == nil {
			ready = false
		} else if result, ok := m.readinessManager.Get(kubecontainer.ParseContainerID(c.ContainerID)); ok && result == results.Success {
			ready = true
		} else {
			// The check whether there is a probe which hasn't run yet.
			w, exists := m.getWorker(pod.UID, c.Name, readiness)
			ready = !exists // no readinessProbe -> always ready
			if exists {
				// Trigger an immediate run of the readinessProbe to update ready state
				select {
				case w.manualTriggerCh <- struct{}{}:
				default: // Non-blocking.
					klog.InfoS("Failed to trigger a manual run", "probe", w.probeType.String())
				}
			}
		}
		podStatus.InitContainerStatuses[i].Ready = ready
	}
}

func (m *manager) getWorker(podUID types.UID, containerName string, probeType probeType) (*worker, bool) {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()
	worker, ok := m.workers[probeKey{podUID, containerName, probeType}]
	return worker, ok
}

// Called by the worker after exiting.
func (m *manager) removeWorker(podUID types.UID, containerName string, probeType probeType) {
	m.workerLock.Lock()
	defer m.workerLock.Unlock()
	delete(m.workers, probeKey{podUID, containerName, probeType})
}

// workerCount returns the total number of probe workers. For testing.
func (m *manager) workerCount() int {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()
	return len(m.workers)
}
