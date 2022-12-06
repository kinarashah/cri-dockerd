/*
Copyright 2021 Mirantis

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

package core

import (
	"context"
	"github.com/sirupsen/logrus"
	"time"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

func (ds *dockerService) refreshStats() {
	for {
		select {
		case <-ds.shutdown:
			return
		case <-time.After(ds.refreshPeriod):
		}
		ds.refresh()
	}
}

func (ds *dockerService) refresh() {
	filter := &runtimeapi.ContainerFilter{}
	listResp, err := ds.ListContainers(context.TODO(), &runtimeapi.ListContainersRequest{Filter: filter})
	if err != nil {
		logrus.Errorf("Error listing containers with filter: %+v", filter)
		logrus.Errorf("Error listing containers error: %s", err)
	}
	start := time.Now()
	logrus.Infof("refresh() start %v", start.String())
	stats1 := make([]*runtimeapi.ContainerStats, 0, len(listResp.Containers))
	for _, container := range listResp.Containers {
		if stats, err := ds.getContainerStats(container.Id); err == nil && stats != nil {
			stats1 = append(stats1, stats)
		} else {
			logrus.Error(err, " Failed to get stats from container "+container.Id)
		}
	}
	ds.mutex.Lock()
	ds.stats = stats1
	ds.mutex.Unlock()
	logrus.Infof("refresh() end %v", time.Since(start).String())
}

// ContainerStats returns stats for a container stats request based on container id.
func (ds *dockerService) ContainerStats(
	_ context.Context,
	r *runtimeapi.ContainerStatsRequest,
) (*runtimeapi.ContainerStatsResponse, error) {
	logrus.Info("ContainerStats")
	stats, err := ds.getContainerStats(r.ContainerId)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.ContainerStatsResponse{Stats: stats}, nil
}

// ListContainerStats returns stats for a list container stats request based on a filter.
func (ds *dockerService) ListContainerStats(
	ctx context.Context,
	r *runtimeapi.ListContainerStatsRequest,
) (*runtimeapi.ListContainerStatsResponse, error) {
	containerStatsFilter := r.GetFilter()
	filter := &runtimeapi.ContainerFilter{}

	if containerStatsFilter != nil {
		filter.Id = containerStatsFilter.Id
		filter.PodSandboxId = containerStatsFilter.PodSandboxId
		filter.LabelSelector = containerStatsFilter.LabelSelector
	}

	logrus.Infof("rancher: ListContainerStats %v", filter)

	start := time.Now()
	ds.mutex.Lock()
	defer ds.mutex.Unlock()
	logrus.Infof("ListContainerStats start() %v", start.String())

	stats := ds.stats

	logrus.Infof("ListContainerStats end() %v", time.Since(start).String())
	return &runtimeapi.ListContainerStatsResponse{Stats: stats}, nil
}
