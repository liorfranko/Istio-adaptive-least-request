package controller

import (
	"testing"

	istioNetworkingV1 "istio.io/api/networking/v1"
)

func TestDiff(t *testing.T) {
	slice := []*istioNetworkingV1.WorkloadEntry{{Address: "a"}, {Address: "b"}, {Address: "c"}, {Address: "k"}}
	slice2 := []*istioNetworkingV1.WorkloadEntry{{Address: "a"}, {Address: "b"}, {Address: "c"}, {Address: "d"}, {Address: "e"}}
	diff1 := diff(nil, slice, slice2)
	if len(diff1) != 3 {
		t.Errorf("Expected 2, got %d", len(diff1))
	}
	diff2 := diff(nil, slice2, slice)
	if len(diff2) != 3 {
		t.Errorf("Expected 2, got %d", len(diff2))
	}

	expected := []string{"d", "e", "k"}
	for i, d := range diff1 {
		if d.Address != expected[i] {
			t.Errorf("Expected %s, got %s", expected[i], d.Address)
		}
	}

	expected2 := []string{"k", "d", "e"}
	for i, d := range diff2 {
		if d.Address != expected2[i] {
			t.Errorf("Expected %s, got %s", expected[i], d.Address)
		}
	}
}
