/*
 Licensed to the Apache Software Foundation (ASF) under one
 or more contributor license agreements.  See the NOTICE file
 distributed with this work for additional information
 regarding copyright ownership.  The ASF licenses this file
 to you under the Apache License, Version 2.0 (the
 "License"); you may not use this file except in compliance
 with the License.  You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package common

import (
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"

	"github.com/apache/yunikorn-k8shim/pkg/log"
	siCommon "github.com/apache/yunikorn-scheduler-interface/lib/go/common"
	"github.com/apache/yunikorn-scheduler-interface/lib/go/si"
)

// resource builder is a helper struct to construct si resources
type ResourceBuilder struct {
	resourceMap map[string]*si.Quantity
}

func NewResourceBuilder() *ResourceBuilder {
	return &ResourceBuilder{
		resourceMap: make(map[string]*si.Quantity),
	}
}

func (w *ResourceBuilder) AddResource(name string, value int64) *ResourceBuilder {
	w.resourceMap[name] = &si.Quantity{Value: value}
	return w
}

func (w *ResourceBuilder) Build() *si.Resource {
	return &si.Resource{Resources: w.resourceMap}
}

// Get the resources from a pod's containers and convert that into a internal resource.
// A pod has two resource parts: Requests and Limits.
// Based on the values a pod gets a QOS assigned, as per
// https://kubernetes.io/docs/tasks/configure-pod-container/quality-service-pod/
// QOS class Guaranteed and Burstable are supported. However Burstable is scheduled based on the request
// values, limits are ignored in the current setup.
// BestEffort pods are scheduled using a minimum resource of 1MB only.
func GetPodResource(pod *v1.Pod) (resource *si.Resource) {
	// var memory, vcore = int64(0), int64(0)
	var podResource *si.Resource
	// A QosBestEffort pod does not request any resources and thus cannot be
	// scheduled. Handle a QosBestEffort pod by setting a tiny memory value.
	if qos.GetPodQOS(pod) == v1.PodQOSBestEffort {
		resources := NewResourceBuilder()
		resources.AddResource(siCommon.Memory, 1000000)
		return resources.Build()
	}

	for _, c := range pod.Spec.Containers {
		resourceList := c.Resources.Requests
		containerResource := getResource(resourceList)
		podResource = Add(podResource, containerResource)
	}

	// each resource compare between initcontainer and sum of containers
	// max(sum(Containers requirement), InitContainers requirement)
	if pod.Spec.InitContainers != nil {
		checkInitContainerRequest(pod, podResource)
	}

	// K8s pod EnableOverHead from:
	// alpha: v1.16
	// beta: v1.18
	// Enables PodOverhead, for accounting pod overheads which are specific to a given RuntimeClass

	// If Overhead is being utilized, add to the total requests for the pod
	if pod.Spec.Overhead != nil {
		podOverHeadResource := getResource(pod.Spec.Overhead)
		podResource = Add(podResource, podOverHeadResource)
		// Logging the overall pod size and pod overhead
		log.Logger().Debug("Pod overhead specified, overall pod size adjusted",
			zap.String("taskID", string(pod.UID)),
			zap.Stringer("overallSize", podResource),
			zap.Stringer("overheadSize", podOverHeadResource))
	}

	return podResource
}

func checkInitContainerRequest(pod *v1.Pod, containersResources *si.Resource) {
	for _, c := range pod.Spec.InitContainers {
		resourceList := c.Resources.Requests
		ICResource := getResource(resourceList)
		for resourceName, ICRequest := range ICResource.Resources {
			containersRequests, exist := containersResources.Resources[resourceName]
			// addtional resource request from init cont, add it to request.
			if !exist {
				containersResources.Resources[resourceName] = ICRequest
				continue
			}
			if ICRequest.GetValue() > containersRequests.GetValue() {
				containersResources.Resources[resourceName] = ICRequest
			}
		}
	}
}

func GetNodeResource(nodeStatus *v1.NodeStatus) *si.Resource {
	// Capacity represents the total capacity of the node
	// Allocatable represents the resources of a node that are available for scheduling.
	// Each kubelet can reserve some resources from the scheduler.
	// We can rely on Allocatable resource here, because if it is not specified,
	// the default value is same as Capacity. (same behavior as the default-scheduler)
	return getResource(nodeStatus.Allocatable)
}

// parse cpu and memory from string to si.Resource, both of them are optional
// if parse failed with some errors, log the error and return a nil
func ParseResource(cpuStr, memStr string) *si.Resource {
	if cpuStr == "" && memStr == "" {
		return nil
	}

	result := NewResourceBuilder()
	if cpuStr != "" {
		if vcore, err := resource.ParseQuantity(cpuStr); err == nil {
			result.AddResource(siCommon.CPU, vcore.MilliValue())
		} else {
			log.Logger().Error("failed to parse cpu resource",
				zap.String("cpuStr", cpuStr),
				zap.Error(err))
			return nil
		}
	}

	if memStr != "" {
		if mem, err := resource.ParseQuantity(memStr); err == nil {
			result.AddResource(siCommon.Memory, mem.Value())
		} else {
			log.Logger().Error("failed to parse memory resource",
				zap.String("memStr", memStr),
				zap.Error(err))
			return nil
		}
	}

	return result.Build()
}

func GetResource(resMap map[string]string) *si.Resource {
	result := NewResourceBuilder()
	for resName, resValue := range resMap {
		switch resName {
		case v1.ResourceCPU.String():
			if actualValue, err := resource.ParseQuantity(resValue); err == nil {
				result.AddResource(siCommon.CPU, actualValue.MilliValue())
			} else {
				log.Logger().Error("failed to parse cpu resource",
					zap.String("res name", "cpu"),
					zap.String("res value", resValue),
					zap.Error(err))
				return nil
			}
		default:
			if actualValue, err := resource.ParseQuantity(resValue); err == nil {
				result.AddResource(resName, actualValue.Value())
			} else {
				log.Logger().Error("failed to parse resource",
					zap.String("res name", resName),
					zap.String("res value", resValue),
					zap.Error(err))
				return nil
			}
		}
	}
	return result.Build()
}

func GetTGResource(resMap map[string]resource.Quantity, members int64) *si.Resource {
	result := NewResourceBuilder()
	for resName, resValue := range resMap {
		switch resName {
		case v1.ResourceCPU.String():
			result.AddResource(siCommon.CPU, members*resValue.MilliValue())
		default:
			result.AddResource(resName, members*resValue.Value())
		}
	}
	return result.Build()
}

func getResource(resourceList v1.ResourceList) *si.Resource {
	resources := NewResourceBuilder()
	for name, value := range resourceList {
		switch name {
		case v1.ResourceCPU:
			vcore := value.MilliValue()
			resources.AddResource(siCommon.CPU, vcore)
		default:
			resources.AddResource(string(name), value.Value())
		}
	}
	return resources.Build()
}

func Equals(left *si.Resource, right *si.Resource) bool {
	if left == right {
		return true
	}

	if left != nil && left.Resources != nil {
		for k, v := range left.Resources {
			if right == nil ||
				right.Resources[k] == nil ||
				right.Resources[k].Value != v.Value {
				return false
			}
		}
	}

	if right != nil && right.Resources != nil {
		for k, v := range right.Resources {
			if left == nil ||
				left.Resources[k] == nil ||
				left.Resources[k].Value != v.Value {
				return false
			}
		}
	}

	return true
}

func Add(left *si.Resource, right *si.Resource) *si.Resource {
	result := &si.Resource{Resources: make(map[string]*si.Quantity)}
	if left == nil && right == nil {
		return result
	}
	if right != nil {
		for k, v := range right.Resources {
			result.Resources[k] = v
		}
	}
	if left != nil {
		for k, v := range left.Resources {
			if er, ok := result.Resources[k]; ok {
				result.Resources[k] = &si.Quantity{Value: int64(er.Value + v.Value)}
				continue
			}
			result.Resources[k] = v
		}
	}
	return result
}

func Sub(left *si.Resource, right *si.Resource) *si.Resource {
	if left == nil {
		left = &si.Resource{}
	}
	if right == nil {
		return left
	}

	// clone left
	rb := NewResourceBuilder()
	for k, v := range left.Resources {
		rb.AddResource(k, v.Value)
	}
	result := rb.Build()

	// sub right
	for k, v := range right.Resources {
		if _, ok := result.Resources[k]; !ok {
			result.Resources[k] = &si.Quantity{
				Value: -v.Value,
			}
		} else {
			result.Resources[k].Value -= v.Value
		}
	}

	return result
}

func IsZero(r *si.Resource) bool {
	if r == nil {
		return true
	}
	for _, v := range r.Resources {
		if v.Value != 0 {
			return false
		}
	}
	return true
}
