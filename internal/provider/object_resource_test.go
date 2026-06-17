// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"reflect"
	"testing"
)

func TestParseLines(t *testing.T) {
	t.Run("valid array", func(t *testing.T) {
		got, err := parseLines(`["switchport pvid 3","name \"X\""]`)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"switchport pvid 3", `name "X"`}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("parseLines = %v, want %v", got, want)
		}
	})
	t.Run("empty string is empty slice", func(t *testing.T) {
		got, err := parseLines("")
		if err != nil || len(got) != 0 {
			t.Fatalf("parseLines(\"\") = %v, err=%v", got, err)
		}
	})
	t.Run("non-array errors", func(t *testing.T) {
		if _, err := parseLines(`{"a":1}`); err == nil {
			t.Fatal("expected error for non-array")
		}
	})
	t.Run("non-string element errors", func(t *testing.T) {
		if _, err := parseLines(`["ok", 7]`); err == nil {
			t.Fatal("expected error for non-string element")
		}
	})
}

func TestMarshalLines(t *testing.T) {
	if got := marshalLines([]string{"a", "b"}); got != `["a","b"]` {
		t.Errorf("marshalLines = %q", got)
	}
	if got := marshalLines(nil); got != "[]" {
		t.Errorf("marshalLines(nil) = %q, want []", got)
	}
}

func TestMarshalParseRoundTrip(t *testing.T) {
	in := []string{`name "TELUS_WAN"`, "no ipv6 enable"}
	out, err := parseLines(marshalLines(in))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip = %v, want %v", out, in)
	}
}

func TestSubtractLines(t *testing.T) {
	// Set math is normalization-insensitive (spacing differences ignored).
	a := []string{"switchport pvid 3", "switchport general allowed vlan 2-3 tagged"}
	b := []string{"switchport   pvid   3"}
	got := subtractLines(a, b)
	want := []string{"switchport general allowed vlan 2-3 tagged"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("subtractLines = %v, want %v", got, want)
	}
}

func TestAddedLines(t *testing.T) {
	declared := []string{`name "NEW"`, "ipv6 enable"}
	prior := []string{"ipv6 enable"}
	got := addedLines(declared, prior)
	if !reflect.DeepEqual(got, []string{`name "NEW"`}) {
		t.Fatalf("addedLines = %v", got)
	}
	// Nothing added when all declared lines pre-existed (adoption-safe).
	if got := addedLines([]string{"ipv6 enable"}, []string{"ipv6 enable"}); len(got) != 0 {
		t.Fatalf("addedLines(all pre-existing) = %v, want empty", got)
	}
}

func TestContextID(t *testing.T) {
	cases := map[string]string{
		"interface gigabitEthernet 1/0/6": "interface gigabitEthernet 1/0/6",
		" vlan   2 ":                      "vlan 2",
		"":                                globalID,
		"   ":                             globalID,
	}
	for in, want := range cases {
		if got := contextID(in); got != want {
			t.Errorf("contextID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLineSubsetMatches(t *testing.T) {
	cases := []struct {
		name        string
		state, cfg  string
		wantMatched bool
	}{
		{
			name:        "config subset of live block — match (0-diff)",
			state:       `["switchport general allowed vlan 2-3 tagged","switchport pvid 3"]`,
			cfg:         `["switchport general allowed vlan 2-3 tagged"]`,
			wantMatched: true,
		},
		{
			name:        "declared line absent on device — no match (update)",
			state:       `["switchport pvid 3"]`,
			cfg:         `["switchport general allowed vlan 2-3 tagged"]`,
			wantMatched: false,
		},
		{
			name:        "spacing-insensitive match",
			state:       `["switchport general allowed vlan 2-3 tagged"]`,
			cfg:         `["switchport   general   allowed   vlan   2-3   tagged"]`,
			wantMatched: true,
		},
		{
			name:        "empty config is a trivial subset — match",
			state:       `["a"]`,
			cfg:         `[]`,
			wantMatched: true,
		},
		{
			name:        "empty live block, non-empty config — no match",
			state:       `[]`,
			cfg:         `["ipv6 enable"]`,
			wantMatched: false,
		},
		{
			name:        "invalid state JSON — no match (fall back to diff)",
			state:       `not json`,
			cfg:         `["a"]`,
			wantMatched: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lineSubsetMatches(tc.state, tc.cfg); got != tc.wantMatched {
				t.Fatalf("lineSubsetMatches() = %v, want %v", got, tc.wantMatched)
			}
		})
	}
}
