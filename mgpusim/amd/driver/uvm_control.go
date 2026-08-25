package driver

// sbin_codex: UVM control plane on the driver side.
//
// Every mapping transition is region-scoped. UVM never reuses the GPU-wide
// ShootDownCommand / GPURestartReq sequence, never flushes a whole cache or
// TLB, and never stops a CU pipeline (spec 2.1, 19, 21). The transitions are
//
//	INVALID -> GPU_LOCAL : H2D, no cache op, no TLB op, replay
//	REMOTE  -> GPU_LOCAL : H2D, no cache op, 64KB TLB invalidate, replay
//	GPU_LOCAL -> REMOTE  : 64KB cache WB+INV, 64KB TLB invalidate, D2H
//
// In ideal mode the same messages are exchanged; only the interconnect that
// carries them is zero-latency.

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// uvmControlPort returns the GPU-side UVM control endpoint of one device.
func (m *UVMManager) uvmControlPort(deviceID uint64) sim.RemotePort {
	index := int(deviceID) - 1
	if index < 0 || index >= len(m.d.uvmGPUPorts) {
		return ""
	}

	return m.d.uvmGPUPorts[index]
}

// enqueueControlLocked queues one control message for the UVM port.
func (m *UVMManager) enqueueControlLocked(msg sim.Msg) {
	m.sendQueue = append(m.sendQueue, msg)
	m.d.TickLater()
}

// sendPendingControl drains one queued control message per tick.
func (m *UVMManager) sendPendingControl() bool {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	if len(m.sendQueue) == 0 || m.d.uvmPort == nil {
		return false
	}

	msg := m.sendQueue[0]
	if err := m.d.uvmPort.Send(msg); err != nil {
		return false
	}

	m.sendQueue = m.sendQueue[1:]

	return true
}

// requestTLBInvalidateLocked issues a 64KB range invalidation and resumes the
// caller once every TLB level has acknowledged it.
func (m *UVMManager) requestTLBInvalidateLocked(
	key RegionKey,
	done func(),
) {
	dst := m.uvmControlPort(key.DeviceID)
	if dst == "" {
		done()
		return
	}

	req := vm.NewUVMTLBInvalidateReq(m.d.uvmPort.AsRemote(), dst)
	req.PID = key.PID
	req.StartVA = key.Base
	req.Size = m.config.RegionSize
	req.DeviceID = key.DeviceID

	m.controlOps[req.ID] = &pendingControlOp{Kind: opTLBInvalidate, Done: done}
	m.stats.TLBRangeInvalidations++
	m.enqueueControlLocked(req)
}

// requestCacheRangeFlushLocked issues the mandatory 64KB writeback+invalidate
// that must precede a GPU_LOCAL -> REMOTE/INVALID transition.
func (m *UVMManager) requestCacheRangeFlushLocked(
	key RegionKey,
	done func(),
) {
	dst := m.uvmControlPort(key.DeviceID)
	if dst == "" {
		done()
		return
	}

	region := m.regions[key]
	if region == nil {
		done()
		return
	}

	frames := make([]uint64, 0, len(region.Pages))

	for _, pk := range region.Pages {
		if managedPage := m.pages[pk]; managedPage != nil &&
			managedPage.GPUFrameValid {
			frames = append(frames, managedPage.GPUFramePAddr)
		}
	}

	req := protocol.NewUVMCacheRangeFlushReq(
		m.d.uvmPort.AsRemote(), dst,
		key.PID, key.Base, m.config.RegionSize,
		frames, m.config.PageSize)

	m.controlOps[req.ID] = &pendingControlOp{Kind: opCacheRangeFlush, Done: done}
	m.stats.CacheRangeFlushes++
	m.enqueueControlLocked(req)
}

// requestRemoteDrainLocked waits until the GPU has no outstanding remote access
// to the region, then resumes.
//
// A CPU-to-GPU migration snapshots host memory. Any remote store still on its
// way to host memory would land after that snapshot, leaving the GPU copy
// stale and letting the eventual eviction write it back over the correct host
// data. Draining first makes the snapshot authoritative. // sbin_codex
func (m *UVMManager) requestRemoteDrainSpanLocked(
	key RegionKey,
	startVAddr, size uint64,
	done func(),
) {
	dst := m.uvmControlPort(key.DeviceID)
	if dst == "" || !m.config.AccessCounterEnabled {
		done()
		return
	}

	req := protocol.NewUVMRemoteDrainReq(
		m.d.uvmPort.AsRemote(), dst, key.PID, startVAddr, size)

	m.controlOps[req.ID] = &pendingControlOp{Kind: opRemoteDrain, Done: done}
	m.stats.RemoteDrains++
	m.enqueueControlLocked(req)
}

// drainRegionsThen drains every region of a migration in turn.
func (m *UVMManager) drainRegionsThen(keys []RegionKey, done func()) {
	remaining := append([]RegionKey(nil), keys...)

	var step func()

	step = func() {
		if len(remaining) == 0 {
			done()
			return
		}

		key := remaining[0]
		remaining = remaining[1:]

		m.requestRemoteDrainSpanLocked(
			key, key.Base, m.config.RegionSize, step)
	}

	step()
}

