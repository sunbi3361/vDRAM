// Package uvm implements the GPU-side UVM remote-access components (plan todo
// 11 of mgpusim-uvm-manager): the GPU-wide AccessCounter and the CPU-remote
// memory endpoint. Both sit behind the UVM access gates and observe remote
// accesses after translation (uvm-manager.md §6.1, §14).
package uvm

import (
	// log "log" // sbin_codex (todo 25): unused after the loop-back panics were removed.
	// "reflect" // sbin_codex (todo 25): unused after the loop-back panics were removed.

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp"
)

// defaultAccessCounterThreshold is the fixed default remote-access threshold
// (uvm-manager.md §14.1: uvm-access-counter-threshold = 8).
const defaultAccessCounterThreshold = 8 // sbin_codex

// regionKey identifies one 64 KB accounting region for one process on one
// GPU (uvm-manager.md §14: (PID, VA_block, 64KB_region)).
type regionKey struct {
	pid        vm.PID
	gpu        int
	regionBase uint64 // 64 KB-aligned VA region base
}

// AccessCounter is the GPU-wide remote-access counter (uvm-manager.md §14,
// §16, §31.1). It counts CPU-remote GPU accesses per (PID, GPU, 64 KB region)
// and emits an immediate equality notification when a region reaches the
// threshold. Notifications are suppressed while the region is in
// FAULT_PENDING / FAULT_HANDLING / MIGRATING_TO_GPU / PREFETCHING_TO_GPU, a
// GPU-resident region does not count until it returns to CPU-resident, and
// every counter is reset at kernel launch through the acknowledged
// CounterResetReq barrier on the CP seam.
//
// sbin_codex: the counter is the other end of the CP's ToAccessCounter seam
// (todo 12); the ToCP port is shared with the CP's ToAccessCounter port, so
// messages are addressed loop-back style (Src = Dst = ToCP.AsRemote()).
type AccessCounter struct {
	*sim.TickingComponent
	sim.MiddlewareHolder

	// ToCP is the CP seam: it receives CounterResetReq and carries
	// CounterResetRsp and AccessCounterNotification back to the CP.
	ToCP sim.Port

	threshold uint64

	counters             map[regionKey]uint64
	notified             map[regionKey]bool
	suppressed           map[regionKey]bool
	gpuResident          map[regionKey]bool
	pendingNotifications []*protocol.AccessCounterNotification
}

// Tick drives the counter pipeline.
func (c *AccessCounter) Tick() bool {
	return c.MiddlewareHolder.Tick()
}

// RecordRemoteAccess increments the 64 KB region counter of one remote GPU
// access once. When the counter reaches the threshold the counter emits an
// immediate equality notification, at most once per residency episode; the
// notification is suppressed while the region is in a migration/prefetch
// state (§16). A GPU-resident region does not count (§31.1).
func (c *AccessCounter) RecordRemoteAccess(pid vm.PID, gpu int, vAddr uint64) {
	key := regionKey{pid: pid, gpu: gpu, regionBase: (vAddr >> 16) << 16}
	if c.gpuResident[key] {
		return
	}
	c.counters[key]++
	if c.counters[key] < c.threshold {
		return
	}
	if c.suppressed[key] {
		// The threshold-crossing event of this residency episode is
		// suppressed: no notification, and no retroactive notification after
		// the region leaves the migration/prefetch state (§16).
		c.notified[key] = true
		return
	}
	if c.notified[key] {
		return
	}
	c.notified[key] = true
	c.pendingNotifications = append(c.pendingNotifications,
		c.buildNotification(key))
}

// buildNotification builds the driver-envelope threshold notification for the
// region, addressed loop-back to the CP seam.
func (c *AccessCounter) buildNotification(
	key regionKey,
) *protocol.AccessCounterNotification {
	return protocol.AccessCounterNotificationBuilder{}.
		WithSrc(c.ToCP.AsRemote()).
		WithDst(c.ToCP.AsRemote()).
		WithPID(key.pid).
		WithGPU(key.gpu).
		WithVAddr(key.regionBase).
		WithAccessKind(vm.AccessKindRead).
		WithAccessCount(c.counters[key]).
		Build()
}

// Reset clears every counter and episode flag at kernel launch
// (uvm-manager.md §14.2, §31.1). GPU residency persists across kernels.
func (c *AccessCounter) Reset() {
	c.counters = make(map[regionKey]uint64)
	c.notified = make(map[regionKey]bool)
	c.suppressed = make(map[regionKey]bool)
	c.pendingNotifications = nil
}

// Suppress marks a region as being serviced (FAULT_PENDING / FAULT_HANDLING /
// MIGRATING_TO_GPU / PREFETCHING_TO_GPU): its accesses keep counting but
// never notify (§16).
func (c *AccessCounter) Suppress(pid vm.PID, gpu int, regionBase uint64) {
	c.suppressed[regionKey{pid: pid, gpu: gpu, regionBase: regionBase}] = true
}

// Unsuppress clears the migration/prefetch suppression of a region.
func (c *AccessCounter) Unsuppress(pid vm.PID, gpu int, regionBase uint64) {
	delete(c.suppressed, regionKey{pid: pid, gpu: gpu, regionBase: regionBase})
}

// MarkGPUResident stops counting a region: once it becomes GPU-resident its
// remote counter no longer affects accesses (§31.1).
func (c *AccessCounter) MarkGPUResident(pid vm.PID, gpu int, regionBase uint64) {
	key := regionKey{pid: pid, gpu: gpu, regionBase: regionBase}
	c.gpuResident[key] = true
	c.notified[key] = false
	delete(c.suppressed, key)
}

