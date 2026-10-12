// Package binlog provides module-level functionality for binlog.
// input: MySQL GTID set text (uuid:a-b:c, several UUIDs, optional tags) and single GTIDs read from segments
// output: GTIDSet, the one GTID interval type of this repository: add, union, subtract, contain, interior gaps, and the canonical text
// pos: GTID arithmetic behind the segment ledger (stored set, holes, recovery sets) and every caller that asks it
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"sort"
	"strconv"
	"strings"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
)

// GTIDRange is one closed interval of transaction sequence numbers.
type GTIDRange struct{ Lo, Hi int64 }

// GTIDSet is a MySQL GTID set: for each source key (a lowercase uuid, or
// uuid:tag) the sorted, merged, closed intervals it holds. Every method
// keeps that invariant. The zero value (nil) is an empty set for reading;
// use GTIDSet{} to add to it.
type GTIDSet map[string][]GTIDRange

// ParseGTIDSet reads a MySQL gtid_set the way go-mysql does: anything it
// rejects is an error, and its canonical form decides keys and tags. An
// empty or blank text is an empty set.
func ParseGTIDSet(text string) (GTIDSet, error) {
	out := GTIDSet{}
	text = strings.TrimSpace(text)
	if text == "" {
		return out, nil
	}
	set, err := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, text)
	if err != nil {
		return nil, err
	}
	for _, item := range strings.Split(strings.ReplaceAll(set.String(), "\n", ""), ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		fields := strings.Split(item, ":")
		key := strings.ToLower(fields[0])
		for _, f := range fields[1:] {
			lo, hi, ok := parseGTIDRange(f)
			if !ok {
				key = strings.ToLower(fields[0]) + ":" + f
				continue
			}
			out.Add(key, lo, hi)
		}
	}
	return out, nil
}

// ParseGTIDSetLenient reads a MySQL GTID set without go-mysql: uuids must
// look like uuids, sequences must be positive, and tags are lowercased.
// ok is false for an empty or unparseable text.
func ParseGTIDSetLenient(text string) (GTIDSet, bool) {
	text = strings.Join(strings.Fields(text), "")
	if text == "" {
		return nil, false
	}
	out := GTIDSet{}
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
			out.Add(key, a, b)
		}
	}
	return out, len(out) > 0
}

func parseGTIDRange(f string) (int64, int64, bool) {
	lo, hi := f, f
	if i := strings.IndexByte(f, '-'); i > 0 {
		lo, hi = f[:i], f[i+1:]
	}
	a, err1 := strconv.ParseInt(lo, 10, 64)
	b, err2 := strconv.ParseInt(hi, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return a, b, true
}

// Add puts lo..hi under key. An empty key, a sequence below 1, or hi < lo
// is ignored. The key is used as given.
func (s GTIDSet) Add(key string, lo, hi int64) {
	if key == "" || lo <= 0 || hi < lo {
		return
	}
	list := s[key]
	if n := len(list); n == 0 || lo > list[n-1].Hi+1 {
		s[key] = append(list, GTIDRange{lo, hi})
		return
	} else if lo >= list[n-1].Lo {
		if hi > list[n-1].Hi {
			list[n-1].Hi = hi
		}
		return
	}
	list = append(list, GTIDRange{lo, hi})
	sort.Slice(list, func(i, j int) bool { return list[i].Lo < list[j].Lo })
	merged := list[:0]
	for _, iv := range list {
		if n := len(merged); n > 0 && iv.Lo <= merged[n-1].Hi+1 {
			if iv.Hi > merged[n-1].Hi {
				merged[n-1].Hi = iv.Hi
			}
			continue
		}
		merged = append(merged, iv)
	}
	s[key] = merged
}

// AddEvents adds each event's uuid:seq. Events without a uuid or with a
// sequence below 1 are skipped.
func (s GTIDSet) AddEvents(events []GTIDEventRef) {
	for _, ev := range events {
		if ev.UUID == "" || ev.Seq <= 0 {
			continue
		}
		s.Add(strings.ToLower(ev.UUID), ev.Seq, ev.Seq)
	}
}

// AddAll adds every interval of o.
func (s GTIDSet) AddAll(o GTIDSet) {
	for key, list := range o {
		for _, iv := range list {
			s.Add(key, iv.Lo, iv.Hi)
		}
	}
}

// Clone is an independent copy.
func (s GTIDSet) Clone() GTIDSet {
	out := make(GTIDSet, len(s))
	for k, list := range s {
		out[k] = append([]GTIDRange(nil), list...)
	}
	return out
}

// Minus is s without o, as a new set. Keys left empty are dropped.
func (s GTIDSet) Minus(o GTIDSet) GTIDSet {
	out := GTIDSet{}
	for k, list := range s {
		cut := o[k]
		for _, iv := range list {
			lo, hi := iv.Lo, iv.Hi
			for _, c := range cut {
				if c.Hi < lo || c.Lo > hi {
					continue
				}
				if c.Lo > lo {
					out.Add(k, lo, c.Lo-1)
				}
				lo = c.Hi + 1
				if lo > hi {
					break
				}
			}
			if lo <= hi {
				out.Add(k, lo, hi)
			}
		}
	}
	return out
}

// Has reports that key:seq is in s.
func (s GTIDSet) Has(key string, seq int64) bool {
	list := s[key]
	i := sort.Search(len(list), func(i int) bool { return list[i].Hi >= seq })
	return i < len(list) && list[i].Lo <= seq
}

// Contains reports that every transaction of o is in s.
func (s GTIDSet) Contains(o GTIDSet) bool {
	for k, list := range o {
		have := s[k]
		for _, iv := range list {
			i := sort.Search(len(have), func(i int) bool { return have[i].Hi >= iv.Lo })
			if i == len(have) || have[i].Lo > iv.Lo || have[i].Hi < iv.Hi {
				return false
			}
		}
	}
	return true
}

// HasEvent reports that the event's uuid:seq is in s.
func (s GTIDSet) HasEvent(ev GTIDEventRef) bool {
	return s.Has(strings.ToLower(ev.UUID), ev.Seq)
}

// IsEmpty reports a set with no transaction.
func (s GTIDSet) IsEmpty() bool {
	for _, list := range s {
		if len(list) > 0 {
			return false
		}
	}
	return true
}

// InteriorGaps lists, per key, the sequences missing between the lowest
// and highest sequence of s. History before the lowest is not a gap.
func (s GTIDSet) InteriorGaps() GTIDSet {
	out := GTIDSet{}
	for k, list := range s {
		for i := 1; i < len(list); i++ {
			out.Add(k, list[i-1].Hi+1, list[i].Lo-1)
		}
	}
	return out
}

// String prints key:a-b:c per key, keys sorted, joined by ",". A tagged key
// prints as uuid:tag:a-b. An empty set is "".
func (s GTIDSet) String() string {
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
			b.WriteByte(':')
			b.WriteString(strconv.FormatInt(iv.Lo, 10))
			if iv.Hi != iv.Lo {
				b.WriteByte('-')
				b.WriteString(strconv.FormatInt(iv.Hi, 10))
			}
		}
		parts = append(parts, b.String())
	}
	return strings.Join(parts, ",")
}
