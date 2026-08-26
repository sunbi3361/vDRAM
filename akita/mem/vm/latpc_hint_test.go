package vm

// sbin_claude_latpc: P1 plumbing tests for the LATPC group triple
// (refs/latpc-plan.md 2.1, 3).

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("TranslationReq LATPC group hint", func() {
	It("should keep zero group fields when no hint is given", func() {
		req := TranslationReqBuilder{}.
			WithPID(1).
			WithVAddr(0x1000).
			Build()

		Expect(req.GroupID).To(Equal(""))
		Expect(req.GroupStride).To(Equal(int64(0)))
		Expect(req.GroupIndex).To(Equal(0))
	})

	It("should keep zero group fields for a nil hint", func() {
		req := TranslationReqBuilder{}.
			WithPID(1).
			WithVAddr(0x1000).
			WithGroupHint(nil).
			Build()

		Expect(req.GroupID).To(Equal(""))
		Expect(req.GroupStride).To(Equal(int64(0)))
		Expect(req.GroupIndex).To(Equal(0))
	})

	It("should adopt the triple from a hint", func() {
		hint := &TranslationGroupHint{
			GroupID:     "g1",
			StridePages: -3,
			Index:       2,
		}

		req := TranslationReqBuilder{}.
			WithPID(1).
			WithVAddr(0x1000).
			WithGroupHint(hint).
			Build()

		Expect(req.GroupID).To(Equal("g1"))
		Expect(req.GroupStride).To(Equal(int64(-3)))
		Expect(req.GroupIndex).To(Equal(2))
	})

	It("should propagate the triple field-by-field with WithGroup", func() {
		req := TranslationReqBuilder{}.
			WithPID(1).
			WithVAddr(0x1000).
			WithGroup("g2", 4, 7).
			Build()

		Expect(req.GroupID).To(Equal("g2"))
		Expect(req.GroupStride).To(Equal(int64(4)))
		Expect(req.GroupIndex).To(Equal(7))
	})

	It("should retain the triple through Clone", func() {
		req := TranslationReqBuilder{}.
			WithPID(1).
			WithVAddr(0x1000).
			WithGroup("g3", 2, 5).
			Build()

		clone := req.Clone().(*TranslationReq)

		Expect(clone.GroupID).To(Equal("g3"))
		Expect(clone.GroupStride).To(Equal(int64(2)))
		Expect(clone.GroupIndex).To(Equal(5))
	})
})
