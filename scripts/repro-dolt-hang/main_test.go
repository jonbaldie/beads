package main

import (
	"reflect"
	"testing"
)

func TestParseReproConfig(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want reproConfig
	}{
		{"defaults", nil, reproConfig{workers: defaultWorkers, opsPerWorker: defaultOpsPerWorker, mode: "both"}},
		{"all overrides", []string{"3", "4", "old"}, reproConfig{workers: 3, opsPerWorker: 4, mode: "old"}},
		{"invalid numbers retain defaults", []string{"bad", "worse", "new"}, reproConfig{workers: defaultWorkers, opsPerWorker: defaultOpsPerWorker, mode: "new"}},
		{"workers only", []string{"7"}, reproConfig{workers: 7, opsPerWorker: defaultOpsPerWorker, mode: "both"}},
		{"workers and operations", []string{"7", "8"}, reproConfig{workers: 7, opsPerWorker: 8, mode: "both"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseReproConfig(tt.args); got != tt.want {
				t.Fatalf("parseReproConfig(%v) = %#v, want %#v", tt.args, got, tt.want)
			}
		})
	}
}

func TestReproModes(t *testing.T) {
	if got, want := reproModes("both"), []string{"old", "new"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reproModes(both) = %v, want %v", got, want)
	}
	if got, want := reproModes("old"), []string{"old"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reproModes(old) = %v, want %v", got, want)
	}
}

func TestHasHang(t *testing.T) {
	if hasHang(nil) {
		t.Fatal("hasHang(nil) = true")
	}
	if hasHang([]runStats{{unresponsive: 0}, {unresponsive: 0}}) {
		t.Fatal("zero unresponsive events reported a hang")
	}
	if !hasHang([]runStats{{unresponsive: 0}, {unresponsive: 1}}) {
		t.Fatal("unresponsive event did not report a hang")
	}
}
