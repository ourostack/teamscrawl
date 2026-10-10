//go:build !windows

package browser

import "time"

type legacyCloseWitness struct{}

func (*Browser) prepareClose(bool) closeWitness { return legacyCloseWitness{} }

func (legacyCloseWitness) deadline() time.Time { return time.Time{} }

func (legacyCloseWitness) stop(b *Browser) error {
	if b.worthSignallingGroup() {
		stopGroup(b.group)
	}
	return sweepArgv(b.profile, true)
}
