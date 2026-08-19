# EMU KNOWLEDGE BASE

## OVERVIEW

Functional ISA emulator for AMD GPU kernels. `ComputeUnit` runs wavefronts instruction-by-instruction without pipeline modeling. `ALU` is the pluggable execution engine. GCN3 is the stable baseline; CDNA3 is a separate architecture variant. All instruction decoding and disassembly is shared from `amd/insts`.

## STRUCTURE

- `emu/`: core emulator. `ALUImpl` is the GCN3 ALU implementation.
- `emu/gcn3/`: GCN3 ALU alias to `emu.ALUImpl`.
- `emu/cdna3/`: separate CDNA3 ALU implementation with its own opcode handlers.
- `emu/wavefront.go`: per-wavefront register file and `InstEmuState` implementation.
- `emu/computeunit.go`: work-group dispatch, instruction cache, barrier resolution.
- `emu/isadebugger.go`: per-instruction JSON trace hook.
- `amd/insts/`: shared instruction definitions, `FormatTable`, `Disassembler`, `InstPrinter`.

## WHERE TO LOOK

- `InstEmuState` contract: `emu/inst.go`.
- Format dispatch: `emu/alu.go` `Run()` and `cdna3/alu.go` `Run()`.
- Opcode handlers: `emu/alu*.go`, `cdna3/*.go`.
- Register read/write and lane model: `emu/wavefront.go`.
- Wavefront setup and PC/barrier loop: `emu/computeunit.go`.
- Shared decoder/disassembler: `amd/insts/disassembler.go`, `amd/insts/decodetable.go`.
- ISA trace logging: `emu/isadebugger.go`.

## CONVENTIONS

- ALU implementations implement the `emu.ALU` interface: `Run`, `SetLDS`, `LDS`, `ArchName`.
- `InstEmuState` separates execution state from the ALU so tests can use mocks.
- `ComputeUnit` caches decoded instructions by PC; do not mutate cached `insts.Inst` objects.
- Use `insts.NewInstPrinter(nil)` for disassembly in hooks and diagnostics.

## ANTI-PATTERNS

- Do not treat `cdna3/` as a reference fallback; it is a separate implementation with its own semantics and bugs.
- Do not add an opcode without auditing both architecture trees; shared operations may need parallel changes while architecture-specific semantics must remain distinct.
- Do not assume CDNA3 wavefront initialization matches GCN3; V5 code objects pack work-item IDs into `v0`.
- Do not call `decodetable.go` auto-generated; it is hand-maintained.

## TESTING

- Unit tests are in `*_test.go` next to each ALU file, using Ginkgo/Gomega mocks.
- Add matching tests in every architecture tree when you change instruction semantics.
- Run `cd mgpusim && go test ./amd/emu/... -v` before claiming an instruction fix is complete.
- Use `-verify` sample runs for end-to-end ISA correctness.
