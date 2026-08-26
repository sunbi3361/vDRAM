package rob

// sbin_claude_latpc: P1 plumbing tests. The ROB rebuilds requests
// field-by-field on the way down, so the LATPC translation hint must be
// carried over explicitly (refs/latpc-plan.md 2.2).

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
)

var _ = Describe("Reorder Buffer LATPC hint plumbing", func() {
	var (
		rob  *ReorderBuffer
		hint *vm.TranslationGroupHint
	)

	BeforeEach(func() {
		rob = MakeBuilder().
			WithBufferSize(10).
			Build("ROB")
		hint = &vm.TranslationGroupHint{
			GroupID:     "g1",
			StridePages: 4,
			Index:       1,
		}
	})

	It("should carry the hint through read duplication", func() {
		read := mem.ReadReqBuilder{}.
			WithAddress(0x40).
			WithByteSize(64).
			WithPID(1).
			WithTranslationHint(hint).
			Build()

		dup := rob.duplicateReadReq(read)

		Expect(dup.TranslationHint).To(BeIdenticalTo(hint))
	})

	It("should carry the hint through write duplication", func() {
		write := mem.WriteReqBuilder{}.
			WithAddress(0x40).
			WithPID(1).
			WithData(make([]byte, 64)).
			WithTranslationHint(hint).
			Build()

		dup := rob.duplicateWriteReq(write)

		Expect(dup.TranslationHint).To(BeIdenticalTo(hint))
	})

	It("should leave the hint nil when the original carries none", func() {
		read := mem.ReadReqBuilder{}.
			WithAddress(0x40).
			WithByteSize(64).
			WithPID(1).
			Build()

		dup := rob.duplicateReadReq(read)

		Expect(dup.TranslationHint).To(BeNil())
	})
})
