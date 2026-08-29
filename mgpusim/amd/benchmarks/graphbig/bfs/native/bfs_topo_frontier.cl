// sbin_claude: GraphBIG BFS, topology-driven frontier model.
//
// Port of gpu_bench/gpu_BFS/bfs_topo_frontier.cu from github.com/graphbig/graphBIG
// ("Topological-Driven: one node per thread, no atomic instructions",
// Harish & Narayanan, HiPC 2007). It replaces the warp-centric kernel in
// bfs_data_warp_centric.cl, which left the page-walk unit idle: a warp there
// walks ONE adjacency list cooperatively, so its 64 lanes read consecutive
// edges[] words and every level degenerates into a sequential scan of vplist[]
// and offsets[]. Measured on the R-MAT dataset that gave 0.17 L2 TLB MPKI,
// 12.4 walks/us and no ideal-l1tlb gain (0.95x).
//
// Here each thread owns one vertex, so the 64 lanes of a wavefront walk 64
// unrelated adjacency lists and the neighbour probes (vplist[dst],
// updating[dst]) scatter across the whole vertex range.
//
// Three deviations from upstream, all documented:
//   1. Upstream keeps a separate visited[] array; vplist[dst] == INF is the
//      same predicate and saves a buffer, matching what the warp-centric
//      kernel already did.
//   2. Upstream clears frontier[] in kernel 1; clearing it in kernel 2 instead
//      (frontier = updating) makes that store unconditional and keeps the
//      control flow flat.
//   3. frontier[]/updating[] are uint, not upstream's bool. A byte compare
//      lowers to v_cmp_eq_u16 / v_cmp_ne_u16 (VOPC 0xAA/0xAD), which the
//      simulator does not implement. The cost is 4 bytes per vertex per array
//      instead of 1; the access pattern, which is what this model is about,
//      is unchanged.
//
// Control-flow note: the loop below has a per-lane trip count, and clang
// lowers the EXEC-mask merge at its tail to s_orn2_b64 (SOP2 opcode 21). That
// instruction was missing from the emulator; it is implemented now (see
// emu/alusop2.go). Every reformulation tried -- uint vs uchar flags, using
// vplist as the frontier marker, a branchless inner body -- emitted it too, so
// implementing it was the fix rather than working around it. The kernels still
// avoid `return`/`break`/`continue` inside loops and keep exactly one
// leaf-level `if`, which is what bfs_data_warp_centric.cl warns about.

// Guarded: this file is concatenated with bfs_data_warp_centric.cl into one
// translation unit, and clang-ocl keeps only the last input file when given
// several. // sbin_claude
#ifndef INF
#define INF 0xffffffffu
#endif

// Kernel 1 -- expand. One vertex per thread; only frontier vertices traverse.
__kernel void bfs_frontier_expand_kernel(
    __global uint*        vplist,
    __global const uint*  offsets,
    __global const uint*  edges,
    __global const uint*  frontier,
    __global uint*        updating,
    __global const uint*  state)   // [0]=changed, [1]=curr_level, [2]=num_nodes
{
    uint tid       = (uint)get_global_id(0);
    uint num_nodes = state[2];

    // Clamp instead of returning early: vertex 0 always exists, so the loads
    // below stay in bounds and out-of-range lanes simply fall out via num_nbr.
    uint in_range = (tid < num_nodes) ? 1u : 0u;
    uint idx      = in_range ? tid : 0u;

    uint fr       = frontier[idx];
    uint active   = (in_range && (fr != 0u)) ? 1u : 0u;
    uint nbr_off  = offsets[idx];
    uint nbr_end  = offsets[idx + 1u];
    uint num_nbr  = active ? (nbr_end - nbr_off) : 0u;
    uint next_lvl = vplist[idx] + 1u;

    for (uint k = 0u; k < num_nbr; k++) {
        uint dst = edges[nbr_off + k];
        if (vplist[dst] == INF) {
            // Benign race: every thread that discovers dst at this level
            // writes the same next_lvl, so no atomic is needed.
            vplist[dst]   = next_lvl;
            updating[dst] = 1u;
        }
    }
}

// Kernel 2 -- compact. Swap updating[] into frontier[] and report progress.
__kernel void bfs_frontier_compact_kernel(
    __global uint* frontier,
    __global uint* updating,
    __global uint* state)         // [0]=changed
{
    uint tid       = (uint)get_global_id(0);
    uint num_nodes = state[2];

    uint in_range = (tid < num_nodes) ? 1u : 0u;
    uint idx      = in_range ? tid : 0u;

    uint u = updating[idx];
    // Every lane that maps to idx -- the owner plus any clamped out-of-range
    // lane -- stores the same two values here, so the stores stay unconditional
    // and the duplication is benign. Reading `u` first is what makes that true:
    // a clamped lane writes updating[0]'s value into frontier[0], exactly what
    // thread 0 writes.
    frontier[idx] = u;
    updating[idx] = 0u;

    if (in_range && (u != 0u)) {
        state[0] = 1u;
    }
}
