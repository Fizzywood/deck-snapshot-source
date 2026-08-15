package limits

import "testing"

func TestCounterEnforcesLimits(t *testing.T) {
	limit := Limits{MaxFiles: 2, MaxFileSize: 5, MaxTotalSize: 8, MaxManifestSize: 5, MaxPathLength: 64, MaxCompressionRatio: 10}
	if err := limit.Validate(); err != nil {
		t.Fatal(err)
	}
	counter := Counter{Limits: limit}
	if err := counter.Add(4); err != nil {
		t.Fatalf("first Add() error = %v", err)
	}
	if err := counter.Add(4); err != nil {
		t.Fatalf("second Add() error = %v", err)
	}
	if err := counter.Add(1); err == nil {
		t.Fatal("Add() accepted a third file")
	}
}

func TestCounterRejectsOversizedFile(t *testing.T) {
	counter := Counter{Limits: Limits{MaxFiles: 2, MaxFileSize: 5, MaxTotalSize: 8}}
	if err := counter.Add(6); err == nil {
		t.Fatal("Add() accepted an oversized file")
	}
}
