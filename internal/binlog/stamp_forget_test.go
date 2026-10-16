package binlog

import "testing"

func TestForgetFileDropsEveryStampCache(t *testing.T) {
	st := FileStamp{Size: 1}
	const p = "/nonexistent/forget-me"
	positionsCache.put(p, st, positionsCacheEntry{})
	contentsCache.put(p, st, SegmentContents{})
	sealedCorruptCache.put(p, st, "x")
	sealSumsCache.put(p, st, nil)
	ForgetFile(p)
	if _, ok := positionsCache.get(p, st); ok {
		t.Error("positionsCache kept")
	}
	if _, ok := contentsCache.get(p, st); ok {
		t.Error("contentsCache kept")
	}
	if _, ok := sealedCorruptCache.get(p, st); ok {
		t.Error("sealedCorruptCache kept")
	}
	if _, ok := sealSumsCache.get(p, st); ok {
		t.Error("sealSumsCache kept")
	}
}
