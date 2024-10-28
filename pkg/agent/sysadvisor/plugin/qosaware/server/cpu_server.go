/*
Copyright 2022 The Katalyst Authors.

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

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"github.com/kubewharf/katalyst-api/pkg/consts"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/advisorsvc"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/commonstate"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/cpuadvisor"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/metacache"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin/qosaware/reporter"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/types"
	"github.com/kubewharf/katalyst-core/pkg/config"
	"github.com/kubewharf/katalyst-core/pkg/metaserver"
	"github.com/kubewharf/katalyst-core/pkg/metrics"
	"github.com/kubewharf/katalyst-core/pkg/util/general"
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
)

const (
	cpuServerName string = "cpu-server"

	cpuServerLWHealthCheckName = "cpu-server-lw"
)

type cpuServer struct {
	*baseServer
	startTime               time.Time
	hasListAndWatchLoop     atomic.Value
	headroomResourceManager reporter.HeadroomResourceManager
}

func NewCPUServer(
	conf *config.Configuration,
	headroomResourceManager reporter.HeadroomResourceManager,
	metaCache metacache.MetaCache,
	metaServer *metaserver.MetaServer,
	advisor subResourceAdvisor,
	emitter metrics.MetricEmitter,
) (*cpuServer, error) {
	cs := &cpuServer{}
	cs.baseServer = newBaseServer(cpuServerName, conf, metaCache, metaServer, emitter, advisor, cs)
	cs.hasListAndWatchLoop.Store(false)
	cs.startTime = time.Now()
	cs.advisorSocketPath = conf.CPUAdvisorSocketAbsPath
	cs.pluginSocketPath = conf.CPUPluginSocketAbsPath
	cs.headroomResourceManager = headroomResourceManager
	cs.resourceRequestName = "CPURequest"
	return cs, nil
}

func (cs *cpuServer) createQRMClient() (cpuadvisor.CPUPluginClient, io.Closer, error) {
	if !general.IsPathExists(cs.pluginSocketPath) {
		return nil, nil, fmt.Errorf("memory plugin socket path %s does not exist", cs.pluginSocketPath)
	}
	conn, err := cs.dial(cs.pluginSocketPath, cs.period)
	if err != nil {
		return nil, nil, fmt.Errorf("dial memory plugin socket failed: %w", err)
	}
	return cpuadvisor.NewCPUPluginClient(conn), conn, nil
}

func (cs *cpuServer) RegisterAdvisorServer() {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()

	grpcServer := grpc.NewServer()
	cpuadvisor.RegisterCPUAdvisorServer(grpcServer, cs)
	cs.grpcServer = grpcServer
}

func (cs *cpuServer) ListAndWatch(_ *advisorsvc.Empty, server cpuadvisor.CPUAdvisor_ListAndWatchServer) error {
	_ = cs.emitter.StoreInt64(cs.genMetricsName(metricServerLWCalled), int64(cs.period.Seconds()), metrics.MetricTypeNameCount)

	if cs.hasListAndWatchLoop.Swap(true).(bool) {
		klog.Warningf("[qosaware-server-cpu] another ListAndWatch loop is running")
		return fmt.Errorf("another ListAndWatch loop is running")
	}
	defer cs.hasListAndWatchLoop.Store(false)

	cpuPluginClient, conn, err := cs.createQRMClient()
	if err != nil {
		_ = cs.emitter.StoreInt64(cs.genMetricsName(metricServerLWGetCheckpointFailed), int64(cs.period.Seconds()), metrics.MetricTypeNameCount)
		klog.Errorf("[qosaware-server-cpu] create cpu plugin client failed: %v", err)
		return fmt.Errorf("create cpu plugin client failed: %w", err)
	}
	defer conn.Close()

	klog.Infof("[qosaware-server-cpu] start to push cpu advices")
	general.RegisterTemporaryHeartbeatCheck(cpuServerLWHealthCheckName, healthCheckTolerationDuration, general.HealthzCheckStateNotReady, healthCheckTolerationDuration)
	defer general.UnregisterTemporaryHeartbeatCheck(cpuServerLWHealthCheckName)

	timer := time.NewTimer(cs.period)
	defer func() {
		if !timer.Stop() {
			<-timer.C
		}
	}()

	for {
		select {
		case <-server.Context().Done():
			klog.Infof("[qosaware-server-cpu] lw stream server exited")
			return nil
		case <-cs.stopCh:
			klog.Infof("[qosaware-server-cpu] lw stopped because cpu server stopped")
			return nil
		case <-timer.C:
			klog.Infof("[qosaware-server-cpu] trigger advisor update")
			if err := cs.getAndPushAdvice(cpuPluginClient, server); err != nil {
				klog.Errorf("[qosaware-server-cpu] get and push advice failed: %v", err)
				_ = general.UpdateHealthzStateByError(cpuServerLWHealthCheckName, err)
			} else {
				_ = general.UpdateHealthzStateByError(cpuServerLWHealthCheckName, nil)
			}
			timer.Reset(cs.period)
		}
	}
}

func (cs *cpuServer) getAndSyncCheckpoint(ctx context.Context, client cpuadvisor.CPUPluginClient) error {
	safeTime := time.Now().UnixNano()

	// get checkpoint
	getCheckpointResp, err := client.GetCheckpoint(ctx, &cpuadvisor.GetCheckpointRequest{})
	if err != nil {
		_ = cs.emitter.StoreInt64(cs.genMetricsName(metricServerLWGetCheckpointFailed), int64(cs.period.Seconds()), metrics.MetricTypeNameCount)
		return fmt.Errorf("get checkpoint failed: %w", err)
	} else if getCheckpointResp == nil {
		_ = cs.emitter.StoreInt64(cs.genMetricsName(metricServerLWGetCheckpointFailed), int64(cs.period.Seconds()), metrics.MetricTypeNameCount)
		return fmt.Errorf("got nil checkpoint")
	}
	klog.Infof("[qosaware-server-cpu] got checkpoint: %v", general.ToString(getCheckpointResp.Entries))
	_ = cs.emitter.StoreInt64(cs.genMetricsName(metricServerLWGetCheckpointSucceeded), int64(cs.period.Seconds()), metrics.MetricTypeNameCount)

	cs.syncCheckpoint(ctx, getCheckpointResp, safeTime)
	return nil
}

func (cs *cpuServer) shouldTriggerAdvisorUpdate() bool {
	// TODO: do we still need this check?
	// skip pushing advice during startup
	if time.Now().Before(cs.startTime.Add(types.StartUpPeriod)) {
		klog.Infof("[qosaware-cpu] skip pushing advice: starting up")
		return false
	}

	// sanity check: if reserve pool exists
	reservePoolInfo, ok := cs.metaCache.GetPoolInfo(commonstate.PoolNameReserve)
	if !ok || reservePoolInfo == nil {
		klog.Errorf("[qosaware-cpu] skip pushing advice: reserve pool does not exist")
		return false
	}

	return true
}

// getAndPushAdvice implements the legacy asynchronous bidirectional communication model between
// qrm plugins and sys-advisor. This is kept for backward compatibility.
// TODO: remove this function after all qrm plugins are migrated to the new synchronous model
func (cs *cpuServer) getAndPushAdvice(client cpuadvisor.CPUPluginClient, server cpuadvisor.CPUAdvisor_ListAndWatchServer) error {
	if err := cs.getAndSyncCheckpoint(server.Context(), client); err != nil {
		return err
	}

	if !cs.shouldTriggerAdvisorUpdate() {
		return nil
	}

	// trigger advisor update and get latest advice
	advisorRespRaw, err := cs.resourceAdvisor.UpdateAndGetAdvice()
	if err != nil {
		_ = cs.emitter.StoreInt64(cs.genMetricsName(metricServerAdvisorUpdateFailed), int64(cs.period.Seconds()), metrics.MetricTypeNameCount)
		return fmt.Errorf("get advice failed: %w", err)
	}
	advisorResp, ok := advisorRespRaw.(*types.InternalCPUCalculationResult)
	if !ok {
		_ = cs.emitter.StoreInt64(cs.genMetricsName(metricServerAdvisorUpdateFailed), int64(cs.period.Seconds()), metrics.MetricTypeNameCount)
		return fmt.Errorf("get advice failed: invalid type: %T", advisorRespRaw)
	}

	klog.Infof("[qosaware-server-cpu] get advisor update: %+v", general.ToString(advisorResp))

	lwResp := cs.assembleResponse(advisorResp)
	if err := server.Send(lwResp); err != nil {
		_ = cs.emitter.StoreInt64(cs.genMetricsName(metricServerLWSendResponseFailed), int64(cs.period.Seconds()), metrics.MetricTypeNameCount)
		return fmt.Errorf("send listWatch response failed: %w", err)
	}
	klog.Infof("[qosaware-server-cpu] sent listWatch resp: %v", general.ToString(lwResp))
	_ = cs.emitter.StoreInt64(cs.genMetricsName(metricServerLWSendResponseSucceeded), int64(cs.period.Seconds()), metrics.MetricTypeNameCount)
	return nil
}

func (cs *cpuServer) assembleResponse(advisorResp *types.InternalCPUCalculationResult) *cpuadvisor.ListAndWatchResponse {
	calculationEntriesMap := make(map[string]*cpuadvisor.CalculationEntries)
	blockID2Blocks := NewBlockSet()

	cs.assemblePoolEntries(advisorResp, calculationEntriesMap, blockID2Blocks)

	// Assemble pod entries
	f := func(podUID string, containerName string, ci *types.ContainerInfo) bool {
		if err := cs.assemblePodEntries(calculationEntriesMap, blockID2Blocks, podUID, ci); err != nil {
			klog.Errorf("[qosaware-server-cpu] assemblePodEntries for pod %s/%s uid %s err: %v", ci.PodNamespace, ci.PodName, ci.PodUID, err)
		}
		return true
	}
	cs.metaCache.RangeContainer(f)

	// Send result
	resp := &cpuadvisor.ListAndWatchResponse{
		Entries:                               calculationEntriesMap,
		ExtraEntries:                          make([]*advisorsvc.CalculationInfo, 0),
		AllowSharedCoresOverlapReclaimedCores: advisorResp.AllowSharedCoresOverlapReclaimedCores,
	}

	for _, retEntry := range advisorResp.ExtraEntries {
		found := false
		for _, respEntry := range resp.ExtraEntries {
			if retEntry.CgroupPath == respEntry.CgroupPath {
				found = true
				for k, v := range retEntry.Values {
					respEntry.CalculationResult.Values[k] = v
				}
				break
			}
		}
		if !found {
			calculationInfo := &advisorsvc.CalculationInfo{
				CgroupPath: retEntry.CgroupPath,
				CalculationResult: &advisorsvc.CalculationResult{
					Values: general.DeepCopyMap(retEntry.Values),
				},
			}
			resp.ExtraEntries = append(resp.ExtraEntries, calculationInfo)
		}
	}

	extraNumaHeadRoom := cs.assembleHeadroom()
	if extraNumaHeadRoom != nil {
		resp.ExtraEntries = append(resp.ExtraEntries, extraNumaHeadRoom)
	}

	return resp
}

// assmble per-numa headroom
func (cs *cpuServer) assembleHeadroom() *advisorsvc.CalculationInfo {
	numaAllocatable, err := cs.headroomResourceManager.GetNumaAllocatable()
	if err != nil {
		klog.Errorf("get numa allocatable failed: %v", err)
		return nil
	}

	numaHeadroom := make(map[int]float64)
	for numaID, res := range numaAllocatable {
		numaHeadroom[numaID] = float64(res.Value()) / 1000.0
	}
	data, err := json.Marshal(numaHeadroom)
	if err != nil {
		klog.Errorf("marshal headroom failed: %v", err)
		return nil
	}

	calculationResult := &advisorsvc.CalculationResult{
		Values: map[string]string{
			string(cpuadvisor.ControlKnobKeyCPUNUMAHeadroom): string(data),
		},
	}

	return &advisorsvc.CalculationInfo{
		CgroupPath:        "",
		CalculationResult: calculationResult,
	}
}

func (cs *cpuServer) syncCheckpoint(ctx context.Context, resp *cpuadvisor.GetCheckpointResponse, safeTime int64) {
	livingPoolNameSet := sets.NewString()

	// parse pool entries first, which are needed for parsing container entries
	for entryName, entry := range resp.Entries {
		if poolInfo, ok := entry.Entries[commonstate.FakedContainerName]; ok {
			poolName := entryName
			livingPoolNameSet.Insert(poolName)
			if err := cs.updatePoolInfo(poolName, poolInfo); err != nil {
				klog.Errorf("[qosaware-server-cpu] update pool info with error: %v", err)
			}
		}
	}

	// parse container entries after pool entries
	for entryName, entry := range resp.Entries {
		if _, ok := entry.Entries[commonstate.FakedContainerName]; !ok {
			podUID := entryName
			pod, err := cs.metaServer.GetPod(ctx, podUID)
			if err != nil {
				klog.Errorf("[qosaware-server-cpu] get pod info with error: %v", err)
				continue
			}

			for containerName, info := range entry.Entries {
				if err := cs.updateContainerInfo(podUID, containerName, pod, info); err != nil {
					klog.Errorf("[qosaware-server-cpu] update container info with error: %v", err)
					_ = cs.emitter.StoreInt64(cs.genMetricsName(metricServerCheckpointUpdateContainerFailed), 1, metrics.MetricTypeNameCount,
						metrics.MetricTag{Key: "podUID", Val: podUID},
						metrics.MetricTag{Key: "containerName", Val: containerName})
				}
			}
		}
	}

	// clean up the containers not existed in resp.Entries
	_ = cs.metaCache.RangeAndDeleteContainer(func(containerInfo *types.ContainerInfo) bool {
		info, ok := resp.Entries[containerInfo.PodUID]
		if !ok {
			return true
		}
		if _, ok = info.Entries[containerInfo.ContainerName]; !ok {
			return true
		}
		return false
	}, safeTime)

	// complement living containers' original owner pools for pool gc
	// todo: deprecate original owner pool and generate owner pool by realtime container status
	cs.metaCache.RangeContainer(func(podUID string, containerName string, containerInfo *types.ContainerInfo) bool {
		livingPoolNameSet.Insert(containerInfo.OriginOwnerPoolName)
		return true
	})

	// gc pool entries
	_ = cs.metaCache.GCPoolEntries(livingPoolNameSet)
}

func (cs *cpuServer) updatePoolInfo(poolName string, info *cpuadvisor.AllocationInfo) error {
	pi, ok := cs.metaCache.GetPoolInfo(poolName)
	if !ok {
		pi = &types.PoolInfo{
			PoolName: info.OwnerPoolName,
		}
	}
	pi.TopologyAwareAssignments = machine.TransformCPUAssignmentFormat(info.TopologyAwareAssignments)
	pi.OriginalTopologyAwareAssignments = machine.TransformCPUAssignmentFormat(info.OriginalTopologyAwareAssignments)

	return cs.metaCache.SetPoolInfo(poolName, pi)
}

func (cs *cpuServer) updateContainerInfo(podUID string, containerName string, pod *v1.Pod, info *cpuadvisor.AllocationInfo) error {
	ci, ok := cs.metaCache.GetContainerInfo(podUID, containerName)
	if !ok {
		return fmt.Errorf("container %v/%v not exist", podUID, containerName)
	}

	ci.RampUp = info.RampUp
	ci.TopologyAwareAssignments = machine.TransformCPUAssignmentFormat(info.TopologyAwareAssignments)
	ci.OriginalTopologyAwareAssignments = machine.TransformCPUAssignmentFormat(info.OriginalTopologyAwareAssignments)
	ci.OwnerPoolName = info.OwnerPoolName

	// get qos level name according to the qos conf
	qosLevel, err := cs.qosConf.GetQoSLevelForPod(pod)
	if err != nil {
		return fmt.Errorf("container %v/%v get qos level failed", podUID, containerName)
	}
	if ci.QoSLevel != qosLevel {
		general.Infof("qos level has change from %s to %s", ci.QoSLevel, qosLevel)
		ci.QoSLevel = qosLevel
	}

	// TODO: can we do without sysadvisor persisted states?
	if ci.OriginOwnerPoolName == "" {
		ci.OriginOwnerPoolName = ci.OwnerPoolName
	}

	// fill in topology aware assignment for containers with owner pool
	if ci.QoSLevel != consts.PodAnnotationQoSLevelDedicatedCores {
		if len(ci.OwnerPoolName) > 0 {
			if poolInfo, ok := cs.metaCache.GetPoolInfo(ci.OwnerPoolName); ok {
				ci.TopologyAwareAssignments = poolInfo.TopologyAwareAssignments.Clone()
			}
		}
	}

	// Need to set back because of deep copy
	return cs.metaCache.SetContainerInfo(podUID, containerName, ci)
}

// assemblePoolEntries fills up calculationEntriesMap and blockSet based on cpu.InternalCPUCalculationResult
// - for each [pool, numa] set, there exists a new Block (and corresponding internalBlock)
func (cs *cpuServer) assemblePoolEntries(advisorResp *types.InternalCPUCalculationResult, calculationEntriesMap map[string]*cpuadvisor.CalculationEntries, bs blockSet) {
	for poolName, entries := range advisorResp.PoolEntries {
		// join reclaim pool lastly
		if poolName == commonstate.PoolNameReclaim && advisorResp.AllowSharedCoresOverlapReclaimedCores {
			continue
		}
		poolEntry := NewPoolCalculationEntries(poolName)
		for numaID, size := range entries {
			block := NewBlock(uint64(size), "")
			numaCalculationResult := &cpuadvisor.NumaCalculationResult{Blocks: []*cpuadvisor.Block{block}}

			innerBlock := NewInnerBlock(block, int64(numaID), poolName, nil, numaCalculationResult)
			innerBlock.join(block.BlockId, bs)

			poolEntry.Entries[commonstate.FakedContainerName].CalculationResultsByNumas[int64(numaID)] = numaCalculationResult
		}
		calculationEntriesMap[poolName] = poolEntry
	}

	if reclaimEntries, ok := advisorResp.PoolEntries[commonstate.PoolNameReclaim]; ok && advisorResp.AllowSharedCoresOverlapReclaimedCores {
		poolEntry := NewPoolCalculationEntries(commonstate.PoolNameReclaim)
		for numaID, reclaimSize := range reclaimEntries {

			overlapSize := advisorResp.GetPoolOverlapInfo(commonstate.PoolNameReclaim, numaID)
			if len(overlapSize) == 0 {
				// If share pool not exists，join reclaim pool directly
				block := NewBlock(uint64(reclaimSize), "")
				numaCalculationResult := &cpuadvisor.NumaCalculationResult{Blocks: []*cpuadvisor.Block{block}}

				innerBlock := NewInnerBlock(block, int64(numaID), commonstate.PoolNameReclaim, nil, numaCalculationResult)
				innerBlock.join(block.BlockId, bs)

				poolEntry.Entries[commonstate.FakedContainerName].CalculationResultsByNumas[int64(numaID)] = numaCalculationResult
			} else {
				numaCalculationResult := &cpuadvisor.NumaCalculationResult{Blocks: []*cpuadvisor.Block{}}
				for sharedPoolName, reclaimedSize := range overlapSize {
					block := NewBlock(uint64(reclaimedSize), "")

					sharedPoolCalculationResults, ok := getNumaCalculationResult(calculationEntriesMap, sharedPoolName, commonstate.FakedContainerName, int64(numaID))
					if ok && len(sharedPoolCalculationResults.Blocks) == 1 {
						innerBlock := NewInnerBlock(block, int64(numaID), commonstate.PoolNameReclaim, nil, numaCalculationResult)
						numaCalculationResult.Blocks = append(numaCalculationResult.Blocks, block)
						innerBlock.join(sharedPoolCalculationResults.Blocks[0].BlockId, bs)
					}
					poolEntry.Entries[commonstate.FakedContainerName].CalculationResultsByNumas[int64(numaID)] = numaCalculationResult
				}
			}
		}
		calculationEntriesMap[commonstate.PoolNameReclaim] = poolEntry
	}
}

// assemblePoolEntries fills up calculationEntriesMap and blockSet based on types.ContainerInfo
//
// todo this logic should be refined to make sure we will assemble entries from	internalCalculationInfo rather than walking through containerInfo
func (cs *cpuServer) assemblePodEntries(calculationEntriesMap map[string]*cpuadvisor.CalculationEntries,
	bs blockSet, podUID string, ci *types.ContainerInfo,
) error {
	calculationInfo := &cpuadvisor.CalculationInfo{
		OwnerPoolName:             ci.OwnerPoolName,
		CalculationResultsByNumas: nil,
	}

	// if isolation is locking in, pass isolation-region name (equals isolation owner-pool) instead of owner pool
	if ci.Isolated {
		if ci.RegionNames.Len() == 1 && ci.OwnerPoolName != ci.RegionNames.List()[0] {
			calculationInfo.OwnerPoolName = ci.RegionNames.List()[0]
		}
	}
	// if isolation is locking out, pass original owner pool instead of owner pool
	if !ci.Isolated && ci.OwnerPoolName != ci.OriginOwnerPoolName {
		calculationInfo.OwnerPoolName = ci.OriginOwnerPoolName
	}

	if ci.QoSLevel == consts.PodAnnotationQoSLevelSharedCores || ci.QoSLevel == consts.PodAnnotationQoSLevelReclaimedCores {
		if calculationInfo.OwnerPoolName == "" {
			klog.Warningf("container %s/%s pool name is empty", ci.PodUID, ci.ContainerName)
			return nil
		}
		if _, ok := calculationEntriesMap[calculationInfo.OwnerPoolName]; !ok {
			klog.Warningf("container %s/%s refer a non-existed pool: %s", ci.PodUID, ci.ContainerName, ci.OwnerPoolName)
			return nil
		}
	}

	// currently, only pods in "dedicated_nums with numa binding" has topology aware allocations
	if ci.IsDedicatedNumaBinding() {
		calculationResultsByNumas := make(map[int64]*cpuadvisor.NumaCalculationResult)

		for numaID, cpuset := range ci.TopologyAwareAssignments {
			numaCalculationResult := &cpuadvisor.NumaCalculationResult{Blocks: []*cpuadvisor.Block{}}

			// the same podUID appears twice iff there exists multiple containers in one pod;
			// in this case, reuse the same blocks as the last container.
			// i.e. sidecar container will always follow up with the main container.
			if podEntries, ok := calculationEntriesMap[podUID]; ok {
				for _, containerEntry := range podEntries.Entries {
					if result, ok := containerEntry.CalculationResultsByNumas[int64(numaID)]; ok {
						for _, block := range result.Blocks {
							newBlock := NewBlock(block.Result, block.BlockId)
							newInnerBlock := NewInnerBlock(newBlock, int64(numaID), "", ci, numaCalculationResult)
							numaCalculationResult.Blocks = append(numaCalculationResult.Blocks, newBlock)
							newInnerBlock.join(block.BlockId, bs)
						}
						break
					}
				}
			} else {
				// if this podUID appears firstly, we should generate a new Block

				reclaimPoolCalculationResults, ok := getNumaCalculationResult(calculationEntriesMap, commonstate.PoolNameReclaim,
					commonstate.FakedContainerName, int64(numaID))
				if !ok {
					// if no reclaimed pool exists, return the generated Block

					block := NewBlock(uint64(cpuset.Size()), "")
					innerBlock := NewInnerBlock(block, int64(numaID), "", ci, numaCalculationResult)
					numaCalculationResult.Blocks = append(numaCalculationResult.Blocks, block)
					innerBlock.join(block.BlockId, bs)
				} else {
					// if reclaimed pool exists, join the generated Block with Block in reclaimed pool

					for _, block := range reclaimPoolCalculationResults.Blocks {
						// todo assume only one reclaimed block exists in a certain numa
						if block.OverlapTargets == nil || len(block.OverlapTargets) == 0 {
							newBlock := NewBlock(uint64(cpuset.Size()), "")
							innerBlock := NewInnerBlock(newBlock, int64(numaID), "", ci, numaCalculationResult)
							numaCalculationResult.Blocks = append(numaCalculationResult.Blocks, newBlock)
							innerBlock.join(block.BlockId, bs)
						}
					}
				}
			}

			calculationResultsByNumas[int64(numaID)] = numaCalculationResult
		}

		calculationInfo.CalculationResultsByNumas = calculationResultsByNumas
	}

	calculationEntries, ok := calculationEntriesMap[podUID]
	if !ok {
		calculationEntriesMap[podUID] = &cpuadvisor.CalculationEntries{
			Entries: make(map[string]*cpuadvisor.CalculationInfo),
		}
		calculationEntries = calculationEntriesMap[podUID]
	}
	calculationEntries.Entries[ci.ContainerName] = calculationInfo

	return nil
}
