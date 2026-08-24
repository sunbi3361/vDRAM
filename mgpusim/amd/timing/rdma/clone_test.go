package rdma

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
)

// sbin_codex: RDMA cloning must preserve the semantically relevant request
// fields so accesscounter can observe the original virtual demand after the
// request crosses the PCIe boundary.
var _ = Describe("cloneReq preserves request semantics", func() {
	var c *Comp

	BeforeEach(func() {
		c = &Comp{}
	})

	Context("marked read request", func() {
		It("retains PID, address, byte size, Info, coalescing, and RemoteDemandInfo",
			func() {
				// Given // sbin_codex
				info := mem.RemoteDemandInfo{
					PID:      vm.PID(7),
					VAddr:    0x12340,
					DeviceID: 3,
				}
				req := mem.ReadReqBuilder{}.
					WithAddress(0x100).
					WithByteSize(64).
					WithPID(vm.PID(9)).
					WithInfo("meta").
					CanWaitForCoalesce().
					WithRemoteDemandInfo(info).
					Build()

				// When // sbin_codex
				cloned := c.cloneReq(req).(*mem.ReadReq)

				// Then // sbin_codex
				Expect(cloned).NotTo(BeIdenticalTo(req))
				Expect(cloned.ID).NotTo(Equal(req.ID))
				Expect(cloned.Src).To(Equal(req.Src))
				Expect(cloned.Dst).To(Equal(req.Dst))
				Expect(cloned.Address).To(Equal(req.Address))
				Expect(cloned.AccessByteSize).To(Equal(req.AccessByteSize))
				Expect(cloned.PID).To(Equal(req.PID))
				Expect(cloned.Info).To(Equal(req.Info))
				Expect(cloned.CanWaitForCoalesce).To(BeTrue())
				Expect(cloned.RemoteDemandInfo).To(Equal(&info))
			})
	})

	Context("marked write request", func() {
		It("retains PID, address, data, dirty mask, Info, coalescing, and RemoteDemandInfo",
			func() {
				// Given // sbin_codex
				info := mem.RemoteDemandInfo{
					PID:      vm.PID(7),
					VAddr:    0x12340,
					DeviceID: 3,
				}
				data := []byte{1, 2, 3, 4}
				mask := []bool{true, false, true, false}
				req := mem.WriteReqBuilder{}.
					WithAddress(0x200).
					WithData(data).
					WithDirtyMask(mask).
					WithPID(vm.PID(9)).
					WithInfo("wmeta").
					CanWaitForCoalesce().
					WithRemoteDemandInfo(info).
					Build()

				// When // sbin_codex
				cloned := c.cloneReq(req).(*mem.WriteReq)

				// Then // sbin_codex
				Expect(cloned).NotTo(BeIdenticalTo(req))
				Expect(cloned.ID).NotTo(Equal(req.ID))
				Expect(cloned.Src).To(Equal(req.Src))
				Expect(cloned.Dst).To(Equal(req.Dst))
				Expect(cloned.Address).To(Equal(req.Address))
				Expect(cloned.Data).To(Equal(data))
				Expect(cloned.DirtyMask).To(Equal(mask))
				Expect(cloned.PID).To(Equal(req.PID))
				Expect(cloned.Info).To(Equal(req.Info))
				Expect(cloned.CanWaitForCoalesce).To(BeTrue())
				Expect(cloned.RemoteDemandInfo).To(Equal(&info))
			})
	})

	Context("unmarked requests", func() {
		It("keep RemoteDemandInfo nil and coalescing false for a read", func() {
			// Given // sbin_codex
			req := mem.ReadReqBuilder{}.
				WithAddress(0x100).
				WithByteSize(64).
				Build()

			// When // sbin_codex
			cloned := c.cloneReq(req).(*mem.ReadReq)

			// Then // sbin_codex
			Expect(cloned.RemoteDemandInfo).To(BeNil())
			Expect(cloned.CanWaitForCoalesce).To(BeFalse())
		})

		It("keep RemoteDemandInfo nil and coalescing false for a write", func() {
			// Given // sbin_codex
			req := mem.WriteReqBuilder{}.
				WithAddress(0x200).
				WithData([]byte{1, 2, 3, 4}).
				Build()

			// When // sbin_codex
			cloned := c.cloneReq(req).(*mem.WriteReq)

			// Then // sbin_codex
			Expect(cloned.RemoteDemandInfo).To(BeNil())
			Expect(cloned.CanWaitForCoalesce).To(BeFalse())
		})
	})
})
