package emu

import (
	"log"
)

//nolint:gocyclo,funlen
func (u *ALUImpl) runSOPC(state InstEmuState) {
	inst := state.Inst()
	switch inst.Opcode {
	case 0:
		u.runSCMPEQU32(state)
	case 1:
		u.runSCMPLGU32(state)
	case 2:
		u.runSCMPGTI32(state)
	case 3:
		u.runSCMPGEI32(state)
	case 4:
		u.runSCMPLTI32(state)
	case 5:
		u.runSCMPLEI32(state)
	case 6:
		u.runSCMPEQU32(state)
	case 7:
		u.runSCMPLGU32(state)
	case 8:
		u.runSCMPGTU32(state)
	// sbin_claude: opcodes 9 and 11 ported from vdram_v2's GCN3 ALU.
	case 9:
		u.runSCMPGEU32(state)
	case 10:
		u.runSCMPLTU32(state)
	case 11:
		u.runSCMPLEU32(state)
	default:
		log.Panicf("Opcode %d for SOPC format is not implemented", inst.Opcode)
	}
}

func (u *ALUImpl) runSCMPGTI32(state InstEmuState) {
	inst := state.Inst()
	src0 := asInt32(uint32(state.ReadOperand(inst.Src0, 0)))
	src1 := asInt32(uint32(state.ReadOperand(inst.Src1, 0)))
	if src0 > src1 {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPLTI32(state InstEmuState) {
	inst := state.Inst()
	src0 := asInt32(uint32(state.ReadOperand(inst.Src0, 0)))
	src1 := asInt32(uint32(state.ReadOperand(inst.Src1, 0)))
	if src0 < src1 {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPLEI32(state InstEmuState) {
	inst := state.Inst()
	src0 := asInt32(uint32(state.ReadOperand(inst.Src0, 0)))
	src1 := asInt32(uint32(state.ReadOperand(inst.Src1, 0)))
	if src0 <= src1 {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPGEI32(state InstEmuState) {
	inst := state.Inst()
	src0 := asInt32(uint32(state.ReadOperand(inst.Src0, 0)))
	src1 := asInt32(uint32(state.ReadOperand(inst.Src1, 0)))
	if src0 >= src1 {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPEQU32(state InstEmuState) {
	inst := state.Inst()
	src0 := uint32(state.ReadOperand(inst.Src0, 0))
	src1 := uint32(state.ReadOperand(inst.Src1, 0))
	if src0 == src1 {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPLGU32(state InstEmuState) {
	inst := state.Inst()
	src0 := uint32(state.ReadOperand(inst.Src0, 0))
	src1 := uint32(state.ReadOperand(inst.Src1, 0))
	if src0 != src1 {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPGTU32(state InstEmuState) {
	inst := state.Inst()
	src0 := uint32(state.ReadOperand(inst.Src0, 0))
	src1 := uint32(state.ReadOperand(inst.Src1, 0))
	if src0 > src1 {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPLTU32(state InstEmuState) {
	inst := state.Inst()
	src0 := uint32(state.ReadOperand(inst.Src0, 0))
	src1 := uint32(state.ReadOperand(inst.Src1, 0))
	if src0 < src1 {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

// sbin_claude: S_CMP_GE_U32 / S_CMP_LE_U32, ported from vdram_v2.

func (u *ALUImpl) runSCMPGEU32(state InstEmuState) {
	inst := state.Inst()
	src0 := uint32(state.ReadOperand(inst.Src0, 0))
	src1 := uint32(state.ReadOperand(inst.Src1, 0))
	if src0 >= src1 {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}

func (u *ALUImpl) runSCMPLEU32(state InstEmuState) {
	inst := state.Inst()
	src0 := uint32(state.ReadOperand(inst.Src0, 0))
	src1 := uint32(state.ReadOperand(inst.Src1, 0))
	if src0 <= src1 {
		state.SetSCC(1)
	} else {
		state.SetSCC(0)
	}
}
