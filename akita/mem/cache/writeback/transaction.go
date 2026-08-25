package writeback

import (
	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

type action int

const (
	actionInvalid action = iota
	bankReadHit
	bankWriteHit
	bankEvict
	bankEvictAndWrite
	bankEvictAndFetch
	bankWriteFetched
	writeBufferFetch
	writeBufferEvictAndFetch
	writeBufferEvictAndWrite
	writeBufferFlush
)

type transaction struct {
	action

	id                string
	read              *mem.ReadReq
	write             *mem.WriteReq
	flush             *cache.FlushReq
	rangeFlush        *cache.RangeFlushReq // sbin_codex
	block             *cache.Block
	victim            *cache.Block
	fetchPID          vm.PID
	fetchAddress      uint64
	fetchedData       []byte
	fetchReadReq      *mem.ReadReq
	evictingPID       vm.PID
	evictingAddr      uint64
	evictingData      []byte
	evictingDirtyMask []bool
	evictionWriteReq  *mem.WriteReq
	mshrEntry         *cache.MSHREntry

	// rangeFlushID tags an eviction created by a UVM region flush so the
	// flusher can tell when its own write-backs have reached memory.
	// sbin_codex
	rangeFlushID string
}

func (t transaction) accessReq() mem.AccessReq {
	if t.read != nil {
		return t.read
	}

	if t.write != nil {
		return t.write
	}

	return nil
}

func (t transaction) req() sim.Msg {
	if t.accessReq() != nil {
		return t.accessReq()
	}

	if t.flush != nil {
		return t.flush
	}

	if t.rangeFlush != nil { // sbin_codex
		return t.rangeFlush
	}

	return nil
}
