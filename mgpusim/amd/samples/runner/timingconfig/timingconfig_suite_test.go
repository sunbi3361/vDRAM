package timingconfig

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestTimingConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Timing Config Suite")
}
