package r9nano

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestR9Nano(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "R9 Nano Suite")
}
