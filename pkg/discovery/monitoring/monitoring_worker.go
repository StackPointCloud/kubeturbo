package monitoring

import (
	"errors"
	"fmt"

	"github.com/turbonomic/kubeturbo/pkg/discovery/metrics"
	"github.com/turbonomic/kubeturbo/pkg/discovery/monitoring/k8sconntrack"
	"github.com/turbonomic/kubeturbo/pkg/discovery/monitoring/kubelet"
	"github.com/turbonomic/kubeturbo/pkg/discovery/monitoring/master"
	"github.com/turbonomic/kubeturbo/pkg/discovery/monitoring/types"
	"github.com/turbonomic/kubeturbo/pkg/discovery/task"
)

type MonitorWorkerConfig interface {
	GetMonitorType() types.MonitorType
	GetMonitoringSource() types.MonitoringSource
}

type MonitoringWorker interface {
	Do() *metrics.EntityMetricSink
	Stop()
	ReceiveTask(task *task.Task)
	GetMonitoringSource() types.MonitoringSource
}

type ResourceMonitoringWorker interface {
	MonitoringWorker
	RetrieveResourceStat() error
}

type StateMonitoringWorker interface {
	MonitoringWorker
	RetrieveClusterStat() error
}

func BuildMonitorWorker(source types.MonitoringSource, config MonitorWorkerConfig) (MonitoringWorker, error) {
	// Build monitoring client
	switch source {
	case types.KubeletSource:
		kubeletConfig, ok := config.(*kubelet.KubeletMonitorConfig)
		if !ok {
			return nil, errors.New("failed to build a Kubelet monitoring client as the provided config was not a KubeletMonitorConfig")
		}
		return kubelet.NewKubeletMonitor(kubeletConfig)
	case types.ClusterSource:
		clusterMonitorConfig, ok := config.(*master.ClusterMonitorConfig)
		if !ok {
			return nil, errors.New("Failed to build a cluster monitoring client as the provided config was not a ClusterMonitorConfig")
		}
		return master.NewClusterMonitor(clusterMonitorConfig)
	case types.K8sConntrackSource:
		k8sconntrackMonitoring, ok := config.(*k8sconntrack.K8sConntrackMonitorConfig)
		if !ok {
			return nil, errors.New("Failed to build a k8sconntrack monitoring client as the provided config was not a K8sConntrackConfig")
		}
		return k8sconntrack.NewK8sConntrackMonitor(k8sconntrackMonitoring)

	//case "Prometheus":
	default:
		return nil, fmt.Errorf("Unsupported monitoring source %s", source)
	}

}
