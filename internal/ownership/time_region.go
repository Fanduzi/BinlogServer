// Package ownership is the pure task-ownership transition from ADR 0011.
// input: the clock comparisons in Step and the predicates Step calls
// output: the boundary list and the saturated deadline delta of the fair-search key
// pos: one table of (field, constant, comparison); a new Step guard has to be listed here
// note: if this file changes, update this header and module README.md.
package ownership

import (
	"strconv"
	"strings"
	"time"
)

// stepTimeGuard is one comparison of a timestamp against a constant.
// c, and for > or >= also c-1ns and c+1ns, stay exact in the search key.
// A delta strictly older than every one of those boundaries is one region.
// Open intervals stay exact: NextDeadline subtracts a concrete duration, and
// an interval does not determine which side of the next boundary that lands on.
// The far region is the only collapse, because a later jump cannot leave it.
type stepTimeGuard struct {
	field string
	cmp   string
	src   string
	cons  func(Params) time.Duration
}

func stepTimeGuards() []stepTimeGuard {
	zero := func(Params) time.Duration { return 0 }
	idle := func(p Params) time.Duration { return p.fill().IdleKill }
	lockWait := func(p Params) time.Duration { return p.fill().LockWait }
	sweep := func(p Params) time.Duration { return p.fill().Sweep }
	noProg := func(p Params) time.Duration { return p.fill().NoProgressAfter }
	ms := func(Params) time.Duration { return time.Millisecond }
	return []stepTimeGuard{
		{field: "lockSince", cmp: ">=", src: "!now.Before(s.Lock.Since.Add(s.Params.IdleKill))", cons: idle},
		{field: "lockSince", cmp: "<=", src: "!now.After(next.Lock.Since.Add(next.Params.LockWait))", cons: lockWait},
		{field: "unowned", cmp: ">=", src: "!now.Before(s.UnownedSince.Add(s.Params.Sweep))", cons: sweep},
		{field: "unowned", cmp: ">=", src: "!now.Before(unownedSince.Add(p.Sweep))", cons: sweep},
		{field: "progress", cmp: ">", src: "now.Sub(anchor) > s.Params.NoProgressAfter", cons: noProg},
		{field: "acquired", cmp: ">", src: "anchor = s.Lease.AcquiredAt", cons: noProg},
		{field: "progress", cmp: ">", src: "now.Sub(anchor) > p.NoProgressAfter", cons: noProg},
		{field: "heartbeat", cmp: ">", src: "now.Sub(hb) > EffectiveNodeGone(s.Params)", cons: EffectiveNodeGone},
		{field: "heartbeat", cmp: ">", src: "due.Before(now)", cons: EffectiveNodeGone},
		{field: "heartbeat", cmp: ">", src: "EffectiveNodeGone(s.Params) + time.Nanosecond", cons: EffectiveNodeGone},
		{field: "expire", cmp: ">", src: "s.Lease.ExpireAt.After(now)", cons: zero},
		{field: "expire", cmp: ">", src: "now.Before(s.Lease.ExpireAt)", cons: zero},
		{field: "expire", cmp: ">", src: "snap.Expire.After(now)", cons: zero},
		{field: "retry", cmp: ">", src: "now.Before(s.RetryUntil)", cons: zero},
		{field: "retry", cmp: ">", src: "s.RetryUntil.After(now)", cons: zero},
		{field: "retry", cmp: ">", src: "snap.RetryUntil.After(now)", cons: zero},
		{field: "until", cmp: ">", src: "r.Until.After(now)", cons: zero},
		{field: "evaluated", cmp: ">", src: "now.Sub(s.EvaluatedAt) > EffectiveStaleAfter(s.Params)", cons: EffectiveStaleAfter},
		{field: "failedAt", cmp: ">", src: "s.FailedAt.Add(time.Millisecond)", cons: ms},
	}
}

// fieldLimit is the furthest boundary age of this field: max(c+1ns).
// A delta of exactly -fieldLimit is still exact. Only a strictly older delta saturates.
func fieldLimit(field string, p Params) time.Duration {
	p = p.fill()
	var limit time.Duration
	for _, g := range stepTimeGuards() {
		if g.field != field {
			continue
		}
		c := g.cons(p)
		if c < 0 {
			c = 0
		}
		edge := c + time.Nanosecond
		if edge > limit {
			limit = edge
		}
	}
	if limit <= 0 {
		return time.Nanosecond
	}
	return limit
}

func writeRegion(b *strings.Builder, field string, t, now time.Time, p Params) {
	if t.IsZero() {
		b.WriteByte('Z')
		b.WriteByte('|')
		return
	}
	d := t.Sub(now)
	limit := fieldLimit(field, p)
	if d < -limit {
		b.WriteByte('S')
		b.WriteByte('|')
		return
	}
	b.WriteString(strconv.FormatInt(int64(d), 10))
	b.WriteByte('|')
}
