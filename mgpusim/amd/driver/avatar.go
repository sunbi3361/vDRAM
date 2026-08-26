// sbin_claude_avatar
package driver

import (
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/meta"
)

// AvatarConfig carries the Avatar policy knobs into the driver. The driver
// plays the OS/runtime role (refs/avatar.md 5.11): it installs the embedded
// page metadata when a page is mapped, invalidates the old physical
// location when a mapping changes, and - when the fragmentation model is on
// - places physical memory at 2MB-region granularity with randomized region
// selection (avatar-plan.md 1.4).
type AvatarConfig struct {
	// Enabled turns the metadata bookkeeping on.
	Enabled bool
	// Registry is the authoritative metadata/placement state shared with
	// the GPU-side Avatar Speculation Unit (ASU).
	Registry *meta.Registry
	// FragEnabled turns 2MB-region randomized physical placement on. When
	// off, the stock sequential frame pool is used (PPN-VPN becomes
	// globally constant and the MOD predicts near-perfectly).
	FragEnabled bool
}

// AvatarRegistry exposes the shared authoritative Avatar state (nil when
// Avatar is disabled). Reporters and tests use it to inspect metadata and
// region occupancy.
func (d *Driver) AvatarRegistry() *meta.Registry {
	return d.avatarConfig.Registry
}
