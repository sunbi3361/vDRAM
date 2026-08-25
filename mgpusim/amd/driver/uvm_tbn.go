package driver

// sbin_codex: Tree-Based Neighborhood prefetcher (spec 11).
//
// A GPU fault is identified at 4KB granularity but is expanded to its aligned
// 64KB leaf for the occupancy tree. The occupancy mask is exactly
//
//	TBNOccupancyMask = GPUResidentMask OR CurrentFaultExpanded64KBMask
//
// In-flight prefetch and migration masks are deliberately excluded from the
// numerator; they only suppress duplicate DMA later (spec 11.13). An ancestor
// is entered only when occupied*100 > total*51, strictly greater, and the
// search never crosses the 2MB VA block.

// tbnSelection is the result of one neighborhood selection.
type tbnSelection struct {
	regionBase    uint64
	selectedSize  uint64
	pageKeys      []PageKey
	demandPages   uint64
	prefetchPages uint64
}

// selectTBNRegion chooses the pages one fault service migrates.
func (m *UVMManager) selectTBNRegion(
	key RegionKey,
	block *VABlock,
) tbnSelection {
	cfg := m.config
	txn := m.faults[key]

	selectedBase := key.Base
	selectedSize := cfg.RegionSize

	if !cfg.PrefetchDisabled {
		selectedBase, selectedSize = m.expandNeighborhood(key, block)
	}

	m.stats.TBNFaultEvents++
	m.stats.TBNSelections[tbnLevelIndex(selectedSize, cfg.RegionSize)]++

	return m.buildSelection(txn, block, selectedBase, selectedSize)
}

// expandNeighborhood walks up the tree while the occupancy threshold holds.
func (m *UVMManager) expandNeighborhood(
	key RegionKey,
	block *VABlock,
) (uint64, uint64) {
	cfg := m.config
	base := key.Base
	size := cfg.RegionSize

	for size < cfg.TBNMaxFetchSize {
		nextSize := size * 2
		nodeBase := cfg.alignDown(base, nextSize)

		if nodeBase < block.Key.Base ||
			nodeBase+nextSize > block.Key.Base+cfg.VABlockSize {
			break
		}

		occupied, total := m.nodeOccupancy(key, nodeBase, nextSize)
		if total == 0 || occupied*100 <= total*51 {
			break
		}

		base = nodeBase
		size = nextSize
	}

	return base, size
}

// nodeOccupancy counts the occupied and the valid 4KB pages of a candidate.
// Only pages that belong to a managed allocation count toward the denominator,
// which is how a partial first or last VA block is handled (spec 11.10).
func (m *UVMManager) nodeOccupancy(
	key RegionKey,
	base, size uint64,
) (occupied, total uint64) {
	cfg := m.config
	leafBase := key.Base
	leafEnd := leafBase + cfg.RegionSize

	for vAddr := base; vAddr < base+size; vAddr += cfg.PageSize {
		managedPage := m.pages[PageKey{PID: key.PID, VAddr: vAddr}]
		if managedPage == nil {
			continue
		}

		total++

		if vAddr >= leafBase && vAddr < leafEnd {
			occupied++
			continue
		}

		if managedPage.State == GPUResident {
			occupied++
		}
	}

	return occupied, total
}

// buildSelection turns the selected region into the actual migration list,
// dropping pages that are resident or already in flight (spec 11.8, 11.9).
func (m *UVMManager) buildSelection(
	txn *FaultTransaction,
	block *VABlock,
	base, size uint64,
) tbnSelection {
	cfg := m.config
	sel := tbnSelection{regionBase: base, selectedSize: size}

	for vAddr := base; vAddr < base+size; vAddr += cfg.PageSize {
		pk := PageKey{PID: block.Key.PID, VAddr: vAddr}

		managedPage := m.pages[pk]
		if managedPage == nil {
			continue
		}

		m.stats.TBNSelectedBytes += cfg.PageSize

		isDemand := txn != nil && txn.demandVAddrs[vAddr]
		if isDemand {
			m.stats.TBNDemandBytes += cfg.PageSize
		} else {
			m.stats.TBNPrefetchCandidateByte += cfg.PageSize
		}

		if managedPage.State == GPUResident {
			m.stats.TBNSuppressedResident += cfg.PageSize
			continue
		}

		if _, inFlight := m.migrationsByPage[pk]; inFlight {
			m.stats.TBNSuppressedInflight += cfg.PageSize
			continue
		}

		sel.pageKeys = append(sel.pageKeys, pk)

		if isDemand {
			sel.demandPages++
			continue
		}

		sel.prefetchPages++
		m.stats.TBNActualPrefetchBytes += cfg.PageSize
	}

	return sel
}

// tbnLevelIndex maps a selected size to its statistics slot, 0 for the 64KB
// leaf up to 5 for a full 2MB VA block.
func tbnLevelIndex(size, regionSize uint64) int {
	index := 0
	for s := regionSize; s < size && index < 5; s *= 2 {
		index++
	}

	return index
}
