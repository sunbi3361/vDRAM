package tlb

import "github.com/sarchlab/akita/v4/mem/vm"

// sbin_codex: non-stalling, PID/VA-range scoped TLB invalidation used by the
// UVM control path. Unlike FlushReq this never changes the component state, so
// translations for unrelated addresses keep flowing while it runs.

// handleInvalidateRange drops every cached entry of req.PID whose page overlaps
// [req.StartVA, req.StartVA+req.Size) and marks overlapping in-flight lookups
// so their fills are not installed. It responds in the same cycle.
func (m *ctrlMiddleware) handleInvalidateRange(req *InvalidateRangeReq) bool {
	rsp := InvalidateRangeRspBuilder{}.
		WithSrc(m.controlPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		Build()

	if err := m.controlPort.Send(rsp); err != nil {
		return false
	}

	m.invalidateRange(req.PID, req.StartVA, req.Size)
	m.controlPort.RetrieveIncoming()

	return true
}

// invalidateRange performs the entry and MSHR bookkeeping for one range.
func (c *Comp) invalidateRange(pid vm.PID, startVA, size uint64) {
	if size == 0 {
		return
	}

	begin := startVA / c.pageSize * c.pageSize
	end := (startVA + size - 1) / c.pageSize * c.pageSize

	for vAddr := begin; ; vAddr += c.pageSize {
		setID := int(vAddr / c.pageSize % uint64(c.numSets))
		set := c.sets[setID]

		if wayID, page, found := set.Lookup(pid, vAddr); found {
			page.Valid = false
			set.Update(wayID, page)
		}

		if vAddr == end {
			break
		}
	}

	for _, entry := range c.mshr.AllEntries() {
		if entry.pid != pid {
			continue
		}

		if entry.vAddr >= begin && entry.vAddr <= end {
			entry.staleOnFill = true
		}
	}
}
