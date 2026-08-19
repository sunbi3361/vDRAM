// Package idealtlb implements an ideal Translation Lookaside Buffer (TLB)
// that resolves every translation request directly from the page table with
// zero latency and unlimited concurrency. It is a drop-in replacement for the
// real TLB component: it exposes the same Top, Bottom, and Control ports and
// speaks the same vm.TranslationReq/TranslationRsp protocol. The Bottom port
// exists for wiring compatibility but is never used — the ideal TLB never
// misses. // sbin_codex
package idealtlb
