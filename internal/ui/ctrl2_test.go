package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func TestDraftRoundTripNewCLIFlags(t *testing.T) {
	spec := sandbox.Spec{
		ID:             "cli01",
		Name:           "app",
		Image:          "alpine:3.20",
		PidsLimit:      128,
		ReadOnlyRoot:   true,
		EnvFiles:       []string{"~/.config/app.env", "/etc/shared.env"},
		GPU:            "venus",
		GPUMemoryLimit: "1g",
		Mounts: []sandbox.Mount{
			{Type: sandbox.MountVolume, Source: "pgdata", Target: "/var/lib/pg", Size: "4g"},
			{Type: sandbox.MountVolume, Source: "", Target: "/scratch", Size: "512m", Ephemeral: true},
		},
	}

	d := DraftFrom(spec)
	if d.PidsLimit != "128" {
		t.Fatalf("Draft.PidsLimit = %q, want 128", d.PidsLimit)
	}
	if !d.ReadOnlyRoot {
		t.Fatal("Draft.ReadOnlyRoot = false, want true")
	}
	if d.GPU != "venus" || d.GPUMemoryLimit != "1g" {
		t.Fatalf("Draft GPU = (%q, %q), want (venus, 1g)", d.GPU, d.GPUMemoryLimit)
	}
	if !strings.Contains(d.Volumes, "pgdata:/var/lib/pg:size=4g") || !strings.Contains(d.Volumes, ":/scratch:size=512m,ephemeral") {
		t.Fatalf("Draft.Volumes = %q", d.Volumes)
	}

	got, err := d.Spec()
	if err != nil {
		t.Fatalf("Draft.Spec() error = %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Spec.Validate() error = %v", err)
	}
	if !got.Equal(spec) {
		t.Fatalf("round-trip mismatch:\n got: %+v\nwant: %+v", got, spec)
	}

	args := got.RunArgs(sandbox.Paths{AllowVolumeAttach: true})
	for _, want := range []string{
		"--pids-limit", "128",
		"--read-only",
		"-gpu", "venus",
		"-gpu-memory-limit", "1g",
		"--allow-volume-attach",
		"--mount", "type=volume,src=pgdata,dst=/var/lib/pg,size=4g",
		"--mount", "type=volume,dst=/scratch,size=512m,ephemeral",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("RunArgs missing %q in %q", want, args)
		}
	}
}

func TestParseVolumeMountValidation(t *testing.T) {
	src, dst, ro, size, eph, err := parseVolumeMount("/scratch:ephemeral,size=256m")
	if err != nil {
		t.Fatalf("parseVolumeMount(ephemeral) error = %v", err)
	}
	if src != "" || dst != "/scratch" || ro || size != "256m" || !eph {
		t.Fatalf("got (%q,%q,%v,%q,%v)", src, dst, ro, size, eph)
	}

	if _, _, _, _, _, err := parseVolumeMount("bad-line"); err == nil {
		t.Fatal("expected error for malformed volume line")
	}
	if _, _, _, _, _, err := parseVolumeMount("data:/mnt:unknown=1"); err == nil {
		t.Fatal("expected error for unknown volume option")
	}
}

func TestEditorActionLivePidsAndVolumeAttach(t *testing.T) {
	active := sandbox.Spec{
		ID:        "live1",
		Name:      "web",
		Image:     "alpine:3.20",
		PidsLimit: 64,
		Mounts:    []sandbox.Mount{{Type: sandbox.MountVolume, Source: "v1", Target: "/v1"}},
	}
	edited := active.Clone()
	edited.PidsLimit = 256
	edited.Mounts = append(edited.Mounts, sandbox.Mount{Type: sandbox.MountVolume, Source: "v2", Target: "/v2"})

	st := sandbox.State{Status: sandbox.Running, Control: true, VolumeAttach: true}
	act := editorActionOf(st, active, true, edited)
	if !act.live || !act.hasDiff || !act.volumeAttach {
		t.Fatalf("editorActionOf = %+v, want live=true hasDiff=true volumeAttach=true", act)
	}
	if got := editorPrimaryLabel(act); got != "Save and apply" {
		t.Fatalf("editorPrimaryLabel = %q, want Save and apply", got)
	}
	if cap := editorCaption(act); !strings.Contains(cap, "volume/bind/tmpfs") {
		t.Fatalf("editorCaption = %q, want mention of volume/bind/tmpfs", cap)
	}
}
