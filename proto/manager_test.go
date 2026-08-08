package proto_test

import (
	"testing"

	pb "github.com/brotherlogic/devcontainer-manager/proto"
)

func TestHarnessEnumAndField(t *testing.T) {
	if pb.Harness_HARNESS_UNSPECIFIED != 0 {
		t.Errorf("expected HARNESS_UNSPECIFIED = 0, got %d", pb.Harness_HARNESS_UNSPECIFIED)
	}
	if pb.Harness_HARNESS_ANTIGRAVITY != 1 {
		t.Errorf("expected HARNESS_ANTIGRAVITY = 1, got %d", pb.Harness_HARNESS_ANTIGRAVITY)
	}
	if pb.Harness_HARNESS_PI != 2 {
		t.Errorf("expected HARNESS_PI = 2, got %d", pb.Harness_HARNESS_PI)
	}

	req := &pb.UpRequest{
		Harness: pb.Harness_HARNESS_PI,
	}

	if req.GetHarness() != pb.Harness_HARNESS_PI {
		t.Errorf("expected req.GetHarness() = HARNESS_PI, got %v", req.GetHarness())
	}
}
