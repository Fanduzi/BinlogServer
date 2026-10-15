// Package assurance provides module-level functionality for assurance.
// input: a Chain (its Local ledger, checkpoint, start set, flavor and lazy planner conclusions) and, for the cut, the chain's segments in order with each one's place relative to the problem segment
// output: the storage-health verdict (the first of: damaged segment, unreadable segment, legacy failback, stored GTID hole, a sealed source file held twice on the chain order) with its StorageProblem, and the segments /replay and /window keep before it; since P2-3 the overlap verdict counts an open copy with the same content, never an empty leftover
// pos: the one storage verdict and replay cut (ADR 0009 phase 1, P1-3); tasks only supplies planners, chain order and the generation comparison
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"strings"

	"binlog_server/internal/binlog"
)

func (c *Chain) plan() []Violation {
	if !c.planDone {
		c.planDone = true
		if c.in.Plan != nil {
			c.planned = c.in.Plan()
		}
	}
	return c.planned
}

// Health is the storage-health verdict: a damaged segment (I1, from
// Ledger.Damage with the checkpoint, start set and flavor) wins, then an
// unreadable sealed segment (I1_UNREADABLE, Ledger.Unreadable), then the
// planner conclusions in the order Plan gives them, then a sealed source file
// held twice on the chain order (I4_OVERLAP). Only those kinds are a
// verdict; the other Check kinds are not, so the verdict stays what it was
// before ADR 0009. Plan is not asked when a segment is damaged.
func (c *Chain) Health() (Violation, bool) {
	if c.in.Local != nil {
		if problem, found := c.in.Local.Damage(c.in.Checkpoint.GTIDSet, c.in.StartGTID, c.in.Flavor); found {
			p := problem
			return Violation{Kind: I1Regression, Segments: []string{problem.Segment}, Detail: problem.Message, Problem: &p}, true
		}
		if problem, found := c.in.Local.Unreadable(); found {
			p := problem
			return Violation{Kind: I1Unreadable, Segments: []string{problem.Segment}, Detail: problem.Message, Problem: &p}, true
		}
	}
	for _, v := range c.plan() {
		if v.Kind == I3LegacyFailback || v.Kind == I3StoredHole {
			return v, true
		}
	}
	if v, found := c.sealedTwice(); found {
		return v, true
	}
	return Violation{}, false
}

// sealedTwice is the first sealed source file the chain order (Inputs.Order,
// the segments replay would return) holds twice in the task directory
// (I4_OVERLAP, #370), judged as Check judges it. The problem is the later
// copy; replay and the window stop before both. Only two local files count:
// a copy that is not on disk cannot be told apart here. A takeover's
// point epoch (no event span of its own) is not on the order: replay
// already leaves it out.
func (c *Chain) sealedTwice() (Violation, bool) {
	segs := c.Segments()
	repeats := c.sealedRepeats(segs, func(earlier, later *Segment) bool {
		return earlier.Local && later.Local
	})
	for i := range segs {
		oi, ok := repeats[i]
		if !ok {
			continue
		}
		earlier, later := &segs[oi], &segs[i]
		problem := binlog.StorageProblem{
			Segment: later.Name,
			Detail:  "Source file " + later.Named.Source + " is stored twice: " + earlier.Name + " and " + later.Name + " hold the same events, or one of them cannot be read.",
		}
		for _, prev := range segs[:oi] {
			if prev.Named.Source != later.Named.Source {
				problem.Valid = append(problem.Valid, prev.Name)
			}
		}
		problem.Message = problem.Detail + " A restore over both would apply the same transactions twice or fail on the bad copy, so replay and the recovery window stop before them, and they and every later segment are left out. Keep the copy that matches the catalog row or object storage, move the other out of the task directory, and the window comes back."
		return Violation{Kind: I4Overlap, Segments: []string{later.Name, earlier.Name}, Evidence: earlier.Name + " " + later.Name, Detail: sealedTwiceDetail(earlier, later), Problem: &problem}, true
	}
	return Violation{}, false
}

// Link is one chosen segment in chain order, as the cut sees it.
type Link struct {
	Name string
	// SameIndex: the segment is a copy (open or sealed, any epoch) of the
	// problem segment's source index.
	SameIndex bool
	// Before: the segment sorts before the problem segment in chain order.
	Before bool
}

// KeepBefore is the positions of chain a restore keeps for a verdict whose
// problem is at segment: every link before it that is not a copy of its
// index. A verdict that names no segment keeps nothing.
func KeepBefore(chain []Link, segment string) []int {
	out := []int{}
	if strings.TrimSpace(segment) == "" {
		return out
	}
	for i, l := range chain {
		if l.SameIndex || !l.Before {
			continue
		}
		out = append(out, i)
	}
	return out
}
