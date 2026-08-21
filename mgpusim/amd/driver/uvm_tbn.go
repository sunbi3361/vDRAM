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
// 64KB leaf; the neighborhood expands up to TBNMaxFetchSize when sibling
// activity reaches the expand threshold.
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

	// Hierarchical expansion: 64KB -> 128KB -> 256KB -> ... up to the max.
	curBase := regionBase
	curSize := cfg.RegionSize
	for curSize < cfg.TBNMaxFetchSize {
		nextSize := curSize * 2
		// The "sibling subtree" of the current neighborhood covers the other
		// half of the next-larger neighborhood. Its base is either the
		// next-larger base or the current base offset by curSize.
		largerBase := cfg.alignDown(curBase, nextSize)
		siblingBase := largerBase
		if siblingBase == curBase {
			siblingBase = curBase + curSize
		}
		// Sibling is within the same 2MB block.
		if siblingBase+curSize > block.Key.Base+cfg.VABlockSize {
			break
		}
		if m.subtreeActivity(block, siblingBase, curSize) < cfg.TBNExpandThreshold {
			break
		}
		// Expand: include the sibling subtree.
		for b := siblingBase; b < siblingBase+curSize; b += cfg.RegionSize {
			idx := (b - block.Key.Base) / cfg.RegionSize
			selectedBases = append(selectedBases, b)
			sel.pageKeys = append(sel.pageKeys, m.regionPageKeys(block, idx)...)
		}
		curBase = largerBase
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

// subtreeActivity sums the per-region activity counters in the range
// [base, base+size) within the block.
func (m *UVMManager) subtreeActivity(block *VABlock, base, size uint64) uint64 {
	cfg := m.config
	var activity uint64
	for b := base; b < base+size; b += cfg.RegionSize {
		idx := (b - block.Key.Base) / cfg.RegionSize
		if idx < uint64(len(block.Activity)) {
			activity += uint64(block.Activity[idx])
		}
	}
	return activity
}
