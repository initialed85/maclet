// Package kube exposes the Kubernetes API types used by maclet's control-plane
// and workload paths. Keeping these aliases in one package makes it explicit
// that wire objects come from upstream Kubernetes rather than local lookalikes.
package kube

import (
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ObjectMeta = metav1.ObjectMeta
type Taint = corev1.Taint
type NodeSpec = corev1.NodeSpec
type NodeAddress = corev1.NodeAddress
type NodeCondition = corev1.NodeCondition
type NodeInfo = corev1.NodeSystemInfo
type NodeStatus = corev1.NodeStatus
type Node = corev1.Node
type NodeList = corev1.NodeList
type Lease = coordinationv1.Lease
type LeaseSpec = coordinationv1.LeaseSpec
type PodList = corev1.PodList
type Pod = corev1.Pod
type PodSpec = corev1.PodSpec
type Service = corev1.Service
type ServiceList = corev1.ServiceList
type Volume = corev1.Volume
type HostPathVolumeSource = corev1.HostPathVolumeSource
type ContainerSpec = corev1.Container
type ContainerPort = corev1.ContainerPort
type EnvVar = corev1.EnvVar
type EnvVarSource = corev1.EnvVarSource
type ObjectFieldSelector = corev1.ObjectFieldSelector
type VolumeMount = corev1.VolumeMount
type PodIP = corev1.PodIP
type PodCondition = corev1.PodCondition
type ContainerState = corev1.ContainerState
type ContainerStateWaiting = corev1.ContainerStateWaiting
type ContainerStateRunning = corev1.ContainerStateRunning
type ContainerStateTerminated = corev1.ContainerStateTerminated
type ContainerStatus = corev1.ContainerStatus
type PodStatus = corev1.PodStatus
