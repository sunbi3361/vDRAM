package r9nano

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("R9 Nano topology validation", func() { // sbin_codex
	DescribeTable("rejects an incompatible pair before component construction",
		func(dataPath DataPathTopology, memory MemoryTopology) {
			// Given
			testSimulation, _, _ := newR9NanoTestSimulation("invalid-topology")
			componentCount := len(testSimulation.Components())
			builder := MakeBuilder().
				WithSimulation(testSimulation).
				WithDataPathTopology(dataPath).
				WithMemoryTopology(memory)

			// When / Then
			Expect(func() {
				builder.Build("GPU")
			}).To(PanicWith("r9nano: data-path and memory topologies must both be baseline or both be virtual"))
			Expect(testSimulation.Components()).To(HaveLen(componentCount))
		},
		Entry("baseline data path with virtual memory", NewBaselineDataPathTopology(), NewVirtualMemoryTopology()),
		Entry("virtual data path with baseline memory", NewVirtualDataPathTopology(), NewBaselineMemoryTopology()),
		Entry("nil data path", nil, NewBaselineMemoryTopology()),
		Entry("nil memory", NewBaselineDataPathTopology(), nil),
	)
})
