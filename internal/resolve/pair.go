// Package resolve turns configured rules into concrete mirror pairs.
package resolve

import (
	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/state"
)

// Pair is one concrete one-way mirror: a single source repository and the
// destination it is copied to.
type Pair struct {
	// Rule is the name of the rule that produced the pair.
	Rule string
	// Settings are the rule's effective settings.
	Settings config.RuleSettings
	// SrcForge names the source forge in the config.
	SrcForge string
	// Src is the source repository as reported by the source forge.
	Src forge.Repo
	// DstForge names the destination forge in the config.
	DstForge string
	// DstOwner and DstName are the rendered destination coordinates.
	DstOwner, DstName string
}

// DstFullName returns "owner/name" of the destination.
func (p Pair) DstFullName() string { return p.DstOwner + "/" + p.DstName }

// Key returns the state-store key for the pair.
func (p Pair) Key() state.PairKey {
	return state.PairKey{
		Rule:     p.Rule,
		SrcForge: p.SrcForge,
		SrcRepo:  p.Src.FullName(),
		DstForge: p.DstForge,
		DstRepo:  p.DstFullName(),
	}
}
