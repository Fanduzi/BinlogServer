// Package binlog provides module-level functionality for binlog.
// input: MySQL GTID set text (uuid:a-b:c, several UUIDs, optional tags) and single GTIDs read from segments
// output: an interval set that adds, subtracts, and prints MySQL GTID sets in canonical order
// pos: GTID arithmetic for the storage-inconsistency report (checkpoint minus stored, start plus valid segments)
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"sort"
	"strconv"
	"strings"
)

type gtidInterval struct{ lo, hi int64 }

// gtidIntervals maps a lowercase source id (uuid, or uuid:tag) to sorted,
// merged, inclusive intervals.
type gtidIntervals map[string][]gtidInterval

func newGTIDIntervals() gtidIntervals { return gtidIntervals{} }

// parseGTIDIntervals reads a MySQL GTID set. ok is false for empty or
// unparseable text.
func parseGTIDIntervals(text string) (gtidIntervals, bool) {
	text = strings.Join(strings.Fields(text), "")
	if text == "" {
		return nil, false
	}
	out := newGTIDIntervals()
	for _, part := range strings.Split(text, ",") {
		if part == "" {
			continue
		}
		fields := strings.Split(part, ":")
		if len(fields) < 2 || !looksUUID(fields[0]) {
			return nil, false
		}
		key := strings.ToLower(fields[0])
		for _, f := range fields[1:] {
			if f == "" {
				continue
			}
			if f[0] < '0' || f[0] > '9' {
				// MySQL 8.4 tag: the intervals that follow belong to uuid:tag.
				key = strings.ToLower(fields[0]) + ":" + strings.ToLower(f)
				continue
			}
			lo, hi := f, f
			if dash := strings.Index(f, "-"); dash >= 0 {
				lo, hi = f[:dash], f[dash+1:]
			}
			a, errA := strconv.ParseInt(lo, 10, 64)
			b, errB := strconv.ParseInt(hi, 10, 64)
			if errA != nil || errB != nil || a <= 0 || b < a {
				return nil, false
			}
			out.add(key, a, b)
		}
	}
	return out, len(out) > 0
}

func (s gtidIntervals) add(key string, lo, hi int64) {
	key = strings.ToLower(key)
	list := append(s[key], gtidInterval{lo, hi})
	sort.Slice(list, func(i, j int) bool { return list[i].lo < list[j].lo })
	merged := list[:0]
	for _, iv := range list {
		if n := len(merged); n > 0 && iv.lo <= merged[n-1].hi+1 {
			if iv.hi > merged[n-1].hi {
				merged[n-1].hi = iv.hi
			}
			continue
		}
		merged = append(merged, iv)
	}
	s[key] = merged
}

func (s gtidIntervals) addAll(o gtidIntervals) {
	for key, list := range o {
		for _, iv := range list {
			s.add(key, iv.lo, iv.hi)
		}
	}
}

func (s gtidIntervals) subtract(o gtidIntervals) {
	for key, cut := range o {
		list := s[key]
		if len(list) == 0 {
			continue
		}
		for _, c := range cut {
			next := make([]gtidInterval, 0, len(list)+1)
			for _, iv := range list {
				if c.hi < iv.lo || c.lo > iv.hi {
					next = append(next, iv)
					continue
				}
				if c.lo > iv.lo {
					next = append(next, gtidInterval{iv.lo, c.lo - 1})
				}
				if c.hi < iv.hi {
					next = append(next, gtidInterval{c.hi + 1, iv.hi})
				}
			}
			list = next
		}
		if len(list) == 0 {
			delete(s, key)
		} else {
			s[key] = list
		}
	}
}

// String prints uuid:a-b:c,uuid2:d with UUIDs sorted. An empty set is "".
func (s gtidIntervals) String() string {
	keys := make([]string, 0, len(s))
	for key, list := range s {
		if len(list) > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		var b strings.Builder
		b.WriteString(key)
		for _, iv := range s[key] {
			b.WriteString(":")
			b.WriteString(strconv.FormatInt(iv.lo, 10))
			if iv.hi != iv.lo {
				b.WriteString("-")
				b.WriteString(strconv.FormatInt(iv.hi, 10))
			}
		}
		parts = append(parts, b.String())
	}
	return strings.Join(parts, ",")
}
