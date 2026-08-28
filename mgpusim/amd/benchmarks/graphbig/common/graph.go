package common

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
)

// CSRGraph stores a directed graph in CSR format.
type CSRGraph struct {
	Offsets []uint32
	Edges   []uint32
}

// NumNodes returns number of vertices.
func (g CSRGraph) NumNodes() int {
	if len(g.Offsets) == 0 {
		return 0
	}
	return len(g.Offsets) - 1
}

// LoadCSR loads GraphBIG CSR files (uint64 entries on disk) and converts them to uint32.
func LoadCSR(datasetPath string) (CSRGraph, error) {
	vertexPath := filepath.Join(datasetPath, "vertex.CSR")
	edgePath := filepath.Join(datasetPath, "edge.CSR")

	off64, err := readUint64Array(vertexPath)
	if err != nil {
		return CSRGraph{}, err
	}
	edges64, err := readUint64Array(edgePath)
	if err != nil {
		return CSRGraph{}, err
	}

	off := make([]uint32, len(off64))
	for i, v := range off64 {
		off[i] = uint32(v)
	}
	edges := make([]uint32, len(edges64))
	for i, v := range edges64 {
		edges[i] = uint32(v)
	}

	return CSRGraph{Offsets: off, Edges: edges}, nil
}

// GenerateSynthetic creates a reproducible directed graph.
func GenerateSynthetic(numNodes, degree int, seed int64) CSRGraph {
	if numNodes < 1 {
		numNodes = 1
	}
	if degree < 1 {
		degree = 1
	}

	rng := rand.New(rand.NewSource(seed))
	off := make([]uint32, numNodes+1)
	edges := make([]uint32, 0, numNodes*degree)

	for v := 0; v < numNodes; v++ {
		off[v] = uint32(len(edges))
		for j := 0; j < degree; j++ {
			to := uint32(rng.Intn(numNodes))
			edges = append(edges, to)
		}
	}
	off[numNodes] = uint32(len(edges))

	return CSRGraph{Offsets: off, Edges: edges}
}

// BuildEdgeSrcArray builds source vertex id per edge index.
func BuildEdgeSrcArray(g CSRGraph) []uint32 {
	src := make([]uint32, len(g.Edges))
	for v := 0; v < g.NumNodes(); v++ {
		start := g.Offsets[v]
		end := g.Offsets[v+1]
		for ei := start; ei < end; ei++ {
			src[ei] = uint32(v)
		}
	}
	return src
}

// BuildIncomingIndex builds incoming edge indices for each vertex.
func BuildIncomingIndex(g CSRGraph) (incomingOffsets []uint32, incomingEdgeIDs []uint32) {
	n := g.NumNodes()
	inCounts := make([]uint32, n)
	for _, dst := range g.Edges {
		inCounts[dst]++
	}

	incomingOffsets = make([]uint32, n+1)
	for i := 0; i < n; i++ {
		incomingOffsets[i+1] = incomingOffsets[i] + inCounts[i]
	}

	incomingEdgeIDs = make([]uint32, len(g.Edges))
	writePos := make([]uint32, n)
	copy(writePos, incomingOffsets[:n])

	for e, dst := range g.Edges {
		p := writePos[dst]
		incomingEdgeIDs[p] = uint32(e)
		writePos[dst] = p + 1
	}

	return incomingOffsets, incomingEdgeIDs
}

// UnitWeights creates all-one weights (GraphBIG default when edge weights are absent).
func UnitWeights(edgeCount int) []uint32 {
	w := make([]uint32, edgeCount)
	for i := range w {
		w[i] = 1
	}
	return w
}

func readUint64Array(path string) ([]uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data)%8 != 0 {
		return nil, fmt.Errorf("invalid uint64 array length in %s", path)
	}

	arr := make([]uint64, len(data)/8)
	for i := range arr {
		arr[i] = binary.LittleEndian.Uint64(data[i*8 : i*8+8])
	}
	return arr, nil
}
