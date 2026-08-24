package mem

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/vm"
)

// sbin_codex: Remote-demand metadata has independent read and write coverage.
var _ = Describe("Remote demand request metadata", func() {
	info := RemoteDemandInfo{PID: vm.PID(7), VAddr: 0x12340, DeviceID: 3}

	It("preserves metadata when building and cloning a read request", func() {
		// Given // sbin_codex
		req := ReadReqBuilder{}.
			WithAddress(0x22340).
			WithByteSize(64).
			WithRemoteDemandInfo(info).
			Build()

		// When // sbin_codex
		clone := req.Clone().(*ReadReq)

		// Then // sbin_codex
		Expect(req.RemoteDemandInfo).To(Equal(&info))
		Expect(clone.RemoteDemandInfo).To(Equal(&info))
	})

	It("leaves read metadata absent by default", func() {
		// Given / When // sbin_codex
		req := ReadReqBuilder{}.Build()

		// Then // sbin_codex
		Expect(req.RemoteDemandInfo).To(BeNil())
	})

	It("preserves metadata when building and cloning a write request", func() {
		// Given // sbin_codex
		req := WriteReqBuilder{}.
			WithAddress(0x22340).
			WithData([]byte{1, 2, 3, 4}).
			WithRemoteDemandInfo(info).
			Build()

		// When // sbin_codex
		clone := req.Clone().(*WriteReq)

		// Then // sbin_codex
		Expect(req.RemoteDemandInfo).To(Equal(&info))
		Expect(clone.RemoteDemandInfo).To(Equal(&info))
	})

	It("leaves write metadata absent by default", func() {
		// Given / When // sbin_codex
		req := WriteReqBuilder{}.Build()

		// Then // sbin_codex
		Expect(req.RemoteDemandInfo).To(BeNil())
	})
})
