package emu

import (
	"log"
)

func (u *ALUImpl) runSOPK(state InstEmuState) {
	inst := state.Inst()
	switch inst.Opcode {
	case 0:
		u.runSMOVKI32(state)
	case 1:
		u.runSCMOVKI32(state)
	case 2:
		u.runSCMPKEQI32(state)
	case 3:
		u.runSCMPKLGI32(state)
	// sbin_claude: opcodes 4-14 ported from vdram_v2's GCN3 ALU. Without
	// them the benchmarks ported from that workspace (gups is the first to
	// hit it, on S_CMPK_LT_U32) panic in the scalar unit.
	case 4:
		u.runSCMPKGTI32(state)
	case 5:
		u.runSCMPKGEI32(state)
	case 6:
		u.runSCMPKLTI32(state)
	case 7:
		u.runSCMPKLEI32(state)
	case 8:
		u.runSCMPKEQU32(state)
	case 9:
		u.runSCMPKLGU32(state)
	case 10:
		u.runSCMPKGTU32(state)
	case 11:
		u.runSCMPKGEU32(state)
	case 12:
		u.runSCMPKLTU32(state)
	case 13:
		u.runSCMPKLEU32(state)
	case 14:
		u.runSADDKI32(state)
	case 15:
		u.runSMULKI32(state)
	default:
		log.Panicf("Opcode %d for SOPK format is not implemented", inst.Opcode)
	}
}

func (u *ALUImpl) runSMOVKI32(state InstEmuState) {
	inst := state.Inst()
	imm := asInt16(uint16(state.ReadOperand(inst.SImm16, 0) & 0xffff))
	state.WriteOperand(inst.Dst, 0, uint64(imm))
}

func (u *ALUImpl) runSCMOVKI32(state InstEmuState) {
	inst := state.Inst()
	if state.SCC() == 1 {
		imm := asInt16(uint16(state.ReadOperand(inst.SImm16, 0) & 0xffff))
		state.WriteOperand(inst.Dst, 0, uint64(imm))
	}
}

func (u *ALUImpl) runSCMPKEQI32(state InstEmuState) {
	inst := state.Inst()
	imm := asInt16(uint16(state.ReadOperand(inst.SImm16, 0) & 0xffff))
	dst := state.ReadOperand(inst.Dst, 0)
	if asInt16(uint16(dst)) == imm {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPKLGI32(state InstEmuState) {
	inst := state.Inst()
	imm := asInt16(uint16(state.ReadOperand(inst.SImm16, 0) & 0xffff))
	dst := state.ReadOperand(inst.Dst, 0)
	if asInt16(uint16(dst)) != imm {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSMULKI32(state InstEmuState) {
	inst := state.Inst()
	imm := asInt16(uint16(state.ReadOperand(inst.SImm16, 0) & 0xffff))
	dst := asInt32(uint32(state.ReadOperand(inst.Dst, 0)))

	state.WriteOperand(inst.Dst, 0, int64ToBits(int64(int32(imm)*dst)))
}

// sbin_claude: SOPK opcodes 4-14, ported from vdram_v2's GCN3 ALU.

func (u *ALUImpl) runSCMPKGTI32(state InstEmuState) {
	inst := state.Inst()
	imm := asInt16(uint16(state.ReadOperand(inst.SImm16, 0) & 0xffff))
	dst := asInt32(uint32(state.ReadOperand(inst.Dst, 0)))
	if dst > int32(imm) {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPKGEI32(state InstEmuState) {
	inst := state.Inst()
	imm := asInt16(uint16(state.ReadOperand(inst.SImm16, 0) & 0xffff))
	dst := asInt32(uint32(state.ReadOperand(inst.Dst, 0)))
	if dst >= int32(imm) {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPKLTI32(state InstEmuState) {
	inst := state.Inst()
	imm := asInt16(uint16(state.ReadOperand(inst.SImm16, 0) & 0xffff))
	dst := asInt32(uint32(state.ReadOperand(inst.Dst, 0)))
	if dst < int32(imm) {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPKLEI32(state InstEmuState) {
	inst := state.Inst()
	imm := asInt16(uint16(state.ReadOperand(inst.SImm16, 0) & 0xffff))
	dst := asInt32(uint32(state.ReadOperand(inst.Dst, 0)))
	if dst <= int32(imm) {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPKEQU32(state InstEmuState) {
	inst := state.Inst()
	imm := uint32(state.ReadOperand(inst.SImm16, 0) & 0xffff)
	dst := uint32(state.ReadOperand(inst.Dst, 0))
	if dst == imm {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPKLGU32(state InstEmuState) {
	inst := state.Inst()
	imm := uint32(state.ReadOperand(inst.SImm16, 0) & 0xffff)
	dst := uint32(state.ReadOperand(inst.Dst, 0))
	if dst != imm {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPKGTU32(state InstEmuState) {
	inst := state.Inst()
	imm := uint32(state.ReadOperand(inst.SImm16, 0) & 0xffff)
	dst := uint32(state.ReadOperand(inst.Dst, 0))
	if dst > imm {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPKGEU32(state InstEmuState) {
	inst := state.Inst()
	imm := uint32(state.ReadOperand(inst.SImm16, 0) & 0xffff)
	dst := uint32(state.ReadOperand(inst.Dst, 0))
	if dst >= imm {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPKLTU32(state InstEmuState) {
	inst := state.Inst()
	imm := uint32(state.ReadOperand(inst.SImm16, 0) & 0xffff)
	dst := uint32(state.ReadOperand(inst.Dst, 0))
	if dst < imm {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPKLEU32(state InstEmuState) {
	inst := state.Inst()
	imm := uint32(state.ReadOperand(inst.SImm16, 0) & 0xffff)
	dst := uint32(state.ReadOperand(inst.Dst, 0))
	if dst <= imm {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSADDKI32(state InstEmuState) {
	inst := state.Inst()
	imm := asInt16(uint16(state.ReadOperand(inst.SImm16, 0) & 0xffff))
	dst := asInt32(uint32(state.ReadOperand(inst.Dst, 0)))
	state.WriteOperand(inst.Dst, 0, int64ToBits(int64(dst+int32(imm))))
}
