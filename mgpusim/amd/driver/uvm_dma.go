package driver

import (
	"fmt"

	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/driver/internal"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// sbin_codex: maximal-run migration through the existing CP DMA Engine with
// capacity reservations (plan todo 16 of mgpusim-uvm-manager, uvm-manager.md
// §23.1.2, §17.1). A migration transfers only the pages that are neither
// GPU-resident nor in flight; the driver reserves the required GPU capacity
// (AdmissionReservation N) BEFORE any destination frame allocation, allocates
// the destination PFNs, forms maximal runs whose source AND destination
// physical addresses are both contiguous, and emits ONE existing
// MemCopyH2DReq / MemCopyD2HReq per run (driver -> CP -> DMA engine). Each
// run is tracked to completion; PTE/residency is committed only after ALL
// runs succeed, and the reservation/frames are released exactly once on
// success or rollback. D2H always copies every valid page of a logical 64 KB
// eviction. No UVM-side concurrency cap is added (the DMA engine's own
// maxRequestCount = 4 governs) and the DMA's 64-byte internal transactions
// (Log2AccessSize = 6) are preserved.

// migrationPage describes one 4 KB page of a migration transfer.
type migrationPage struct {
	Page  uint64 // allocation page index within the registration
	SrcPA uint64 // source physical address (CPU backing PA for H2D, GPU PA for D2H)
	DstPA uint64 // destination physical address (GPU PA for H2D, CPU backing PA for D2H)
}

// migrationRun is one maximal contiguous run of a migration: consecutive
// pages whose source AND destination physical addresses are both contiguous.
type migrationRun struct {
	SrcStart uint64   // first source PA of the run
	DstStart uint64   // first destination PA of the run
	Bytes    uint64   // runBytes = len(Pages) * 4 KB
	Pages    []uint64 // allocation page indices in VA order
}

// migrationPlan is the complete run set of one migration plus its accounting.
type migrationPlan struct {
	Runs       []migrationRun
	TotalBytes uint64 // sum of run bytes (== PageCount * 4 KB)
	PageCount  int

	// frames are the destination GPU frames allocated by THIS migration;
	// rollback frees them exactly once.
	frames []uint64
	// allocatedPages are the pages whose destination frames were allocated by
	// this migration; rollback resets their GPUPhysicalPage.
	allocatedPages []uint64
	// released guards the exactly-once rollback.
	released bool
	// sbin_codex (todo 20): PreEvictions are the projected-occupancy
	// pre-eviction victims launched by the admission gate; the driving
	// middleware hands them to the eviction service. They are returned even
	// when the migration itself fails (frame shortage) so the victims are
	// never orphaned.
	PreEvictions []*evictionTransaction
}

// formMigrationRuns groups pages into maximal runs where consecutive pages
// have both contiguous source and contiguous destination physical addresses.
// The minimum accounting granularity is one 4 KB page (uvm-manager.md
// §23.1.2). // sbin_codex
func formMigrationRuns(pages []migrationPage) migrationPlan {
	plan := migrationPlan{}
	for _, p := range pages {
		plan.PageCount++
		plan.TotalBytes += basePageSize
		if n := len(plan.Runs); n > 0 {
			last := &plan.Runs[n-1]
			if p.SrcPA == last.SrcStart+last.Bytes &&
				p.DstPA == last.DstStart+last.Bytes {
				last.Pages = append(last.Pages, p.Page)
				last.Bytes += basePageSize
				continue
			}
		}
		plan.Runs = append(plan.Runs, migrationRun{
			SrcStart: p.SrcPA,
			DstStart: p.DstPA,
			Bytes:    basePageSize,
			Pages:    []uint64{p.Page},
		})
	}
	return plan
}

// migrationFrameAllocator allocates and frees raw GPU physical frames for
// migration destinations. The Driver implements it over its registered
// devices. // sbin_codex
type migrationFrameAllocator interface {
	allocateMigrationFrames(gpu int, count int) ([]uint64, error)
	freeMigrationFrames(frames []uint64)
}

// SetFrameAllocator installs the migration frame allocator (the driver).
// sbin_codex
func (m *UVMManager) SetFrameAllocator(fa migrationFrameAllocator) {
	m.Lock()
	defer m.Unlock()

	m.frames = fa
}

// allocateMigrationFrames allocates count contiguous GPU physical frames for
// a migration destination on GPU gpu (1-based). // sbin_codex
func (d *Driver) allocateMigrationFrames(gpu int, count int) ([]uint64, error) {
	if gpu < 1 || gpu >= len(d.devices) {
		return nil, fmt.Errorf("uvm: no device for GPU %d", gpu)
	}
	dev := d.devices[gpu]
	if dev.Type == internal.DeviceTypeUnifiedGPU {
		return nil, fmt.Errorf("uvm: unified GPU devices are not supported for migration frames")
	}
	return dev.AllocatePages(count), nil
}

// freeMigrationFrames returns migration destination frames to their device.
// sbin_codex
func (d *Driver) freeMigrationFrames(frames []uint64) {
	for _, f := range frames {
		devID := d.memAllocator.GetDeviceIDByPAddr(f)
		d.devices[devID].FreePage(f)
	}
}

// pageStateLocked returns the PageState of allocation page `page` of reg.
// The caller must hold the manager lock. // sbin_codex
func (m *UVMManager) pageStateLocked(
	reg *ManagedAllocationRegistration,
	page uint64,
) *PageState {
	blockIdx := (BlockForVA(reg.Base+page*basePageSize) -
		BlockForVA(reg.Base)) / vablockSizeBytes
	block := reg.VABlocks[blockIdx]
	blockLocal := (reg.Base + page*basePageSize - block.StartVA) / basePageSize
	return &block.Pages[blockLocal]
}

// prepareFaultMigration reserves the admission for the migrated bytes,
// allocates destination GPU frames for pages without a pre-assigned frame,
// marks the missing pages in flight, and forms the maximal runs. The
// reservation is the capacity gate: it happens BEFORE any destination frame
// allocation, and a failed reservation mutates nothing. // sbin_codex
// sbin_codex (todo 18): delegates to the generic page-based
// prepareMigrationPages so AC/write migration transactions reuse the same
// reservation -> destination frame allocation -> run formation.
func (m *UVMManager) prepareFaultMigration(
	tx *faultTransaction,
	missing []uint64,
) (*migrationPlan, error) {
	return m.prepareMigrationPages(tx.reg, tx.GPU, missing)
}

// prepareMigrationPages reserves the admission for the migrated bytes,
// allocates destination GPU frames for pages without a pre-assigned frame,
// marks the pages in flight, and forms the maximal runs. The reservation is
// the capacity gate: it happens BEFORE any destination frame allocation, and
// a failed reservation mutates nothing. // sbin_codex
// sbin_codex (todo 20): the projected-occupancy admission gate runs before
// the frame allocation: a hard capacity shortage returns an error (the
// admission queues) and the optional-headroom pre-eviction victims are
// launched; a frame shortage releases the reservation and returns the
// victims with the error so they are driven and never orphaned.
func (m *UVMManager) prepareMigrationPages(
	reg *ManagedAllocationRegistration,
	gpu int,
	pages []uint64,
) (*migrationPlan, error) {
	m.Lock()
	defer m.Unlock()

	if reg == nil {
		return nil, fmt.Errorf("uvm: migration without a registration")
	}
	bytes := uint64(len(pages)) * basePageSize
	// sbin_codex (todo 20): the projected-occupancy admission gate — reserve
	// the admission (N += bytes) and launch the pre-eviction victims for the
	// 64 KB headroom; a hard capacity shortage queues the admission.
	victims, err := m.admitWithPreEvictionLocked(reg.PID, gpu, bytes)
	if err != nil {
		return &migrationPlan{PreEvictions: victims}, err
	}

	// Allocate destination GPU frames for pages without a pre-assigned frame.
	need := 0
	for _, page := range pages {
		if m.pageStateLocked(reg, page).GPUPhysicalPage == 0 {
			need++
		}
	}
	var frames []uint64
	if need > 0 {
		if m.frames == nil {
			m.reservation.ReleaseAdmission(bytes)
			return &migrationPlan{PreEvictions: victims},
				fmt.Errorf("uvm: no migration frame allocator")
		}
		frames, err = m.frames.allocateMigrationFrames(gpu, need)
		if err != nil {
			m.reservation.ReleaseAdmission(bytes)
			return &migrationPlan{PreEvictions: victims}, err
		}
	}

	plan := &migrationPlan{frames: frames, PreEvictions: victims}
	allocated := make([]uint64, 0, need)
	mpages := make([]migrationPage, 0, len(pages))
	fi := 0
	for _, page := range pages {
		p := m.pageStateLocked(reg, page)
		if p.GPUPhysicalPage == 0 {
			p.GPUPhysicalPage = frames[fi]
			fi++
			allocated = append(allocated, page)
		}
		setMaskBit(reg.InFlightMask, page, true)
		mpages = append(mpages, migrationPage{
			Page:  page,
			SrcPA: p.CPUPhysicalPage,
			DstPA: p.GPUPhysicalPage,
		})
	}
	formed := formMigrationRuns(mpages)
	plan.Runs = formed.Runs
	plan.TotalBytes = formed.TotalBytes
	plan.PageCount = formed.PageCount
	plan.allocatedPages = allocated
	return plan, nil
}

// commitFaultMigration publishes GPU residency for the migrated pages and
// returns their (VA, HBM PA) pairs for the GPU PTE publication. It is called
// only after ALL runs of the migration succeeded. // sbin_codex
// sbin_codex (todo 18): delegates to the generic page-based
// commitMigrationPages so AC/write migration transactions reuse the same
// publish-after-all-runs commit.
func (m *UVMManager) commitFaultMigration(
	tx *faultTransaction,
) ([]faultMigratedPage, error) {
	return m.commitMigrationPages(tx.reg, tx.plan)
}

// commitMigrationPages publishes GPU residency for the migrated pages and
// returns their (VA, HBM PA) pairs for the GPU PTE publication. It is called
// only after ALL runs of the migration succeeded. // sbin_codex
func (m *UVMManager) commitMigrationPages(
	reg *ManagedAllocationRegistration,
	plan *migrationPlan,
) ([]faultMigratedPage, error) {
	m.Lock()
	defer m.Unlock()

	if reg == nil {
		return nil, fmt.Errorf("uvm: migration commit without a registration")
	}
	if plan == nil {
		return nil, fmt.Errorf("uvm: migration commit without a plan")
	}
	pages := make([]faultMigratedPage, 0, plan.PageCount)
	for _, run := range plan.Runs {
		for i, page := range run.Pages {
			setMaskBit(reg.ResidentMask, page, true)
			setMaskBit(reg.InFlightMask, page, false)
			pages = append(pages, faultMigratedPage{
				PageVA:  reg.Base + page*basePageSize,
				GPUPage: run.DstStart + uint64(i)*basePageSize,
			})
		}
	}
	return pages, nil
}

// rollbackFaultMigration releases the admission reservation and frees the
// destination frames of a failed migration exactly once, clears the in-flight
// marks, and resets the allocated destination frames so a retry re-allocates
// them. // sbin_codex
func (m *UVMManager) rollbackFaultMigration(
	tx *faultTransaction,
	plan *migrationPlan,
) {
	m.Lock()
	defer m.Unlock()

	if plan == nil || plan.released {
		return
	}
	plan.released = true

	reg := tx.reg
	if reg != nil {
		for _, run := range plan.Runs {
			for _, page := range run.Pages {
				setMaskBit(reg.InFlightMask, page, false)
			}
		}
		// sbin_codex (todo 17): a rolled-back TBN prefetch never published
		// the prefetched mark; clear it defensively so a retry re-accounts
		// the prefetch outcome exactly once (§11.12).
		for _, page := range tx.prefetchPages {
			setMaskBit(reg.PrefetchedMask, page, false)
		}
		for _, page := range plan.allocatedPages {
			m.pageStateLocked(reg, page).GPUPhysicalPage = 0
		}
	}
	if plan.frames != nil {
		if m.frames != nil {
			m.frames.freeMigrationFrames(plan.frames)
		}
		plan.frames = nil
	}
	m.reservation.ReleaseAdmission(plan.TotalBytes)
}

// prepareEvictionD2H forms the maximal D2H runs of one logical 64 KB
// eviction: every valid page of the region is copied (uvm-manager.md §18.3).
// The source is the resident GPU frame; the destination is the CPU backing
// frame. // sbin_codex
func (m *UVMManager) prepareEvictionD2H(
	reg *ManagedAllocationRegistration,
	gpu int,
	regionBase uint64,
) (*migrationPlan, error) {
	m.Lock()
	defer m.Unlock()

	blockIdx := (BlockForVA(regionBase) - BlockForVA(reg.Base)) / vablockSizeBytes
	block := reg.VABlocks[blockIdx]
	regionIdx := (regionBase - block.StartVA) / subblockSizeBytes
	allocStart, valid := (&InvariantContext{
		Reg: reg, Block: block, RegionIdx: regionIdx,
	}).regionPageRange()
	pages := make([]migrationPage, 0, valid)
	for i := uint64(0); i < valid; i++ {
		page := allocStart + i
		p := m.pageStateLocked(reg, page)
		if p.GPUPhysicalPage == 0 {
			return nil, fmt.Errorf(
				"uvm: eviction page %d has no GPU physical page", page)
		}
		pages = append(pages, migrationPage{
			Page:  page,
			SrcPA: p.GPUPhysicalPage,
			DstPA: p.CPUPhysicalPage,
		})
	}
	formed := formMigrationRuns(pages)
	return &migrationPlan{
		Runs:       formed.Runs,
		TotalBytes: formed.TotalBytes,
		PageCount:  formed.PageCount,
	}, nil
}

// evictionD2HRun pairs one D2H run with its superior request. // sbin_codex
type evictionD2HRun struct {
	run  migrationRun
	req  *protocol.MemCopyD2HReq
	done bool
}

// evictionD2HTransfer tracks the D2H runs of one logical 64 KB eviction to
// completion. Each run is one MemCopyD2HReq; when every run completes, each
// run's buffer is written to its CPU backing frames. The range WB+INV/TLB
// ordering is the eviction transaction (plan todo 19); this transfer provides
// the D2H run emission and completion. // sbin_codex
type evictionD2HTransfer struct {
	driver  *Driver
	runs    []evictionD2HRun
	pending int
}

// startEvictionD2H forms the maximal D2H runs of one logical 64 KB eviction
// and emits one MemCopyD2HReq per run. // sbin_codex
func (d *Driver) startEvictionD2H(
	reg *ManagedAllocationRegistration,
	gpu int,
	regionBase uint64,
) (*evictionD2HTransfer, error) {
	plan, err := d.uvm.prepareEvictionD2H(reg, gpu, regionBase)
	if err != nil {
		return nil, err
	}
	t := &evictionD2HTransfer{driver: d, pending: len(plan.Runs)}
	for _, run := range plan.Runs {
		req := protocol.NewMemCopyD2HReq(
			d.gpuPort, d.GPUs[gpu-1], run.SrcStart, make([]byte, run.Bytes))
		t.runs = append(t.runs, evictionD2HRun{run: run, req: req})
		d.requestsToSend = append(d.requestsToSend, req)
	}
	return t, nil
}

// processRsp completes one D2H run; a stray or duplicate completion is
// rejected. When every run has completed, each run's buffer is written to its
// CPU backing frames. // sbin_codex
func (t *evictionD2HTransfer) processRsp(rsp *sim.GeneralRsp) bool {
	req, ok := rsp.OriginalReq.(*protocol.MemCopyD2HReq)
	if !ok {
		return false
	}
	for i := range t.runs {
		if t.runs[i].req == req && !t.runs[i].done {
			t.runs[i].done = true
			t.pending--
			if t.pending == 0 {
				t.writeback()
			}
			return true
		}
	}
	return false
}

// writeback writes every completed run's buffer to its CPU backing frames.
// sbin_codex
func (t *evictionD2HTransfer) writeback() {
	for _, r := range t.runs {
		if r.done {
			t.driver.globalStorage.Write(r.run.DstStart, r.req.DstBuffer)
		}
	}
}
