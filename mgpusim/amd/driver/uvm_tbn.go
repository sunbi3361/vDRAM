package driver

// tbnSelection holds the result of a TBN region selection.
type tbnSelection struct {
	regionBase    uint64
	pageKeys      []PageKey
	demandKey     PageKey
	demandPages   uint64
	prefetchPages uint64
}

// selectTBNRegion selects the neighborhood to migrate for a demand fault.
// The demanded 4KB page is always included. The minimum fetch is the aligned
// 64KB leaf. Within the 2MB VA block, the selection expands up the hierarchy:
// when at least TBNExpandRatio (default 51%) of the pages inside a node are
// already GPU-resident, the whole node is migrated. // sbin_codex
func (m *UVMManager) selectTBNRegion(faultKey PageKey, block *VABlock) tbnSelection {
	cfg := m.config
	regionBase := cfg.alignDown(faultKey.VAddr, cfg.RegionSize)
	regionIndex := (regionBase - block.Key.Base) / cfg.RegionSize

	sel := tbnSelection{
		regionBase: regionBase,
		demandKey:  faultKey,
	}

	// Always include the demanded 64KB leaf.
	selectedBases := []uint64{regionBase}
	sel.pageKeys = append(sel.pageKeys, m.regionPageKeys(block, regionIndex)...)

	// Hierarchical expansion: 64KB -> 128KB -> ... -> up to the max fetch.
	curBase := regionBase
	curSize := cfg.RegionSize
	for curSize < cfg.TBNMaxFetchSize {
		nextSize := curSize * 2
		nodeBase := cfg.alignDown(curBase, nextSize)
		if nodeBase+nextSize > block.Key.Base+cfg.VABlockSize {
			break
		}
		if !m.nodeResidentRatio(block, nodeBase, nextSize) {
			break
		}
		// Migrate the whole node: add every leaf under it.
		for b := nodeBase; b < nodeBase+nextSize; b += cfg.RegionSize {
			idx := (b - block.Key.Base) / cfg.RegionSize
			selectedBases = append(selectedBases, b)
			sel.pageKeys = append(sel.pageKeys, m.regionPageKeys(block, idx)...)
		}
		curBase = nodeBase
		curSize = nextSize
	}

	if curSize == cfg.RegionSize {
		m.stats.TBN64KBFetches++
	} else {
		m.stats.TBNLargerFetches++
	}
	m.stats.TBNFetches++

	// Count demand vs prefetch and drop pages already GPU-resident.
	demand := 0
	prefetch := 0
	filtered := sel.pageKeys[:0]
	for _, pk := range sel.pageKeys {
		mp := m.pages[pk]
		if mp == nil {
			continue
		}
		if mp.GPUFrameValid && mp.State == GPUResident {
			continue
		}
		filtered = append(filtered, pk)
		if pk == sel.demandKey {
			demand++
		} else {
			prefetch++
		}
	}
	sel.pageKeys = filtered
	sel.demandPages = uint64(demand)
	sel.prefetchPages = uint64(prefetch)

	return sel
}

func (m *UVMManager) regionPageKeys(block *VABlock, regionIndex uint64) []PageKey {
	region := block.Regions[regionIndex]
	if region == nil {
		return nil
	}
	return region.Pages
}

// nodeResidentRatio reports whether at least TBNExpandRatio of the pages
// inside the node [base, base+size) are GPU-resident. If the ratio is 51% or
// more, the whole node becomes a migration candidate. // sbin_codex
func (m *UVMManager) nodeResidentRatio(block *VABlock, base, size uint64) bool {
	cfg := m.config
	ratio := cfg.TBNExpandRatio
	if ratio <= 0 {
		return false
	}
	if ratio >= 1 {
		return true
	}

	var total, resident uint64
	for b := base; b < base+size; b += cfg.PageSize {
		pk := PageKey{PID: block.Key.PID, VAddr: b}
		mp := m.pages[pk]
		if mp == nil {
			continue
		}
		total++
		if mp.GPUFrameValid && mp.State == GPUResident {
			resident++
		}
	}
	if total == 0 {
		return false
	}
	return float64(resident)/float64(total) >= ratio
}
