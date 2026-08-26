// sbin_claude_avatar
package meta

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
)

const (
	testLog2PageSize = 12
	testPageSize     = uint64(1) << testLog2PageSize
	testBase         = uint64(0x1_0000_0000)
	testSize         = uint64(64) << 20 // 32 regions
)

func newTestRegistry(ratio float64) *Registry {
	r := NewRegistry(testLog2PageSize, ratio, 0x5b1c1a0)
	r.RegisterDevice(1, testBase, testSize, testPageSize)

	return r
}

func TestOffsetConstantWithinRegionDiffersAcrossRegions(t *testing.T) {
	r := newTestRegistry(1.0)
	pid := vm.PID(1)

	// Two pages of virtual region 0 share one physical region linearly.
	pA, okA := r.AllocateFrame(1, pid, 0x1000)
	pB, okB := r.AllocateFrame(1, pid, 0x2000)
	if !okA || !okB {
		t.Fatal("allocation failed")
	}
	if pB-pA != 0x1000 {
		t.Fatalf("offset not constant within region: %x vs %x", pA, pB)
	}

	// A page of another virtual region lands in a different physical region.
	pC, okC := r.AllocateFrame(1, pid, RegionBytes+0x1000)
	if !okC {
		t.Fatal("allocation failed")
	}
	offsetAB := pA - 0x1000
	offsetC := pC - (RegionBytes + 0x1000)
	if offsetAB == offsetC {
		t.Fatalf("offsets should differ across regions: %x", offsetAB)
	}
}

func TestAllocateFrameIsDeterministic(t *testing.T) {
	first := make([]uint64, 0)
	for run := 0; run < 2; run++ {
		r := newTestRegistry(1.0)
		got := make([]uint64, 0)
		for i := uint64(0); i < 8; i++ {
			p, ok := r.AllocateFrame(1, vm.PID(1), i*RegionBytes+0x1000)
			if !ok {
				t.Fatal("allocation failed")
			}
			got = append(got, p)
		}
		if run == 0 {
			first = got
			continue
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("run %d differs at %d: %x vs %x",
					run, i, got[i], first[i])
			}
		}
	}
}

func TestFreeFrameUnbindsEmptyRegion(t *testing.T) {
	r := newTestRegistry(1.0)
	pid := vm.PID(1)

	p, _ := r.AllocateFrame(1, pid, 0x1000)
	bound, _ := r.Occupancy(1)
	if bound != 1 {
		t.Fatalf("expected 1 bound region, got %d", bound)
	}

	if !r.FreeFrame(1, p) {
		t.Fatal("frame must be region-owned")
	}
	bound, free := r.Occupancy(1)
	if bound != 0 || free != int(testSize/RegionBytes) {
		t.Fatalf("region not returned: bound=%d free=%d", bound, free)
	}

	if r.FreeFrame(1, 0xdead000) {
		t.Fatal("unknown frame must not be region-owned")
	}
}

func TestCompressRatioConverges(t *testing.T) {
	r := newTestRegistry(0.8)

	compressible := 0
	const n = 10000
	for i := 0; i < n; i++ {
		if r.FrameCompressible(uint64(i) << testLog2PageSize) {
			compressible++
		}
	}

	got := float64(compressible) / n
	if got < 0.77 || got > 0.83 {
		t.Fatalf("compressible fraction %f too far from 0.8", got)
	}
}

func TestValidateVerdicts(t *testing.T) {
	r := newTestRegistry(1.0) // every frame compressible
	pid := vm.PID(1)
	pAddr := testBase + 4*testPageSize

	if v := r.Validate(pAddr, pid, 0x4000); v != VerdictNoMetadata {
		t.Fatalf("expected NoMetadata, got %v", v)
	}

	r.Install(pAddr, pid, 0x4000)
	if v := r.Validate(pAddr, pid, 0x4123); v != VerdictPass {
		t.Fatalf("expected Pass, got %v", v)
	}
	if v := r.Validate(pAddr, pid, 0x8000); v != VerdictMismatch {
		t.Fatalf("expected Mismatch, got %v", v)
	}
	if v := r.Validate(pAddr, vm.PID(2), 0x4000); v != VerdictMismatch {
		t.Fatalf("expected Mismatch on PID change, got %v", v)
	}

	// Migration: the old location must never validate again (refs 5.11).
	r.Invalidate(pAddr)
	if v := r.Validate(pAddr, pid, 0x4000); v != VerdictNoMetadata {
		t.Fatalf("stale metadata validated after invalidate: %v", v)
	}
}

func TestValidateIncompressible(t *testing.T) {
	r := NewRegistry(testLog2PageSize, 0.0, 42) // nothing compressible
	pid := vm.PID(1)
	pAddr := uint64(0x9000)

	r.Install(pAddr, pid, 0x4000)
	if v := r.Validate(pAddr, pid, 0x4000); v != VerdictIncompressible {
		t.Fatalf("expected Incompressible, got %v", v)
	}
}
