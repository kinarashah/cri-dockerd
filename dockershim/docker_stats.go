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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	cc "k8s.io/kubernetes/pkg/kubelet/cm"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/leaky"
	"k8s.io/utils/pointer"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	cadvisormetrics "github.com/google/cadvisor/container"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
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
		cc, err := initCadvisorClient(ds.cgroupDriver)
		if err != nil {
			return nil, err
		}
		ds.cadvisorClient1 = cc
		err = ds.cadvisorClient1.Start()
		if err != nil {
			fmt.Println("kinara: error starting cadvisorClient", err)
			return nil, err
		}

		hostssProvider := NewHostStatsProvider(kubecontainer.RealOS{}, func(podUID types.UID) (string, bool) {
			return getEtcHostsPath(getPodDir(podUID)), SupportsSingleFileMapping()
		})

		ds.hoststatProvider = hostssProvider
		fmt.Println("kinara: registered cadvisorClient1")
	}

	//_, err := ds.cadvisorClient1.DockerInfo()
	//if err != nil {
	//	fmt.Println("kinara: err", err)
	//}

	rootFsInfo, err := ds.cadvisorClient1.RootFsInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get rootFs info: %v", err)
	}

	imageFsInfo, err := ds.cadvisorClient1.ImagesFsInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get imageFsInfo info: %v", err)
	}

	infos, err := getCadvisorContainerInfo(ds.cadvisorClient1)
	if err != nil {
		return nil, fmt.Errorf("failed to get container info from cadvisor: %v", err)
	}

	filteredInfos, allInfos := filterTerminatedContainerInfoAndAssembleByPodCgroupKey(infos)
	podToStats := map[statsapi.PodReference]*statsapi.PodStats{}

	for key, cinfo := range filteredInfos {
		// On systemd using devicemapper each mount into the container has an
		// associated cgroup. We ignore them to ensure we do not get duplicate
		// entries in our summary. For details on .mount units:
		// http://man7.org/linux/man-pages/man5/systemd.mount.5.html
		if strings.HasSuffix(key, ".mount") {
			continue
		}
		// Build the Pod key if this container is managed by a Pod
		if !isPodManagedContainer(&cinfo) {
			continue
		}
		ref := buildPodRef(cinfo.Spec.Labels)

		// Lookup the PodStats for the pod using the PodRef. If none exists,
		// initialize a new entry.
		podStats, found := podToStats[ref]
		if !found {
			podStats = &statsapi.PodStats{PodRef: ref}
			podToStats[ref] = podStats
		}

		// Update the PodStats entry with the stats from the container by
		// adding it to podStats.Containers.
		containerName := kubetypes.GetContainerName(cinfo.Spec.Labels)
		if containerName == leaky.PodInfraContainerName {
			// Special case for infrastructure container which is hidden from
			// the user and has network stats.
			podStats.Network = cadvisorInfoToNetworkStats(&cinfo)
		} else {
			podStats.Containers = append(podStats.Containers, *cadvisorInfoToContainerStats(containerName, &cinfo, &rootFsInfo, &imageFsInfo))
		}
	}

	// Add each PodStats to the result.
	result := make([]statsapi.PodStats, 0, len(podToStats))
	for _, podStats := range podToStats {
		// Lookup the volume stats for each pod.
		podUID := types.UID(podStats.PodRef.UID)
		var ephemeralStats []statsapi.VolumeStats
		// see if this can be done in kubelet
		//if vstats, found := p.resourceAnalyzer.GetPodVolumeStats(podUID); found {
		//	ephemeralStats = make([]statsapi.VolumeStats, len(vstats.EphemeralVolumes))
		//	copy(ephemeralStats, vstats.EphemeralVolumes)
		//	podStats.VolumeStats = append(append([]statsapi.VolumeStats{}, vstats.EphemeralVolumes...), vstats.PersistentVolumes...)
		//}

		logStats, err := ds.hoststatProvider.getPodLogStats(podStats.PodRef.Namespace, podStats.PodRef.Name, podUID, &rootFsInfo)
		if err != nil {
			klog.ErrorS(err, "Unable to fetch pod log stats", "pod", klog.KRef(podStats.PodRef.Namespace, podStats.PodRef.Name))
		}
		etcHostsStats, err := ds.hoststatProvider.getPodEtcHostsStats(podUID, &rootFsInfo)
		if err != nil {
			klog.ErrorS(err, "Unable to fetch pod etc hosts stats", "pod", klog.KRef(podStats.PodRef.Namespace, podStats.PodRef.Name))
		}

		podStats.EphemeralStorage = calcEphemeralStorage(podStats.Containers, ephemeralStats, &rootFsInfo, logStats, etcHostsStats, false)
		// Lookup the pod-level cgroup's CPU and memory stats
		podInfo := getCadvisorPodInfoFromPodUID(podUID, allInfos)
		if podInfo != nil {
			cpu, memory := cadvisorInfoToCPUandMemoryStats(podInfo)
			podStats.CPU = cpu
			podStats.Memory = memory
			podStats.ProcessStats = cadvisorInfoToProcessStats(podInfo)
		}

		result = append(result, *podStats)
		// see if this can be done in kubelet
		//status, found := p.statusProvider.GetPodStatus(podUID)
		//if found && status.StartTime != nil && !status.StartTime.IsZero() {
		//	podStats.StartTime = *status.StartTime
		//	// only append stats if we were able to get the start time of the pod
		//	result = append(result, *podStats)
		//}
	}

	var stats []*runtimeapi.ContainerStats
	for _, res := range result {
		for _, cc := range res.Containers {
			fmt.Println(cc)
			stats = append(stats, &runtimeapi.ContainerStats{})
		}
	}

	//var stats1 []*runtimeapi.ContainerStats
	return &runtimeapi.ListContainerStatsResponse{Stats: stats}, nil

	//var stats []*runtimeapi.ContainerStats

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

