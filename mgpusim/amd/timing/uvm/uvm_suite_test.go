package uvm

// sbin_codex: package-level Ginkgo bootstrap for the UVM remote-access
// components (plan todo 11 of mgpusim-uvm-manager). The generated mocks are
// gitignored; regenerate with `go generate ./amd/timing/uvm/...`.

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

//go:generate mockgen -destination "mock_sim_test.go" -package $GOPACKAGE -write_package_comment=false github.com/sarchlab/akita/v4/sim Engine,Port

func TestUvm(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "UVM Suite")
}
