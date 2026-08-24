package driver

// sbin_codex: UVMManager generation contract test (plan todo 10 of
// mgpusim-uvm-manager). The generation counter is the "generation" source the
// virtual access gates consume: the manager increments it before publications,
// and the gates stamp it into GPU_LOCAL annotations so stale retries can be
// detected.

import "testing"

// TestUVMGenerationIncrementsBeforePublications proves the manager's
// generation starts at zero, increments monotonically, and the increment API
// returns the new generation that gates must stamp into annotations.
func TestUVMGenerationIncrementsBeforePublications(t *testing.T) {
	cfg := DefaultUVMConfig()
	cfg.Enabled = true
	manager := NewUVMManager(cfg, 1<<30)

	if got := manager.Generation(); got != 0 {
		t.Fatalf("the generation must start at 0, got %d", got)
	}

	// The manager increments before a publication: the increment returns the
	// generation the publication's annotations must carry.
	if got := manager.IncrementGeneration(); got != 1 {
		t.Fatalf("the first increment must return generation 1, got %d", got)
	}
	if got := manager.Generation(); got != 1 {
		t.Fatalf("the generation must read 1 after the increment, got %d", got)
	}
	if got := manager.IncrementGeneration(); got != 2 {
		t.Fatalf("the second increment must return generation 2, got %d", got)
	}
}
