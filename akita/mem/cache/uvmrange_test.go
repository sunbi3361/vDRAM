package cache

// sbin_codex: contract tests for the scoped UVM range writeback/invalidation
// framework (plan todo 13 of mgpusim-uvm-manager). Written first (RED), then
// made to pass (GREEN) by implementing akita/mem/cache/uvmrange.go.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
)

// TestUVMRangeRejectMalformed proves structurally malformed range flush
// commands are rejected without any side effect.
func TestUVMRangeRejectMalformed(t *testing.T) {
	cases := []struct {
		name string
		req  *UVMCacheRangeFlushReq
	}{
		{
			"unknown operation",
			&UVMCacheRangeFlushReq{
				Operation:     99,
				VABase:        0x10000,
				ValidPageMask: 1,
				PhysicalRuns:  []PhysicalRun{{Start: 0x2000, Length: 0x1000}},
			},
		},
		{
			"unaligned VABase",
			&UVMCacheRangeFlushReq{
				Operation:     UVMCacheRangeFlushWritebackInvalidate,
				VABase:        0x10004,
				ValidPageMask: 1,
				PhysicalRuns:  []PhysicalRun{{Start: 0x2000, Length: 0x1000}},
			},
		},
		{
			"empty mask",
			&UVMCacheRangeFlushReq{
				Operation:    UVMCacheRangeFlushWritebackInvalidate,
				VABase:       0x10000,
				PhysicalRuns: []PhysicalRun{{Start: 0x2000, Length: 0x1000}},
			},
		},
		{
			"out-of-range mask",
			&UVMCacheRangeFlushReq{
				Operation:     UVMCacheRangeFlushWritebackInvalidate,
				VABase:        0x10000,
				ValidPageMask: 1 << 16,
				PhysicalRuns:  []PhysicalRun{{Start: 0x2000, Length: 0x1000}},
			},
		},
		{
			"no runs",
			&UVMCacheRangeFlushReq{
				Operation:     UVMCacheRangeFlushWritebackInvalidate,
				VABase:        0x10000,
				ValidPageMask: 1,
			},
		},
		{
			"empty run",
			&UVMCacheRangeFlushReq{
				Operation:     UVMCacheRangeFlushWritebackInvalidate,
				VABase:        0x10000,
				ValidPageMask: 1,
				PhysicalRuns:  []PhysicalRun{{Start: 0x2000, Length: 0}},
			},
		},
		{
			"unaligned run",
			&UVMCacheRangeFlushReq{
				Operation:     UVMCacheRangeFlushWritebackInvalidate,
				VABase:        0x10000,
				ValidPageMask: 1,
				PhysicalRuns:  []PhysicalRun{{Start: 0x2004, Length: 0x1000}},
			},
		},
		{
			"overlapping runs",
			&UVMCacheRangeFlushReq{
				Operation:     UVMCacheRangeFlushWritebackInvalidate,
				VABase:        0x10000,
				ValidPageMask: 0b11,
				PhysicalRuns: []PhysicalRun{
					{Start: 0x2000, Length: 0x2000},
					{Start: 0x3000, Length: 0x1000},
				},
			},
		},
		{
			"coverage mismatch",
			&UVMCacheRangeFlushReq{
				Operation:     UVMCacheRangeFlushWritebackInvalidate,
				VABase:        0x10000,
				ValidPageMask: 0b11,
				PhysicalRuns:  []PhysicalRun{{Start: 0x2000, Length: 0x1000}},
			},
		},
	}
	for _, c := range cases {
		if err := ValidateUVMCacheRangeFlushReq(c.req); err == nil {
			t.Errorf("%s: must be rejected", c.name)
		}
	}

	ok := &UVMCacheRangeFlushReq{
		Operation:     UVMCacheRangeFlushWritebackInvalidate,
		PID:           vm.PID(1),
		VABase:        0x10000,
		ValidPageMask: 0b11,
		PhysicalRuns:  []PhysicalRun{{Start: 0x2000, Length: 0x2000}},
	}
	if err := ValidateUVMCacheRangeFlushReq(ok); err != nil {
		t.Errorf("a well-formed request must pass validation: %v", err)
	}
}

