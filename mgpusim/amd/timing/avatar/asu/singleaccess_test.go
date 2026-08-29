// sbin_claude_avatar
package asu

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/meta"
)

// refs/avatar.md 5.3 and 5.6 give CAST no memory request of its own: the
// speculative access is the requester's data access, issued to
// "SpeculatedPPN || PageOffset", and its return both carries the data and
// validates the speculation. An ASU that fetches a sector itself charges
// every speculation a second full trip through L2/DRAM, so it must not own a
// port into the data network.
func TestASUIssuesNoMemoryTrafficOfItsOwn(t *testing.T) {
	c := buildTestASU()

	for _, port := range c.Ports() {
		if port == c.topPort || port == c.bottomPort {
			continue
		}

		t.Fatalf("ASU owns an unexpected port %q; the speculative access "+
			"is the requester's demand access", port.Name())
	}
}

// A confident prediction is only waiting on CAVA's decompress-and-compare,
// because the sector it rides on is fetched by the CU regardless.
func TestSpeculationWaitsOnlyForDecompressAndCompare(t *testing.T) {
	c := buildTestASU()
	c.validationLatency = 7
	m := &middleware{Comp: c}

	pid, vAddr := vm.PID(1), uint64(0x2000)
	offset := int64(testFrameBase) - int64(vAddr)
	// sbin_claude_avatar v4: the MOD is keyed by the instruction PC, so the
	// two training translations have to come from the same instruction.
	pc := uint64(0x1200)

	// Two matching translations take the MOD to the confidence threshold.
	mod := m.modOf("L1TLB")
	mod.train(pid, pc, offset)
	mod.train(pid, pc, offset)

	req := vm.TranslationReqBuilder{}.
		WithSrc("L1TLB").WithVAddr(vAddr).WithPID(pid).
		WithInstPC(pc). // sbin_claude_avatar v4
		Build()
	trans := transaction{req: req}

	predicted, confident := mod.predict(pid, pc)
	if !confident {
		t.Fatal("two matching translations must reach the threshold")
	}

	trans.specActive = true
	trans.specPAddr = uint64(int64(vAddr) + predicted)
	trans.specCountdown = true
	trans.specCycleLeft = c.validationLatency

	// The countdown is armed at admission, not after a fetch returns.
	for i := 0; i < c.validationLatency; i++ {
		if !trans.specActive {
			t.Fatalf("speculation resolved after %d of %d cycles",
				i, c.validationLatency)
		}

		m.advanceSpeculation(&trans)
	}

	m.advanceSpeculation(&trans)

	if trans.specActive || trans.specCountdown {
		t.Fatal("speculation must resolve once the compare latency elapses")
	}
}

const testFrameBase = 0x40000000

func buildTestASU() *Comp {
	registry := meta.NewRegistry(12, 0.8, 1)
	pageTable := vm.NewPageTable(12)

	return MakeBuilder().
		WithRegistry(registry).
		WithPageTable(pageTable).
		WithMemoryRange(0, 1<<40).
		Build("ASU")
}