func initCadvisorClient(cgroupDriver string) (*cadvisorClient, error) {
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
	cgroupRoot := cc.NodeAllocatableRoot("/", true, cgroupDriver)
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

	return &cadvisorClient{Manager: m, rootPath: "/var/lib/kubelet"}, nil
}

type containerInfoWithCgroup struct {
	cinfo  cadvisorapiv2.ContainerInfo
	cgroup string
}

type containerID struct {
	podRef        statsapi.PodReference
	containerName string
}

func filterTerminatedContainerInfoAndAssembleByPodCgroupKey(containerInfo map[string]cadvisorapiv2.ContainerInfo) (map[string]cadvisorapiv2.ContainerInfo, map[string]cadvisorapiv2.ContainerInfo) {
	cinfoMap := make(map[containerID][]containerInfoWithCgroup)
	cinfosByPodCgroupKey := make(map[string]cadvisorapiv2.ContainerInfo)
	for key, cinfo := range containerInfo {
		var podCgroupKey string
		if IsSystemdStyleName(key) {
			// Convert to internal cgroup name and take the last component only.
			internalCgroupName := ParseSystemdToCgroupName(key)
			podCgroupKey = internalCgroupName[len(internalCgroupName)-1]
		} else {
			// Take last component only.
			podCgroupKey = path.Base(key)
		}
		cinfosByPodCgroupKey[podCgroupKey] = cinfo
		if !isPodManagedContainer(&cinfo) {
			continue
		}
		cinfoID := containerID{
			podRef:        buildPodRef(cinfo.Spec.Labels),
			containerName: kubetypes.GetContainerName(cinfo.Spec.Labels),
		}
		cinfoMap[cinfoID] = append(cinfoMap[cinfoID], containerInfoWithCgroup{
			cinfo:  cinfo,
			cgroup: key,
		})
	}
	result := make(map[string]cadvisorapiv2.ContainerInfo)
	for _, refs := range cinfoMap {
		if len(refs) == 1 {
			// ContainerInfo with no CPU/memory/network usage for uncleaned cgroups of
			// already terminated containers, which should not be shown in the results.
			if !isContainerTerminated(&refs[0].cinfo) {
				result[refs[0].cgroup] = refs[0].cinfo
			}
			continue
		}
		sort.Sort(ByCreationTime(refs))
		for i := len(refs) - 1; i >= 0; i-- {
			if hasMemoryAndCPUInstUsage(&refs[i].cinfo) {
				result[refs[i].cgroup] = refs[i].cinfo
				break
			}
		}
	}
	return result, cinfosByPodCgroupKey
}

