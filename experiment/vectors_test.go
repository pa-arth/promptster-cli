package main

import (
	"encoding/json"
	"os"
	"testing"
)

// testdata/experiment-block-vectors.json is a VERBATIM copy of the backend's
// apps/api/src/__tests__/__fixtures__/experiment-block-vectors.json (PR #699,
// branch feat/experiment-assignment-log). Both implementations assert against
// this file rather than against each other's prose.
//
// A mismatch here is the two halves disagreeing about what arm an engineer was
// actually shown — the one failure the assignment log cannot survive, because
// the log's whole claim is that assignment is ground truth. If this test ever
// goes red, stop assigning: it is not a formatting difference.
const vectorsPath = "testdata/experiment-block-vectors.json"

type blockVectors struct {
	Algorithm map[string]string `json:"algorithm"`
	Vectors   []struct {
		OrgID          string   `json:"orgId"`
		EngineerUserID string   `json:"engineerUserId"`
		Stratum        string   `json:"stratum"`
		ExperimentKey  string   `json:"experimentKey"`
		Cells          []string `json:"cells"`
		Arms           []string `json:"arms"`
	} `json:"vectors"`
}

func loadVectors(t *testing.T) blockVectors {
	t.Helper()
	data, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatalf("shared vectors missing: %v", err)
	}
	var v blockVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("shared vectors are not valid JSON: %v", err)
	}
	if len(v.Vectors) == 0 {
		t.Fatal("shared vectors file has no vectors")
	}
	return v
}

// TestGoAllocatorMatchesBackendVectors is the cross-implementation check that
// was task 0.1's stated open item before batch 1's first real assignment.
func TestGoAllocatorMatchesBackendVectors(t *testing.T) {
	vs := loadVectors(t)
	for _, v := range vs.Vectors {
		cfg := Config{
			OrgID:         v.OrgID,
			EngineerID:    v.EngineerUserID,
			ExperimentKey: v.ExperimentKey,
			Enabled:       true,
		}
		for pos, want := range v.Arms {
			got, _, _ := assignArm(cfg, v.Stratum, pos)
			if got != want {
				t.Fatalf("DRIFT — %s / %s position %d: Go says %q, backend vector says %q",
					v.EngineerUserID, v.Stratum, pos, got, want)
			}
		}
	}
}

// TestCellDeclarationOrderMatchesTheVectors guards the subtlest way the two
// implementations could diverge while every seed byte still agrees: the
// permutation is over cells in DECLARATION order, so a reordering of armCells
// silently relabels every arm.
func TestCellDeclarationOrderMatchesTheVectors(t *testing.T) {
	vs := loadVectors(t)
	for _, v := range vs.Vectors {
		if len(v.Cells) != len(armCells) {
			t.Fatalf("vector declares %d cells, Go has %d", len(v.Cells), len(armCells))
		}
		for i := range v.Cells {
			if v.Cells[i] != armCells[i] {
				t.Fatalf("cell declaration order differs at %d: Go %q, backend %q\nGo:      %v\nbackend: %v",
					i, armCells[i], v.Cells[i], armCells, v.Cells)
			}
		}
	}
}
