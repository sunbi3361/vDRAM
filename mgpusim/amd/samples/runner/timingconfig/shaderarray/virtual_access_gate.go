package shaderarray

// VirtualAccessGateWaiterCounter is the waiter-count reporting seam for
// virtual-caching L1V/L1S access gates (plan todo 7 of mgpusim-uvm-manager).
// Virtual-caching has no leaf data TLB, so each gate is the leaf data
// translation point: it records every original request that reaches it and
// reports raw = unique + coalesced (uvm-manager.md §8.4). The full gate
// admission and watermark logic is a later todo; this type only owns the
// count reporting. // sbin_codex
type VirtualAccessGateWaiterCounter struct {
	unique    uint32
	coalesced uint32
}

// RecordOriginalRequest records one original request that reached the gate.
// unique marks the first request for its 64 KB fault-service region; duplicate
// waiters for an already-pending region are coalesced. // sbin_codex
func (c *VirtualAccessGateWaiterCounter) RecordOriginalRequest(unique bool) {
	if unique {
		c.unique++
	} else {
		c.coalesced++
	}
}

// Raw returns the raw fault count: unique + coalesced. // sbin_codex
func (c *VirtualAccessGateWaiterCounter) Raw() uint32 {
	return c.unique + c.coalesced
}

// Unique returns the number of unique original requests recorded. // sbin_codex
func (c *VirtualAccessGateWaiterCounter) Unique() uint32 {
	return c.unique
}

// Coalesced returns the number of coalesced (duplicate) waiters recorded. // sbin_codex
func (c *VirtualAccessGateWaiterCounter) Coalesced() uint32 {
	return c.coalesced
}