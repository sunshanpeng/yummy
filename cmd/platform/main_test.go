package main

import (
	"slices"
	"testing"
)

func TestParseModeDefaultsToAll(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantMode string
		wantRest []string
	}{
		{name: "no arguments", wantMode: "all"},
		{name: "flags start all", args: []string{"--public-addr", ":9000"}, wantMode: "all", wantRest: []string{"--public-addr", ":9000"}},
		{name: "explicit all", args: []string{"all", "--public-addr", ":9000"}, wantMode: "all", wantRest: []string{"--public-addr", ":9000"}},
		{name: "control", args: []string{"control"}, wantMode: "control"},
		{name: "worker", args: []string{"worker", "--worker-id", "worker-2"}, wantMode: "worker", wantRest: []string{"--worker-id", "worker-2"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mode, rest, err := parseMode(test.args)
			if err != nil {
				t.Fatalf("parseMode() error = %v", err)
			}
			if mode != test.wantMode || !slices.Equal(rest, test.wantRest) {
				t.Fatalf("parseMode() = %q, %#v; want %q, %#v", mode, rest, test.wantMode, test.wantRest)
			}
		})
	}
}

func TestParseModeRejectsUnknownSubcommand(t *testing.T) {
	if _, _, err := parseMode([]string{"unknown"}); err == nil {
		t.Fatal("parseMode() 应拒绝未知子命令")
	}
}
