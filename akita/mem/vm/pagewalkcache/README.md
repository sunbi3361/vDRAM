# pagewalkcache

`pagewalkcache` is a GMMU-facing, non-blocking cache for page-table walk
segments. // sbin_codex

Its protocol is intentionally different from `mmuCache`:

1. GMMU sends one `LookupReq` containing `(PID, VAddr)`.
2. The cache probes its independent level 4, 3, 2, and 1 banks in parallel.
3. The cache returns a `LookupRsp` with `Hit` and the deepest hit `Level`.
4. A miss is returned immediately and does not reserve a miss entry or wait
   for memory.
5. GMMU sends a `FillReq` after it completes the walk. The fill updates the
   cache and produces no response. Level 0 is never cached.

Each level is fully associative and has the same number of entries. The key is
the cumulative upper prefix of the virtual page number: level 4 uses 9 bits,
level 3 uses 18 bits, level 2 uses 27 bits, and level 1 uses 36 bits.

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
