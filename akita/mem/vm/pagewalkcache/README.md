# pagewalkcache

`pagewalkcache` is a GMMU-facing, non-blocking cache for page-table walk
segments.

Its protocol is intentionally different from `mmuCache`:

1. GMMU sends a `LookupReq` for one virtual-address/page-table level.
2. The cache reads `(PID, segment)` and returns a `LookupRsp` with `Hit`.
3. A miss is returned immediately and does not reserve a miss entry or wait
   for memory.
4. GMMU sends a `FillReq` after it completes the walk. The fill updates the
   cache and produces no response.

Lookup and fill messages share the bidirectional `Top` port so the cache can
be connected directly to a GMMU port.

```go
cache := pagewalkcache.MakeBuilder().
    WithEngine(engine).
    WithLog2PageSize(12).
    WithBitsPerLevel(9).
    WithLatency(1).
    Build("PageWalkCache")
```

`WithLatency` models the internal cache-read latency. It does not make misses
blocking: multiple lookup reads can be in flight up to
`WithMaxNumReqInFlight`.
