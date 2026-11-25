package engine

import (
	"context"
	"testing"
)

func TestMatchMicroVMImage(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		ref      string
		want     bool
	}{
		{"empty_patterns", nil, "postgres:16", false},
		{"exact_tag_shorthand", []string{"postgres:16"}, "postgres:16", true},
		{"exact_tag_canonical_hub", []string{"postgres:16"}, "docker.io/library/postgres:16", true},
		{"canonical_pattern_shorthand_ref", []string{"docker.io/library/postgres:16"}, "postgres:16", true},
		{"exact_tag_mismatch", []string{"postgres:16"}, "postgres:15", false},
		{"repo_only_matches_any_tag", []string{"postgres"}, "postgres:16-alpine", true},
		{"repo_only_matches_untagged", []string{"postgres"}, "postgres", true},
		{"repo_only_matches_digest", []string{"postgres"}, "postgres@sha256:deadbeef", true},
		{"latest_pattern_matches_untagged", []string{"postgres:latest"}, "postgres", true},
		{"org_repo_hub_shorthand", []string{"catthehacker/ubuntu:act-latest"}, "docker.io/catthehacker/ubuntu:act-latest", true},
		{"wildcard_tag", []string{"postgres:16*"}, "postgres:16-alpine", true},
		{"wildcard_tag_no_match", []string{"postgres:16*"}, "postgres:15-alpine", false},
		{"wildcard_registry_prefix", []string{"ghcr.io/ironpark/*"}, "ghcr.io/ironpark/gvx-ext-github-actions:0.2.0", true},
		{"wildcard_registry_nested", []string{"ghcr.io/ironpark/*"}, "ghcr.io/ironpark/team/worker:v1", true},
		{"wildcard_registry_mismatch", []string{"ghcr.io/ironpark/*"}, "ghcr.io/other/app:1", false},
		{"localhost_registry_with_port", []string{"localhost:5001/myapp:dev"}, "localhost:5001/myapp:dev", true},
		{"localhost_registry_repo_only", []string{"localhost:5001/myapp"}, "localhost:5001/myapp:v2", true},
		{"case_insensitive_repo", []string{"GHCR.IO/Org/App:V1"}, "ghcr.io/org/app:V1", true},
		{"wildcard_all", []string{"*"}, "alpine:3.20", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchMicroVMImage(tc.patterns, tc.ref); got != tc.want {
				t.Errorf("MatchMicroVMImage(%v, %q) = %v, want %v", tc.patterns, tc.ref, got, tc.want)
			}
		})
	}
}

func TestNormalizeMicroVMImagePattern(t *testing.T) {
	cases := map[string]string{
		"  Postgres:16-Alpine ": "postgres:16-Alpine",
		"GHCR.IO/Org/App/*":     "ghcr.io/org/app/*",
		"localhost:5001/App":    "localhost:5001/app",
		"   ":                   "",
		"bad image":             "",
	}
	for in, want := range cases {
		if got := NormalizeMicroVMImagePattern(in); got != want {
			t.Errorf("NormalizeMicroVMImagePattern(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveCreateImage_PerImageMicroVMRules(t *testing.T) {
	te := newTestEngine(t)
	addCachedMicroVMImage(t, te, hostImageArch, "postgres:16", ImageConfig{Cmd: []string{"postgres"}}, 1)
	addCachedMicroVMImage(t, te, hostImageArch, "redis:7", ImageConfig{Cmd: []string{"redis-server"}}, 2)

	setupFakeMicroVM(t, te, true, nil)
	if err := te.SetMicroVM(&MicroVMConfig{
		Images: []string{"postgres:16"},
	}); err != nil {
		t.Fatalf("SetMicroVM: %v", err)
	}

	ctx := context.Background()

	// 1. Registered image with no explicit runtime -> automatically selects microvm.
	_, rt, _, err := te.resolveCreateImage(ctx, "postgres:16", "", "")
	if err != nil {
		t.Fatalf("resolveCreateImage(postgres:16): %v", err)
	}
	if rt != RuntimeMicroVM {
		t.Fatalf("registered image runtime = %q, want %q", rt, RuntimeMicroVM)
	}

	// 2. Registered image with explicit host platform and no runtime -> selects microvm.
	_, rt, _, err = te.resolveCreateImage(ctx, "docker.io/library/postgres:16", "linux/"+hostImageArch, "")
	if err != nil {
		t.Fatalf("resolveCreateImage(canonical postgres:16): %v", err)
	}
	if rt != RuntimeMicroVM {
		t.Fatalf("canonical registered image runtime = %q, want %q", rt, RuntimeMicroVM)
	}

	// 3. Explicit --runtime=gvx overrides the registered micro-VM image rule.
	_, rt, _, err = te.resolveCreateImage(ctx, "postgres:16", "", RuntimeGVX)
	if err != nil {
		t.Fatalf("resolveCreateImage(postgres:16, gvx): %v", err)
	}
	if rt != RuntimeGVX {
		t.Fatalf("explicit gvx override runtime = %q, want %q", rt, RuntimeGVX)
	}

	// 4. Unregistered image stays on default gvx sandbox runtime.
	_, rt, _, err = te.resolveCreateImage(ctx, "redis:7", "", "")
	if err != nil {
		t.Fatalf("resolveCreateImage(redis:7): %v", err)
	}
	if rt != RuntimeGVX {
		t.Fatalf("unregistered image runtime = %q, want %q", rt, RuntimeGVX)
	}

	// 5. Turning micro-VM runtime off falls back to gvx cleanly.
	if err := te.SetMicroVM(nil); err != nil {
		t.Fatalf("SetMicroVM(nil): %v", err)
	}
	_, rt, _, err = te.resolveCreateImage(ctx, "postgres:16", "", "")
	if err != nil {
		t.Fatalf("resolveCreateImage(postgres:16, off): %v", err)
	}
	if rt != RuntimeGVX {
		t.Fatalf("micro-VM disabled runtime = %q, want %q", rt, RuntimeGVX)
	}
}
