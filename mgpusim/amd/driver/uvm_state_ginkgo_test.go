package driver

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// sbin_codex: Ginkgo coverage for the UVM state model (plan todo 4). The QA
// command runs `ginkgo -r --focus='UVM state' ./amd/driver`, so the suite
// description below must contain "UVM state".
var _ = ginkgo.Describe("UVM state model", func() {
	var (
		region *SubBlockState
		sm     *RegionStateMachine
	)

	ginkgo.BeforeEach(func() {
		reg := buildTestRegistration(vm.PID(9), 4096, 512)
		region = reg.VABlocks[0].SubBlocks[0]
		sm = NewRegionStateMachine(
			RegionContext{PID: vm.PID(9), GPU: 1, Block: 0, Region: 0}, region)
	})

	ginkgo.It("UVM state follows the legal CPU to GPU to CPU lifecycle", func() {
		steps := []RegionState{
			RegionFaultPending,
			RegionMigratingToGPU,
			RegionGPUResident,
			RegionEvictPending,
			RegionMigratingToCPU,
			RegionCPUResident,
		}
		for _, to := range steps {
			gomega.Expect(sm.Transition(to, sim.VTimeInSec(100))).To(gomega.Succeed())
			gomega.Expect(region.State).To(gomega.Equal(to))
		}
		gomega.Expect(region.LastMigrationTime).To(gomega.Equal(sim.VTimeInSec(100)))
	})

	ginkgo.It("UVM state rejects a duplicate migration without mutating state", func() {
		gomega.Expect(sm.Transition(RegionMigratingToGPU, 10)).To(gomega.Succeed())
		err := sm.Transition(RegionMigratingToGPU, 20)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.(*TransitionError).Context.PID).To(gomega.Equal(vm.PID(9)))
		gomega.Expect(region.State).To(gomega.Equal(RegionMigratingToGPU))
	})

	ginkgo.It("UVM state coalesces a second fault while migrating to GPU", func() {
		gomega.Expect(sm.Transition(RegionMigratingToGPU, 10)).To(gomega.Succeed())
		gomega.Expect(sm.CoalesceFault()).To(gomega.Succeed())
		gomega.Expect(region.State).To(gomega.Equal(RegionMigratingToGPU))
	})

	ginkgo.It("UVM state stalls a GPU access while migrating to CPU", func() {
		for _, to := range []RegionState{
			RegionFaultPending, RegionMigratingToGPU, RegionGPUResident,
			RegionEvictPending, RegionMigratingToCPU,
		} {
			gomega.Expect(sm.Transition(to, 10)).To(gomega.Succeed())
		}
		gomega.Expect(sm.StallOnGPUAccess()).To(gomega.HaveOccurred())
		gomega.Expect(region.State).To(gomega.Equal(RegionMigratingToCPU))
	})
})
