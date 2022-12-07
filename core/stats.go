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
	"sync"
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
	logrus.Infof("dockerService: refresh() start: %v", start.String())
	newStats := make(map[string]*runtimeapi.ContainerStats)
	var mtx sync.Mutex
	var wg sync.WaitGroup
	for _, container := range listResp.Containers {
		container := container
		wg.Add(1)
		go func() {
			defer wg.Done()
			if containerStats, err := ds.getContainerStats(container.Id); err == nil && containerStats != nil {
				mtx.Lock()
				newStats[container.Id] = containerStats
				mtx.Unlock()
			} else if err != nil {
				logrus.Error(err, " Failed to get stats from container "+container.Id)
			}
		}()
	}
	wg.Wait()

	ds.mutex.Lock()
	ds.containerStats = newStats
	ds.mutex.Unlock()
	logrus.Infof("dockerService: refresh() end: %v", time.Since(start).Seconds())
}

// ContainerStats returns stats for a container stats request based on container id.
func (ds *dockerService) ContainerStats(
	_ context.Context,
	r *runtimeapi.ContainerStatsRequest,
) (*runtimeapi.ContainerStatsResponse, error) {
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

	listResp, err := ds.ListContainers(ctx, &runtimeapi.ListContainersRequest{Filter: filter})
	if err != nil {
		logrus.Errorf("Error listing containers with filter: %+v", filter)
		logrus.Errorf("Error listing containers error: %s", err)
		return nil, err
	}

	start := time.Now()
	logrus.Infof("listContainerStats start() %v", start.String())
	var stats = make([]*runtimeapi.ContainerStats, 0, len(listResp.Containers))
	ds.mutex.Lock()
	defer ds.mutex.Unlock()

	for _, container := range listResp.Containers {
		if st, ok := ds.containerStats[container.Id]; ok {
			stats = append(stats, st)
		} else {
			logrus.Errorf("containerStats: missing stats for %v", container.Id)
		}
	}
	logrus.Infof("listContainerStats end() %v", time.Since(start).Seconds())
	return &runtimeapi.ListContainerStatsResponse{Stats: stats}, nil
}
