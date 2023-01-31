//go:build !dockerless
// +build !dockerless

/*
Copyright 2019 The Kubernetes Authors.

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

package dockershim

import (
	"context"
	"fmt"
	"github.com/google/cadvisor/cache/memory"
	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	"github.com/google/cadvisor/manager"
	"github.com/google/cadvisor/utils/sysfs"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	cc "k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/utils/pointer"
	"net/http"
	"time"

	cadvisormetrics "github.com/google/cadvisor/container"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

// ContainerStats returns stats for a container stats request based on container id.
func (ds *dockerService) ContainerStats(_ context.Context, r *runtimeapi.ContainerStatsRequest) (*runtimeapi.ContainerStatsResponse, error) {
	stats, err := ds.getContainerStats(r.ContainerId)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.ContainerStatsResponse{Stats: stats}, nil
}

// ListContainerStats returns stats for a list container stats request based on a filter.
func (ds *dockerService) ListContainerStats(ctx context.Context, r *runtimeapi.ListContainerStatsRequest) (*runtimeapi.ListContainerStatsResponse, error) {
	//fmt.Println("kinara: calling cadvisor method")

	if ds.cadvisorClient1 == nil {
		sysFs := sysfs.NewRealSysFs()
		duration := 15 * time.Second
		allowDynamicHousekeeping := true
		statsCacheDuration := 2 * time.Minute
		housekeepingConfig := manager.HouskeepingConfig{
			Interval:     &duration,
			AllowDynamic: pointer.BoolPtr(allowDynamicHousekeeping),
		}

		var cgroupRoots []string
		// hardcoding for now
		cgroupRoot := cc.NodeAllocatableRoot("/", true, ds.cgroupDriver)
		if cgroupRoot != "" {
			fmt.Println("kinara: appending cgroupRoot", cgroupRoot)
			cgroupRoots = append(cgroupRoots, cgroupRoot)
		}
		kubeletCgroup, err := cc.GetKubeletContainer("")
		if err != nil {
			fmt.Errorf("kinara: kubeletGroup error! %v", err)
		}
		if kubeletCgroup != "" {
			cgroupRoots = append(cgroupRoots, kubeletCgroup)
		}
		// ignoring runtimeCgroups and systemCgroups - optional

		includedMetrics := cadvisormetrics.MetricSet{
			cadvisormetrics.CpuUsageMetrics:         struct{}{},
			cadvisormetrics.MemoryUsageMetrics:      struct{}{},
			cadvisormetrics.CpuLoadMetrics:          struct{}{},
			cadvisormetrics.DiskIOMetrics:           struct{}{},
			cadvisormetrics.NetworkUsageMetrics:     struct{}{},
			cadvisormetrics.AcceleratorUsageMetrics: struct{}{},
			cadvisormetrics.AppMetrics:              struct{}{},
			cadvisormetrics.ProcessMetrics:          struct{}{},
			cadvisormetrics.DiskUsageMetrics:        struct{}{},
		}

		// Create the cAdvisor container manager.
		m, err := manager.New(memory.New(statsCacheDuration, nil), sysFs, housekeepingConfig, includedMetrics, http.DefaultClient, cgroupRoots, "")
		if err != nil {
			return nil, err
		}

		ds.cadvisorClient1 = &cadvisorClient{Manager: m, rootPath: "/var/lib/kubelet"}
		err = ds.cadvisorClient1.Start()
		if err != nil {
			fmt.Println("kinara: error starting cadvisorClient", err)
			return nil, err
		}

		fmt.Println("kinara: registered cadvisorClient1")
	}

	_, err := ds.cadvisorClient1.DockerInfo()
	if err != nil {
		fmt.Println("kinara: err", err)
	}

	rootFsInfo, err := ds.cadvisorClient1.RootFsInfo()
	if err != nil {
		fmt.Println("kinara: rootFsInfo err", err)
	} else {
		fmt.Println("rootFsInfo ", rootFsInfo)
	}

	imageFsInfo, err := ds.cadvisorClient1.ImagesFsInfo()
	if err != nil {
		fmt.Println("kinara: imageFsInfo err", err)
	} else {
		fmt.Println("imageFsInfo ", imageFsInfo)
	}

	//infos, err := getCadvisorContainerInfo(ds.cadvisorClient1)
	//if err != nil {
	//	fmt.Println("kinara: getCadvisorContainerInfo", err)
	//} else {
	//	fmt.Println("infos ", infos)
	//}

	var stats1 []*runtimeapi.ContainerStats
	return &runtimeapi.ListContainerStatsResponse{Stats: stats1}, nil

	var stats []*runtimeapi.ContainerStats

	containerStatsFilter := r.GetFilter()
	filter := &runtimeapi.ContainerFilter{}

	if containerStatsFilter != nil {
		filter.Id = containerStatsFilter.Id
		filter.PodSandboxId = containerStatsFilter.PodSandboxId
		filter.LabelSelector = containerStatsFilter.LabelSelector
	}

	listResp, err := ds.ListContainers(ctx, &runtimeapi.ListContainersRequest{Filter: filter})
	if err != nil {
		return nil, err
	}

	for _, container := range listResp.Containers {
		containerStats, err := ds.getContainerStats(container.Id)
		if err != nil {
			return nil, err
		}
		if containerStats != nil {
			stats = append(stats, containerStats)
		}
	}

	return &runtimeapi.ListContainerStatsResponse{Stats: stats}, nil
}

func getCadvisorContainerInfo(ca cadvisor.Interface) (map[string]cadvisorapiv2.ContainerInfo, error) {
	infos, err := ca.ContainerInfoV2("/", cadvisorapiv2.RequestOptions{
		IdType:    cadvisorapiv2.TypeName,
		Count:     2, // 2 samples are needed to compute "instantaneous" CPU
		Recursive: true,
	})
	if err != nil {
		if _, ok := infos["/"]; ok {
			// If the failure is partial, log it and return a best-effort
			// response.
			fmt.Println("partial failure issuing cadvisor.ContainerInfoV2", err)
		} else {
			return nil, fmt.Errorf("failed to get root cgroup stats: %v", err)
		}
	}
	return infos, nil
}