// TestUVMRangeVirtualTag proves the virtual matcher selects lines by PID+VA
// and validates the stored annotation PA against the command's page mapping.
func TestUVMRangeVirtualTag(t *testing.T) {
	req := &UVMCacheRangeFlushReq{
		Operation:     UVMCacheRangeFlushWritebackInvalidate,
		PID:           vm.PID(1),
		VABase:        0x10000,
		ValidPageMask: 0b11,
		PhysicalRuns:  []PhysicalRun{{Start: 0x8000, Length: 0x2000}},
	}
	m := NewUVMRangeMatcher(req, true)

	ann := func(hbmPA uint64, generation uint64) *VirtualAccessAnnotation {
		return &VirtualAccessAnnotation{
			PID:        vm.PID(1),
			VAPage:     0x10000,
			HBMPA:      hbmPA,
			Location:   vm.MemoryLocationGPU_LOCAL,
			Generation: generation,
		}
	}

	match := &Block{PID: 1, Tag: 0x10040, IsValid: true, Annotation: ann(0x8000, 2)}
	if !m.MatchBlock(match) {
		t.Fatal("a PID+VA matching line with the stored PA must match")
	}

	wrongPID := &Block{PID: 2, Tag: 0x10040, IsValid: true, Annotation: ann(0x8000, 2)}
	if m.MatchBlock(wrongPID) {
		t.Fatal("a line of another PID must not match")
	}

	outside := &Block{PID: 1, Tag: 0x20040, IsValid: true, Annotation: ann(0x8000, 2)}
	if m.MatchBlock(outside) {
		t.Fatal("a line outside the VA range must not match")
	}

	invalidPage := &Block{PID: 1, Tag: 0x11040, IsValid: true, Annotation: ann(0x9000, 2)}
	if m.MatchBlock(invalidPage) {
		t.Fatal("a line in an invalid page must not match")
	}

	stalePA := &Block{PID: 1, Tag: 0x10040, IsValid: true, Annotation: ann(0x7000, 1)}
	if m.MatchBlock(stalePA) {
		t.Fatal("a line whose stored PA is not the command mapping must not match")
	}

	noAnn := &Block{PID: 1, Tag: 0x10040, IsValid: true}
	if m.MatchBlock(noAnn) {
		t.Fatal("an unannotated line must not match virtually")
	}

	mshr := &MSHREntry{PID: 1, Address: 0x10040, Annotation: ann(0x8000, 2)}
	if !m.MatchMSHR(mshr) {
		t.Fatal("a matching pending refill must match")
	}

	if pa, ok := m.PagePA(0x10040); !ok || pa != 0x8000 {
		t.Fatalf("PagePA(0x10040) = %#x, %v; want 0x8000, true", pa, ok)
	}
	if pa, ok := m.PagePA(0x12040); ok {
		t.Fatalf("PagePA of an invalid page must not be ok, got %#x", pa)
	}
}

// TestUVMRangePhysicalRuns proves the baseline matcher selects lines by
// physical runs only.
func TestUVMRangePhysicalRuns(t *testing.T) {
	req := &UVMCacheRangeFlushReq{
		Operation:     UVMCacheRangeFlushWritebackOnly,
		VABase:        0x10000,
		ValidPageMask: 0b11,
		PhysicalRuns:  []PhysicalRun{{Start: 0x8000, Length: 0x2000}},
	}
	m := NewUVMRangeMatcher(req, false)

	in := &Block{PID: 0, Tag: 0x8040, IsValid: true}
	if !m.MatchBlock(in) {
		t.Fatal("a line inside a physical run must match")
	}
	out := &Block{PID: 0, Tag: 0x10000, IsValid: true}
	if m.MatchBlock(out) {
		t.Fatal("a line outside every physical run must not match")
	}
	invalid := &Block{PID: 0, Tag: 0x8040}
	if m.MatchBlock(invalid) {
		t.Fatal("an invalid line must not match")
	}

	mshr := &MSHREntry{PID: 0, Address: 0x8000}
	if !m.MatchMSHR(mshr) {
		t.Fatal("a pending refill inside a physical run must match")
	}
	if !m.MatchAccess(0, 0x8000, nil) {
		t.Fatal("an in-flight access inside a physical run must match")
	}
	if !m.MatchEvictingTag(0x8040) {
		t.Fatal("an in-flight eviction inside a physical run must match")
	}
}