// sendFaultReplayLocked tells the GPU that one region became usable again. The
// GMMU re-runs every stalled translation in the range and the access counter
// releases any write it held for the region.
func (m *UVMManager) sendFaultReplayLocked(key RegionKey) {
	m.sendReplayLocked(key, false)
}

// sendRefusedReplayLocked reports that the region will not become GPU-local.
// Requests that were only stalled to force a migration must complete the
// ordinary way instead of waiting for a mapping that will not arrive.
// sbin_codex
func (m *UVMManager) sendRefusedReplayLocked(key RegionKey) {
	m.sendReplayLocked(key, true)
}

func (m *UVMManager) sendReplayLocked(key RegionKey, refused bool) {
	dst := m.uvmControlPort(key.DeviceID)
	if dst == "" {
		return
	}

	req := vm.NewUVMFaultReplayReq(m.d.uvmPort.AsRemote(), dst)
	req.PID = key.PID
	req.StartVA = key.Base
	req.Size = m.config.RegionSize
	req.DeviceID = key.DeviceID
	req.Refused = refused

	m.stats.FaultReplays++

	if refused {
		m.stats.RefusedMigrations++
	}

	m.enqueueControlLocked(req)
}

// onControlRsp resumes the sequence that was waiting on a GPU acknowledgement.
func (m *UVMManager) onControlRsp(rspTo string) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	op, found := m.controlOps[rspTo]
	if !found {
		return
	}

	delete(m.controlOps, rspTo)

	if op.Done != nil {
		op.Done()
	}
}

// completeMigration finalizes the transfer of one migration: it installs the
// new mappings and then runs the invalidation and replay the transition
// requires.
func (m *UVMManager) completeMigration(migID string) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	mig := m.migrations[migID]
	if mig == nil {
		return
	}

	mig.DataFinishedAt = m.d.TickScheduler.CurrentTime()
	if !m.config.Ideal {
		m.stats.MigrationTime += mig.DataFinishedAt - mig.DataStartedAt
	}

	if mig.Direction == CPUToGPU {
		m.completeCPUToGPUMigrationLocked(mig)
		return
	}

	m.completeEvictionMigrationLocked(mig)
}

func (m *UVMManager) completeCPUToGPUMigrationLocked(mig *Migration) {
	now := m.d.TickScheduler.CurrentTime()

	for _, pk := range mig.Pages {
		managedPage := m.pages[pk]
		if managedPage == nil {
			continue
		}

		managedPage.State = GPUResident
		managedPage.RemoteMapped = false
		managedPage.TimesMigrated++

		m.d.memAllocator.UpdatePage(vm.Page{
			PID:              managedPage.Key.PID,
			VAddr:            managedPage.Key.VAddr,
			PAddr:            managedPage.GPUFramePAddr,
			PageSize:         m.config.PageSize,
			Valid:            true,
			DeviceID:         mig.DeviceID,
			Unified:          false,
			Managed:          true,
			IsMigrating:      false,
			RemoteAccessible: false,
		})

		m.stats.LocalPTEInstalls++
		m.stats.GPUResidentPages++
	}

	for _, key := range mig.RegionKeys {
		if region := m.regions[key]; region != nil {
			region.LastMigrationTime = now
			m.addLRU(key)
		}
	}

	m.updateResidencyPeak()

	if !mig.NeedsTLBInvalidate {
		m.finishMigrationLocked(mig)
		return
	}

	// REMOTE -> GPU_LOCAL: a valid remote translation may sit in the L2 TLB,
	// so the 64KB invalidation is mandatory before the region is replayed.
	m.invalidateRegionsThen(mig.RegionKeys, func() {
		m.finishMigrationLocked(mig)
	})
}

// invalidateRegionsThen walks the regions of a migration, invalidating each in
// turn, and runs done once the last acknowledgement arrives.
func (m *UVMManager) invalidateRegionsThen(keys []RegionKey, done func()) {
	remaining := append([]RegionKey(nil), keys...)
	if len(remaining) == 0 {
		done()
		return
	}

	var step func()

	step = func() {
		if len(remaining) == 0 {
			done()
			return
		}

		key := remaining[0]
		remaining = remaining[1:]

		m.requestTLBInvalidateLocked(key, step)
	}

	step()
}

// finishMigrationLocked retires a completed CPU-to-GPU migration: it releases
// the regions, re-arms their access counters, and replays every request that
// was stalled on them.
func (m *UVMManager) finishMigrationLocked(mig *Migration) {
	for _, key := range mig.RegionKeys {
		if region := m.regions[key]; region != nil {
			region.MigrationID = ""
			if region.Phase == RegionMigratingToGPU {
				region.Phase = RegionIdle
			}
		}
	}

	m.queueAccessCounterResets(mig)

	for _, pk := range mig.Pages {
		if m.migrationsByPage[pk] == mig.ID {
			delete(m.migrationsByPage, pk)
		}
	}

	delete(m.migrations, mig.ID)

	for _, key := range mig.RegionKeys {
		m.sendFaultReplayLocked(key)
	}

	for _, faultID := range mig.FaultIDs {
		if txn := m.faultsByID[faultID]; txn != nil {
			m.finishFaultLocked(txn)
		}
	}
}