// MarkCPUResident resumes counting a region that returned to CPU-resident and
// opens a new residency episode for notifications (§31.1).
func (c *AccessCounter) MarkCPUResident(pid vm.PID, gpu int, regionBase uint64) {
	key := regionKey{pid: pid, gpu: gpu, regionBase: regionBase}
	delete(c.gpuResident, key)
	c.notified[key] = false
}

// Count returns the current counter value of a region (test/control accessor).
func (c *AccessCounter) Count(pid vm.PID, gpu int, regionBase uint64) uint64 {
	return c.counters[regionKey{pid: pid, gpu: gpu, regionBase: regionBase}]
}

// Threshold returns the configured remote-access threshold.
func (c *AccessCounter) Threshold() uint64 {
	return c.threshold
}

// SetThreshold changes the remote-access threshold.
func (c *AccessCounter) SetThreshold(threshold uint64) {
	c.threshold = threshold
}

// accessCounterMiddleware drives the counter's CP seam and notification
// queue.
type accessCounterMiddleware struct {
	*AccessCounter
}

// Tick processes the acknowledged reset barrier and flushes queued threshold
// notifications.
func (m *accessCounterMiddleware) Tick() bool {
	madeProgress := false
	madeProgress = m.processResetReq() || madeProgress
	madeProgress = m.flushNotifications() || madeProgress
	return madeProgress
}

// processResetReq consumes one CounterResetReq, resets every counter, and
// acknowledges the reset to the CP so the kernel-dispatch barrier can clear
// (uvm-manager.md §14.2). A non-reset message on the shared ToCP seam (e.g.
// the counter's own loop-back notification) is left for the CP. // sbin_codex
func (m *accessCounterMiddleware) processResetReq() bool {
	msg := m.ToCP.PeekIncoming()
	if msg == nil {
		return false
	}
	req, ok := msg.(*cp.CounterResetReq)
	if !ok {
		// sbin_codex (todo 25): the shared ToCP seam carries the counter's
		// own loop-back notifications; skip them instead of panicking.
		return false
	}
	m.Reset()
	rsp := &cp.CounterResetRsp{}
	rsp.ID = sim.GetIDGenerator().Generate()
	rsp.Src = m.ToCP.AsRemote()
	rsp.Dst = req.Src
	// sbin_codex (todo 25): the reset ack is a loop-back message on the
	// shared ToCP seam (Src == Dst == ToCP.AsRemote()); a real port's Send
	// rejects src == dst, so the ack is delivered directly into the shared
	// port's incoming buffer, which the CP peeks.
	// if err := m.ToCP.Send(rsp); err != nil {
	// 	return false
	// }
	if err := m.ToCP.Deliver(rsp); err != nil {
		return false
	}
	m.ToCP.RetrieveIncoming()
	return true
}

// flushNotifications sends queued threshold notifications to the CP seam,
// one per tick.
func (m *accessCounterMiddleware) flushNotifications() bool {
	if len(m.pendingNotifications) == 0 {
		return false
	}
	notif := m.pendingNotifications[0]
	// sbin_codex (todo 25): the notification is a loop-back message on the
	// shared ToCP seam (Src == Dst == ToCP.AsRemote()); a real port's Send
	// rejects src == dst, so the notification is delivered directly into the
	// shared port's incoming buffer, which the CP peeks.
	// if err := m.ToCP.Send(notif); err != nil {
	// 	return false
	// }
	if err := m.ToCP.Deliver(notif); err != nil {
		return false
	}
	m.pendingNotifications = m.pendingNotifications[1:]
	return true
}

// AccessCounterBuilder builds an AccessCounter.
type AccessCounterBuilder struct {
	engine    sim.Engine
	freq      sim.Freq
	threshold uint64
}

// MakeAccessCounterBuilder creates a new AccessCounter builder with the
// default threshold.
func MakeAccessCounterBuilder() AccessCounterBuilder {
	return AccessCounterBuilder{
		freq:      1 * sim.GHz,
		threshold: defaultAccessCounterThreshold,
	}
}

// WithEngine sets the engine of the counter to build.
func (b AccessCounterBuilder) WithEngine(engine sim.Engine) AccessCounterBuilder {
	b.engine = engine
	return b
}

// WithFreq sets the frequency of the counter to build.
func (b AccessCounterBuilder) WithFreq(freq sim.Freq) AccessCounterBuilder {
	b.freq = freq
	return b
}

// WithThreshold sets the remote-access threshold of the counter to build.
func (b AccessCounterBuilder) WithThreshold(threshold uint64) AccessCounterBuilder {
	b.threshold = threshold
	return b
}

// Build creates a new AccessCounter.
func (b AccessCounterBuilder) Build(name string) *AccessCounter {
	c := &AccessCounter{
		threshold:            b.threshold,
		counters:             make(map[regionKey]uint64),
		notified:             make(map[regionKey]bool),
		suppressed:           make(map[regionKey]bool),
		gpuResident:          make(map[regionKey]bool),
		pendingNotifications: make([]*protocol.AccessCounterNotification, 0),
	}
	c.TickingComponent = sim.NewTickingComponent(name, b.engine, b.freq, c)
	c.ToCP = sim.NewPort(c, 32, 32, name+".ToCP")
	c.AddPort("ToCP", c.ToCP)
	middleware := &accessCounterMiddleware{AccessCounter: c}
	c.AddMiddleware(middleware)
	return c
}
