# sbin_claude: build both BFS traversal models into one code object.
#   bfs_data_warp_centric.cl -> bfs_data_warp_centric_kernel (previous model)
#   bfs_topo_frontier.cl     -> bfs_frontier_expand_kernel
#                               bfs_frontier_compact_kernel  (current default)
CLANG_OCL   ?= /opt/rocm/bin/clang-ocl
GPU_ARCH    ?= gfx803
DOCKER_IMAGE = rocm/dev-ubuntu-20.04:3.9
DOCKER_RUN   = docker run --rm -v $(PWD):/work -w /work $(DOCKER_IMAGE)

SOURCES = native/bfs_data_warp_centric.cl native/bfs_topo_frontier.cl

all: kernels.hsaco

# clang-ocl keeps only the last input when handed several .cl files, so the
# sources are concatenated into one translation unit first. // sbin_claude
kernels.hsaco: $(SOURCES)
	cat $(SOURCES) > native/_combined.cl
	$(DOCKER_RUN) $(CLANG_OCL) -mcpu=$(GPU_ARCH) -o $@ native/_combined.cl
	rm -f -- native/_combined.cl "-.o" *.orig.bc

disasm: kernels.hsaco
	$(DOCKER_RUN) /opt/rocm/llvm/bin/llvm-objdump \
		--disassemble --mcpu=$(GPU_ARCH) kernels.hsaco > kernels.disasm

# clang-ocl leaves intermediates (-.o, *.orig.bc) in the working directory.
clean-temps:
	rm -f -- "-.o" *.orig.bc native/_combined.cl

clean: clean-temps
	rm -f kernels.hsaco kernels.disasm
