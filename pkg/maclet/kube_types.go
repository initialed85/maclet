package maclet

import kubeapi "github.com/initialed85/maclet/pkg/kube"

// Short aliases keep the application code readable while pkg/kube centralizes
// the upstream Kubernetes object mapping.
type ObjectMeta = kubeapi.ObjectMeta
type Taint = kubeapi.Taint
type NodeSpec = kubeapi.NodeSpec
type NodeAddress = kubeapi.NodeAddress
type NodeCondition = kubeapi.NodeCondition
type NodeInfo = kubeapi.NodeInfo
type NodeStatus = kubeapi.NodeStatus
type Node = kubeapi.Node
type NodeList = kubeapi.NodeList
type Lease = kubeapi.Lease
type LeaseSpec = kubeapi.LeaseSpec
type PodList = kubeapi.PodList
type Pod = kubeapi.Pod
type PodSpec = kubeapi.PodSpec
type Volume = kubeapi.Volume
type HostPathVolumeSource = kubeapi.HostPathVolumeSource
type ContainerSpec = kubeapi.ContainerSpec
type ContainerPort = kubeapi.ContainerPort
type EnvVar = kubeapi.EnvVar
type EnvVarSource = kubeapi.EnvVarSource
type ObjectFieldSelector = kubeapi.ObjectFieldSelector
type VolumeMount = kubeapi.VolumeMount
type PodIP = kubeapi.PodIP
type PodCondition = kubeapi.PodCondition
type ContainerState = kubeapi.ContainerState
type ContainerStateWaiting = kubeapi.ContainerStateWaiting
type ContainerStateRunning = kubeapi.ContainerStateRunning
type ContainerStateTerminated = kubeapi.ContainerStateTerminated
type ContainerStatus = kubeapi.ContainerStatus
type PodStatus = kubeapi.PodStatus
