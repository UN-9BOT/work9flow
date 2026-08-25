package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionIsSet(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must not be empty")
	}
	if !strings.HasPrefix(Version, "0.") {
		t.Errorf("Version = %q, want 0.x.x-* prefix", Version)
	}
}

func TestHealthRoundTrip(t *testing.T) {
	in := HealthResponse{Status: "ok", Version: Version, UptimeS: 42}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out HealthResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", out, in)
	}
}

func TestRunListRoundTrip(t *testing.T) {
	in := RunListResponse{Runs: []RunSummary{
		{ID: "r1", WorkflowID: "feature-dev", State: "running", Stage: "review", Title: "Add login", Pending: 2},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out RunListResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Runs) != 1 || out.Runs[0].ID != "r1" || out.Runs[0].Pending != 2 {
		t.Fatalf("round-trip mismatch: got %+v", out)
	}
}
