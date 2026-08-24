package cache

// sbin_codex: virtual-caching typed request annotation (plan todo 10 of
// mgpusim-uvm-manager). Virtual L1V/L1S access gates stamp this annotation on
// every GPU_LOCAL request admitted to the cache: the cache keeps the virtual
// tag (the request address stays the VA) and the annotation carries the
// resolved metadata (PID, VA page, HBM PA, location, generation). The cache
// clone sites persist the annotation through the MSHR and the block so
// refill, replacement, and range operations can validate stored mappings.
// CPU_REMOTE requests are never annotated (their CPU PA is routed only to the
// remote endpoint, uvm-manager.md §13.2) and INVALID requests carry no PA.

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
)

// VirtualAccessAnnotation is the typed metadata a virtual access gate attaches
// to a GPU_LOCAL request before cache admission. VAPage is the page-aligned
// virtual address (the virtual tag base); HBMPA is the HBM physical address
// the page currently maps to; Generation is the UVM manager generation the
// gate observed at admission, so a stale annotation can be detected after a
// publication.
type VirtualAccessAnnotation struct {
	PID        vm.PID
	VAPage     uint64
	HBMPA      uint64
	Location   vm.MemoryLocation
	Generation uint64
}

// ResolveAnnotation returns the VirtualAccessAnnotation carried by an access
// request, or nil when the request is not annotated (unmanaged traffic, L1I,
// or a disabled gate).
func ResolveAnnotation(req mem.AccessReq) *VirtualAccessAnnotation {
	switch r := req.(type) {
	case *mem.ReadReq:
		if ann, ok := r.Info.(*VirtualAccessAnnotation); ok {
			return ann
		}
	case *mem.WriteReq:
		if ann, ok := r.Info.(*VirtualAccessAnnotation); ok {
			return ann
		}
	}
	return nil
}

// Annotate attaches the annotation to an access request. A nil annotation
// clears the Info slot, which is a no-op for requests that never carried one.
func Annotate(req mem.AccessReq, ann *VirtualAccessAnnotation) {
	switch r := req.(type) {
	case *mem.ReadReq:
		r.Info = ann
	case *mem.WriteReq:
		r.Info = ann
	}
}

// IsStale reports whether the annotation's generation differs from the given
// current generation. A request whose annotation is stale must retry its
// probe because the mapping may have changed since admission.
func (a *VirtualAccessAnnotation) IsStale(current uint64) bool {
	return a != nil && a.Generation != current
}
