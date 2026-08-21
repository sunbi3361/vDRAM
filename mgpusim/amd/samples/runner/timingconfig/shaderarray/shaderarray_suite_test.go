package shaderarray

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestShaderArray(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Shader Array Suite")
}
