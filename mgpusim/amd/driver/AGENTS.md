# DRIVER KNOWLEDGE BASE
# branch: main | commit: bba8240 | generated: 2026-08-19

## OVERVIEW

The driver package is the host side of mgpusim. It owns contexts, command queues, memory allocation, kernel launch, and the middleware that turns API calls into messages for the command processor. driver.go is an Akita TickingComponent. Its Tick loop is the single integration point that drains queues, sends requests to GPUs, handles returns, and runs the migration state machine.

## STRUCTURE

Maintained source:
- api.go: public API. Init, SelectGPU, AllocateMemory, EnqueueMemCopy*, EnqueueLaunchKernel wrappers.
- driver.go: TickingComponent, Tick(), command dispatch, migration state machine.
- command.go / commandqueue.go: Command interface and per-context queues.
- builder.go: Builder wiring, middleware selection, port creation.
- internal/memoryallocator.go: physical allocation, page-table updates, migration support.
- middleware.go, memorycopy.go, memorycopyglobalstorage.go: copy middleware implementations.
- memorystats.go, internal/memorystats.go: allocation counters (live/peak/total pages).

## WHERE TO LOOK

- Launch flow: api.go EnqueueLaunchKernel -> commandqueue.go Enqueue -> Driver.Tick -> processNewCommand -> processLaunchKernelCommand -> protocol.NewLaunchKernelReq -> sendToGPUs -> CP. The CP later replies with LaunchKernelRsp; processReturnReq matches it to the pending command via findCommandByReqID, clears the request, and dequeues.
- Copy flow: api.go EnqueueMemCopy* -> commandqueue.go Enqueue -> Driver.Tick -> processCommandWithMiddleware. Default DMA sends MemCopyH2DReq/MemCopyD2HReq to GPUs and waits for responses. Magic copy writes directly to globalStorage and completes synchronously.
- Migration flow: parseFromMMU receives PageMigrationReqToDriver -> initiateRDMADrain -> sendShootDownReqs -> preparePageForMigration -> sendMigrationReqToCP -> processPageMigrationRspFromCP -> prepareGPURestartReqs -> handleGPURestartRsp -> prepareRDMARestartReqs.
- Allocation: api.go AllocateMemory -> internal/memoryallocator.go allocatePages. AllocateUnifiedMemory allocates on CPU (device 0).

## CONVENTIONS

- GPU IDs are 1-based in the driver (device 0 is CPU).
- Command queues are per-context and execute commands in order. queue.IsRunning serializes the head command.
- Enqueue writes to a queue and wakes listeners. DrainCommandQueue blocks until the queue is empty.
- Driver.Run starts a goroutine that pauses the engine, calls TickLater, and runs the engine while commands are pending.

## ANTI-PATTERNS

- Do not treat generated mocks as source. mock_internal_test.go, mock_sim_test.go, mock_vm_test.go, and internal/mock_vm_test.go are produced by mockgen.
- Do not use MagicMemoryCopyMiddleware for timing-accurate simulations. It bypasses the memory system and writes directly to globalStorage, so it is only valid for fast functional tests.
- Do not assume a single page table. The allocator mirrors every mapping into the CPU page table and every registered GPU page table through insertPage, updatePage, and removePageFromTables.
- Do not break allocation stats. recordAllocation and recordFree update livePageCount, peakPageCount, and totalPageCount. Removing a page must also delete it from vAddrToPageMapping to keep accounting correct.

## TESTING

- Tests use Ginkgo/Gomega.
- Mock generation: from the `mgpusim` module root, `go generate ./amd/driver/...` creates mock_internal_test.go, mock_sim_test.go, mock_vm_test.go, and internal/mock_vm_test.go. Regenerate after interface changes.
- Focused tests:
  - driver_test.go: Tick, command processing, launch returns, migration response handling.
  - api_test.go: public API calls (memory copy, allocation, unified memory).
  - memorycopy_test.go: middleware copy behavior.
  - internal/memoryallocator_test.go: allocation, remap, free, page-table consistency.
  - internal/memoryallocator_stats_test.go: live/peak/total page counters.
  - internal/devicebuddymemstate_test.go and buddystructures_test.go: buddy allocator internals.
- Run: `cd mgpusim/amd/driver && ginkgo -r` or `go test ./...`.
