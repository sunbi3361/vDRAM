package main

import (
	"slices"
	"testing"
)

func TestFIRVirtualCachingCase(t *testing.T) {
	t.Helper()

	var fir benchmark
	for _, candidate := range benchmarks {
		if candidate.executable == "fir" {
			fir = candidate
			break
		}
	}

	virtualCachingCases := make([]benchmarkCase, 0)
	for _, candidate := range fir.cases {
		if candidate.gpuType == "virtual-caching" {
			virtualCachingCases = append(virtualCachingCases, candidate)
		}
	}

	if len(virtualCachingCases) != 1 {
		t.Fatalf("expected one FIR virtual-caching case, got %d", len(virtualCachingCases))
	}

	got := virtualCachingCases[0]
	if !slices.Equal(got.gpus, []int{1}) || !got.timing || !got.parallel ||
		got.unifiedGPU || got.unifiedMemory || got.arch != "gcn3" {
		t.Fatalf("unexpected FIR virtual-caching case: %+v", got)
	}
}

func TestPopulateArgsForFIRVirtualCaching(t *testing.T) {
	t.Helper()

	benchmark := benchmark{executable: "fir"}
	caseConfig := benchmarkCase{
		gpus:          []int{1},
		timing:        true,
		parallel:      true,
		unifiedGPU:    false,
		unifiedMemory: false,
		arch:          "gcn3",
		gpuType:       "virtual-caching",
	}

	args := benchmark.populateArgs(caseConfig)
	expected := []string{
		"fir",
		"-verify",
		"--report-all",
		"-gpus=1",
		"-timing=true",
		"-parallel=true",
		"-use-unified-memory=false",
		"-arch=gcn3",
		"-gpu=virtual-caching",
	}

	if !slices.Equal(args, expected) {
		t.Fatalf("unexpected FIR virtual-caching arguments: got %v, want %v", args, expected)
	}
}
