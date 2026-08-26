// sbin_claude_utopia
package rsw

import (
	"testing"

	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/timing/utopia/restseg"
)

func buildUTUWithAssoc(t *testing.T, assoc int) *Comp {
	t.Helper()

	registry := restseg.NewRegistry()
	registry.AddSegment(restseg.MakeConfig(
		1, 0x1000_0000, 1<<20, 4096, assoc))

	engine := sim.NewSerialEngine()

	return MakeBuilder().
		WithEngine(engine).
		WithDeviceID(1).
		WithRegistry(registry).
		Build("UTU")
}

// TestTARPackingDerivedFromAssociativity checks that one 64B TAR line holds
// lineBytes/(assoc*4B) RestSeg sets: the paper-faithful ~4B-per-way entry,
// not a fixed multi-set packing.
func TestTARPackingDerivedFromAssociativity(t *testing.T) {
	cases := []struct {
		assoc        int
		wantSetsLine int
	}{
		{assoc: 16, wantSetsLine: 1}, // 16 ways x 4B = 64B: one set per line
		{assoc: 8, wantSetsLine: 2},
		{assoc: 4, wantSetsLine: 4},
		{assoc: 32, wantSetsLine: 1}, // wider than a line: floor at one set
	}

	for _, c := range cases {
		utu := buildUTUWithAssoc(t, c.assoc)
		utu.segmentConfigs()

		if got := utu.tarCache.entriesPerLine; got != c.wantSetsLine {
			t.Errorf("assoc %d: TAR sets per line = %d, want %d",
				c.assoc, got, c.wantSetsLine)
		}
	}
}

// TestDefaultCacheSizes locks the iso-capacity defaults: the TAR cache
// matches the baseline GMMU page-walk cache storage (128 entries x 4 cached
// levels x 8B = 4KB) and the SF cache keeps the paper's 2KB.
func TestDefaultCacheSizes(t *testing.T) {
	b := MakeBuilder()

	if b.tarCacheBytes != 4096 {
		t.Errorf("default TAR cache = %dB, want 4096B (baseline PWC size)",
			b.tarCacheBytes)
	}

	if b.sfCacheBytes != 2048 {
		t.Errorf("default SF cache = %dB, want 2048B", b.sfCacheBytes)
	}
}