func IsSystemdStyleName(name string) bool {
	return strings.HasSuffix(name, systemdSuffix)
}

func ParseSystemdToCgroupName(name string) CgroupName {
	driverName := path.Base(name)
	driverName = strings.TrimSuffix(driverName, systemdSuffix)
	parts := strings.Split(driverName, "-")
	result := []string{}
	for _, part := range parts {
		result = append(result, unescapeSystemdCgroupName(part))
	}
	return CgroupName(result)
}

type CgroupName []string

func (cgroupName CgroupName) ToCgroupfs() string {
	return "/" + path.Join(cgroupName...)
}

func unescapeSystemdCgroupName(part string) string {
	return strings.Replace(part, "_", "-", -1)
}

func isPodManagedContainer(cinfo *cadvisorapiv2.ContainerInfo) bool {
	podName := kubetypes.GetPodName(cinfo.Spec.Labels)
	podNamespace := kubetypes.GetPodNamespace(cinfo.Spec.Labels)
	managed := podName != "" && podNamespace != ""
	if !managed && podName != podNamespace {
		klog.InfoS(
			"Expect container to have either both podName and podNamespace labels, or neither",
			"podNameLabel", podName, "podNamespaceLabel", podNamespace)
	}
	return managed
}

// buildPodRef returns a PodReference that identifies the Pod managing cinfo
func buildPodRef(containerLabels map[string]string) statsapi.PodReference {
	podName := kubetypes.GetPodName(containerLabels)
	podNamespace := kubetypes.GetPodNamespace(containerLabels)
	podUID := kubetypes.GetPodUID(containerLabels)
	return statsapi.PodReference{Name: podName, Namespace: podNamespace, UID: podUID}
}

func isContainerTerminated(info *cadvisorapiv2.ContainerInfo) bool {
	if !info.Spec.HasCpu && !info.Spec.HasMemory && !info.Spec.HasNetwork {
		return true
	}
	cstat, found := latestContainerStats(info)
	if !found {
		return true
	}
	if cstat.Network != nil {
		iStats := cadvisorInfoToNetworkStats(info)
		if iStats != nil {
			for _, iStat := range iStats.Interfaces {
				if *iStat.RxErrors != 0 || *iStat.TxErrors != 0 || *iStat.RxBytes != 0 || *iStat.TxBytes != 0 {
					return false
				}
			}
		}
	}
	if cstat.CpuInst == nil || cstat.Memory == nil {
		return true
	}
	return cstat.CpuInst.Usage.Total == 0 && cstat.Memory.RSS == 0
}

type ByCreationTime []containerInfoWithCgroup

func (a ByCreationTime) Len() int      { return len(a) }
func (a ByCreationTime) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ByCreationTime) Less(i, j int) bool {
	if a[i].cinfo.Spec.CreationTime.Equal(a[j].cinfo.Spec.CreationTime) {
		// There shouldn't be two containers with the same name and/or the same
		// creation time. However, to make the logic here robust, we break the
		// tie by moving the one without CPU instantaneous or memory RSS usage
		// to the beginning.
		return hasMemoryAndCPUInstUsage(&a[j].cinfo)
	}
	return a[i].cinfo.Spec.CreationTime.Before(a[j].cinfo.Spec.CreationTime)
}

func hasMemoryAndCPUInstUsage(info *cadvisorapiv2.ContainerInfo) bool {
	if !info.Spec.HasCpu || !info.Spec.HasMemory {
		return false
	}
	cstat, found := latestContainerStats(info)
	if !found {
		return false
	}
	if cstat.CpuInst == nil {
		return false
	}
	return cstat.CpuInst.Usage.Total != 0 && cstat.Memory.RSS != 0
}

// getCadvisorPodInfoFromPodUID returns a pod cgroup information by matching the podUID with its CgroupName identifier base name
func getCadvisorPodInfoFromPodUID(podUID types.UID, infos map[string]cadvisorapiv2.ContainerInfo) *cadvisorapiv2.ContainerInfo {
	if info, found := infos[GetPodCgroupNameSuffix(podUID)]; found {
		return &info
	}
	return nil
}
