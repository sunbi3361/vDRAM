package runner

import (
	"flag"
	"testing"
	"time"

	"github.com/sarchlab/akita/v4/mem/mem"
)

// sbin_codex: UVM flag presence contract tests (todo 1 of mgpusim-uvm-manager).
// These tests lock the §26 flag names, CLI defaults, and the config the runner
// builds from them.

func TestUVMFlagPresence(t *testing.T) {
	checks := []struct {
		name     string
		defValue string
	}{
		{"uvm", "false"},
		{"uvm-ideal", "false"},
		{"uvm-access-counter", "false"},
		{"uvm-fault-handling-latency", "20µs"},
		{"uvm-access-counter-threshold", "8"},
		{"uvm-vablock-size", "2097152"},
		{"uvm-tbn-min-node-size", "65536"},
		{"uvm-gpu-memory-capacity", "0"},
		{"uvm-prefetcher", "tbn"},
	}

	for _, c := range checks {
		f := flag.Lookup(c.name)
		if f == nil {
			t.Errorf("flag -%s is not registered", c.name)
			continue
		}
		if f.DefValue != c.defValue {
			t.Errorf("flag -%s default = %q, want %q", c.name, f.DefValue, c.defValue)
		}
	}

	// With no flags set, parseUVMConfig must yield the canonical §26 defaults.
	cfg := parseUVMConfig()
	if cfg.Enabled {
		t.Error("parseUVMConfig: Enabled must be false by default")
	}
	if cfg.Ideal {
		t.Error("parseUVMConfig: Ideal must be false by default")
	}
	if cfg.AccessCounter {
		t.Error("parseUVMConfig: AccessCounter must be false by default")
	}
	if cfg.FaultHandlingLatency != 20*time.Microsecond {
		t.Errorf("parseUVMConfig: FaultHandlingLatency = %v, want 20us",
			cfg.FaultHandlingLatency)
	}
	if cfg.AccessCounterThreshold != 8 {
		t.Errorf("parseUVMConfig: AccessCounterThreshold = %d, want 8",
			cfg.AccessCounterThreshold)
	}
	if cfg.VABlockSize != 2*mem.MB {
		t.Errorf("parseUVMConfig: VABlockSize = %d, want 2MB", cfg.VABlockSize)
	}
	if cfg.TBNMinNodeSize != 64*mem.KB {
		t.Errorf("parseUVMConfig: TBNMinNodeSize = %d, want 64KB",
			cfg.TBNMinNodeSize)
	}
	if cfg.Prefetcher != "tbn" {
		t.Errorf("parseUVMConfig: Prefetcher = %q, want tbn", cfg.Prefetcher)
	}
	if cfg.CapacitySet {
		t.Error("parseUVMConfig: CapacitySet must be false when -uvm-gpu-memory-capacity is not set")
	}
}
