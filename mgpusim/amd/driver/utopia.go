// sbin_claude_utopia
package driver

import (
	"github.com/sarchlab/mgpusim/v4/amd/timing/utopia/restseg"
)

// UtopiaConfig carries the Utopia (hybrid RestSeg/FlexSeg address
// translation) policy knobs into the driver. The driver plays the OS role
// (utopia.md 4.13): it reserves the RestSeg physical region per GPU, owns the
// authoritative TAR/SF state through the shared registry, and places pages
// into RestSeg sets at allocation time.
type UtopiaConfig struct {
	// Enabled turns RestSeg reservation and RestSeg-first allocation on.
	Enabled bool
	// Registry is the authoritative RestSeg state shared with the GPU-side
	// RestSeg walker (UTU).
	Registry *restseg.Registry
	// RestSegBytes is the RestSeg size reserved per GPU. The remainder of the
	// GPU memory stays FlexSeg.
	RestSegBytes uint64
	// Associativity is the number of ways per RestSeg set.
	Associativity int
}

// UtopiaRegistry exposes the shared authoritative RestSeg state (nil when
// Utopia is disabled). Reporters and tests use it to inspect RestSeg
// occupancy.
func (d *Driver) UtopiaRegistry() *restseg.Registry {
	return d.utopiaConfig.Registry
}
