package driver

import (
	"fmt"

	"github.com/sarchlab/akita/v4/sim"
)

// sbin_codex: VA-block geometry and per-page state (uvm-manager.md §4, §4.1,
// §4.2, §5.2). The 2 MB VA block is the maximum TBN prefetch region; the 64 KB
// sub-block is the minimum TBN node and the access-counter/eviction region.

// Fixed UVM address granularity (uvm-manager.md §4). The config validates
// VABlockSize == 2 MB and TBNMinNodeSize == 64 KB, so these constants are the
// canonical geometry.
const (
	vablockSizeBytes  = 2 * 1024 * 1024                      // 2 MB VA block
	subblockSizeBytes = 64 * 1024                            // 64 KB TBN leaf / region
	pagesPerVABlock   = vablockSizeBytes / basePageSize      // 512
	subBlocksPerBlock = vablockSizeBytes / subblockSizeBytes // 32
	pagesPerSubBlock  = subblockSizeBytes / basePageSize     // 16
)

// PageMigrationState is the per-4KB-page migration direction (uvm-manager.md §4.2).
type PageMigrationState int

const (
	PageAtCPU PageMigrationState = iota
	PageMigratingToGPU
	PageAtGPU
	PageMigratingToCPU
)

// PageState is the per-4KB-page detail (uvm-manager.md §4.2). Residency,
// in-flight, dirty, and valid bits remain authoritative in the registration
// masks (todo 3); this struct carries the physical and mapping detail the
// masks do not.
type PageState struct {
	GPUPhysicalPage uint64 // HBM PA when GPU-resident, else 0
	CPUPhysicalPage uint64 // authoritative CPU backing PA (always set for managed)
	RemoteMapped    bool   // GPU PTE maps the page remotely (CPU_REMOTE)
	CachedOnGPU     bool   // page data present in a GPU data cache
	MigrationState  PageMigrationState
	LastAccess      sim.VTimeInSec
}

// VABlock models one 2 MB VA block (uvm-manager.md §4.1, §5.2): 512 base pages
// in 32 x 64 KB sub-blocks. TBN must never expand across a block boundary.
type VABlock struct {
	StartVA uint64
	Size    uint64
	Index   uint64 // block index within the allocation

	Pages     [pagesPerVABlock]PageState
	SubBlocks [subBlocksPerBlock]*SubBlockState

	ResidentBytesGPU uint64 // maintained aggregate; invariant checker recomputes
	ResidentBytesCPU uint64
	LastAccessTime   sim.VTimeInSec
}

// NewVABlock builds an empty 2 MB VA block at startVA.
func NewVABlock(startVA uint64, idx uint64) *VABlock {
	block := &VABlock{
		StartVA: startVA,
		Size:    vablockSizeBytes,
		Index:   idx,
	}
	for s := 0; s < subBlocksPerBlock; s++ {
		block.SubBlocks[s] = NewSubBlockState(startVA + uint64(s)*subblockSizeBytes)
	}
	return block
}

// BlockForVA returns the 2 MB-aligned base VA of the block containing va.
func BlockForVA(va uint64) uint64 { return va &^ (vablockSizeBytes - 1) }

// SubBlockStartVA returns the 64 KB-aligned base VA of the region containing va.
func SubBlockStartVA(va uint64) uint64 { return va &^ (subblockSizeBytes - 1) }

// PageIndexInBlock returns the 0..511 page index within the block for va.
func PageIndexInBlock(va uint64) uint64 { return (va % vablockSizeBytes) / basePageSize }

// SubBlockIndexForPage returns the 0..31 sub-block index for a page index.
func SubBlockIndexForPage(pageIdx uint64) uint64 { return pageIdx / pagesPerSubBlock }

// SubBlockIndexForVA returns the 0..31 sub-block index for a VA.
func SubBlockIndexForVA(va uint64) uint64 { return PageIndexInBlock(va) / pagesPerSubBlock }

// TBNNodeWithinBlock verifies that a 64 KB TBN leaf node starting at nodeVA is
// entirely inside one 2 MB VA block (uvm-manager.md §4.1: TBN must not expand
// across a 2 MB VA block boundary).
func TBNNodeWithinBlock(nodeVA uint64) error {
	if nodeVA%subblockSizeBytes != 0 {
		return fmt.Errorf("uvm: TBN node %#x not 64 KB aligned", nodeVA)
	}
	if BlockForVA(nodeVA) != BlockForVA(nodeVA+subblockSizeBytes-1) {
		return fmt.Errorf("uvm: TBN node %#x crosses a 2 MB VA block boundary", nodeVA)
	}
	return nil
}

// buildVABlocks constructs the VA-block model for a registration over its
// authoritative masks. Blocks are 2 MB-aligned in VA space; an allocation that
// starts or ends mid-block keeps only the pages covered by ValidMask, and
// every valid page receives its CPU backing PA.
func buildVABlocks(reg *ManagedAllocationRegistration) []*VABlock {
	firstBlockVA := BlockForVA(reg.Base)
	lastVA := reg.Base + reg.PageCount*basePageSize - 1
	lastBlockVA := BlockForVA(lastVA)
	numBlocks := lastBlockVA/vablockSizeBytes - firstBlockVA/vablockSizeBytes + 1
	blocks := make([]*VABlock, 0, numBlocks)
	for b := uint64(0); b < numBlocks; b++ {
		blockVA := firstBlockVA + b*vablockSizeBytes
		block := NewVABlock(blockVA, b)
		for blockLocal := uint64(0); blockLocal < pagesPerVABlock; blockLocal++ {
			va := blockVA + blockLocal*basePageSize
			if va < reg.Base || va >= reg.Base+reg.PageCount*basePageSize {
				continue
			}
			allocPage := (va - reg.Base) / basePageSize
			block.Pages[blockLocal].CPUPhysicalPage = reg.CPUBackingPages[allocPage]
		}
		blocks = append(blocks, block)
	}
	return blocks
}
