package lintguard

import (
	"reflect"
	"testing"
)

// The scanner finds both shapes and leaves handled errors and plain blank vars alone.
func TestScanFindsIgnoredErrors(t *testing.T) {
	sites, err := Scan("testdata", "testdata/bad")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range sites {
		got = append(got, s.Key())
	}
	want := []string{"bad/bad.go:Ignored:blank-call", "bad/bad.go:Ignored:log-only"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sites %v, want %v", got, want)
	}
}
