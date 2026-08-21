package r9nano

const incompatibleTopologyMessage = "r9nano: data-path and memory topologies must both be baseline or both be virtual" // sbin_codex

func (b Builder) validateTopologyPair() { // sbin_codex
	switch b.dataPathTopology.(type) {
	case baselineDataPathTopology:
		if _, compatible := b.memoryTopology.(baselineMemoryTopology); compatible {
			return
		}
	case virtualDataPathTopology:
		if _, compatible := b.memoryTopology.(virtualMemoryTopology); compatible {
			return
		}
	}

	panic(incompatibleTopologyMessage)
}
