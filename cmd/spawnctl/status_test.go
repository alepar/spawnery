package main

import (
	"bytes"
	"strings"
	"testing"

	cpv1 "spawnery/gen/cp/v1"
)

func TestRenderStatusShowsFullError(t *testing.T) {
	errorDetail := "403 Forbidden: the token lacks write access\nError code: PERMISSION_DENIED\nSee https://docs.example.com for help"
	s := &cpv1.SpawnSummary{
		Status:      cpv1.SpawnStatus_SPAWN_STATUS_ERROR,
		ErrorStep:   "authorize",
		ErrorDetail: errorDetail,
	}

	var buf bytes.Buffer
	renderStatus(s, &buf)
	out := buf.String()

	if !strings.Contains(out, "ERROR") {
		t.Errorf("output missing ERROR status:\n%s", out)
	}
	if !strings.Contains(out, "authorize") {
		t.Errorf("output missing failed step 'authorize':\n%s", out)
	}
	if !strings.Contains(out, errorDetail) {
		t.Errorf("output missing full errorDetail (no truncation):\n%s", out)
	}
}

func TestRenderStatusActive(t *testing.T) {
	s := &cpv1.SpawnSummary{
		Status: cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE,
	}
	var buf bytes.Buffer
	renderStatus(s, &buf)
	out := buf.String()

	if !strings.Contains(out, "ACTIVE") {
		t.Errorf("output missing ACTIVE:\n%s", out)
	}
	if strings.Contains(out, "✗") {
		t.Errorf("unexpected failure indicator in ACTIVE status:\n%s", out)
	}
}

func TestRenderStatusErrorNilDetail(t *testing.T) {
	// ERROR with step but no detail: headline appears, no extra blank lines.
	s := &cpv1.SpawnSummary{
		Status:    cpv1.SpawnStatus_SPAWN_STATUS_ERROR,
		ErrorStep: "pull-image",
	}
	var buf bytes.Buffer
	renderStatus(s, &buf)
	out := buf.String()

	if !strings.Contains(out, "ERROR:pull-image") {
		t.Errorf("output missing step in status:\n%s", out)
	}
	if !strings.Contains(out, "✗ failed at pull-image") {
		t.Errorf("output missing failure headline:\n%s", out)
	}
}
