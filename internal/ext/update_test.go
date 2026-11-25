package ext

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerUpdateGit_PullsCommitAndEscalatesPermissions(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary unavailable")
	}

	tmp := t.TempDir()
	bareRepo := filepath.Join(tmp, "remote.git")
	workRepo := filepath.Join(tmp, "work")

	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	runGit(tmp, "init", "--bare", bareRepo)
	runGit(tmp, "clone", bareRepo, workRepo)
	runGit(workRepo, "checkout", "-b", "main")

	writeAndPush := func(tomlBody, msg string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workRepo, ManifestName), []byte(tomlBody), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workRepo, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(workRepo, "add", ".")
		runGit(workRepo, "commit", "-m", msg)
		runGit(workRepo, "push", "-u", "origin", "main")
	}

	writeAndPush(`
schema_version = 1
id = "git-upd"
version = "0.1.0"
publisher = "acme"
name = "Git Upd"
permissions = ["storage"]

[runtime]
type = "sandbox"
dockerfile = "Dockerfile"
`, "v0.1.0")

	dataDir := t.TempDir()
	mgr, err := NewManager(Config{
		DataDir: dataDir,
		ImageBuilder: fakeImageBuilder{buildFn: func(ctx context.Context, req BuildRequest, onLine func(string)) (string, error) {
			return "sha256:fake-img", nil
		}},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	defer mgr.Close(2 * time.Second)

	repoURL := "file://" + filepath.ToSlash(bareRepo)
	prep, err := prepareGitInstall(context.Background(), dataDir, repoURL, "main", true, nil)
	if err != nil {
		t.Fatalf("prepareGitInstall: %v", err)
	}
	info, err := mgr.registerInstalled(prep.man, prep.spec, func() error {
		return commitGitInstall(dataDir, prep)
	})
	if err != nil {
		t.Fatalf("registerInstalled: %v", err)
	}
	if info.Version != "0.1.0" {
		t.Fatalf("initial version = %q, want 0.1.0", info.Version)
	}
	// Approve and mark disabled so updateGit won't try to launch a real rungvx sandbox.
	if err := mgr.grants.Approve("git-upd", prep.man, prep.spec.Commit); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := mgr.Disable("git-upd"); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	// 1. No new commit -> already up to date.
	updated, needsApproval, err := mgr.updateGit("git-upd", true)
	if err != nil || updated || needsApproval {
		t.Fatalf("updateGit (no changes) = (%v, %v, %v), want (false, false, nil)", updated, needsApproval, err)
	}

	// 2. Push v0.2.0 with same permissions + [i18n.ko] -> updates & rebuilds without waiting.
	writeAndPush(`
schema_version = 1
id = "git-upd"
version = "0.2.0"
publisher = "acme"
name = "Git Upd"
permissions = ["storage"]

[runtime]
type = "sandbox"
dockerfile = "Dockerfile"

[i18n.ko]
description = "한국어 설명"
`, "v0.2.0")

	updated, needsApproval, err = mgr.updateGit("git-upd", true)
	if err != nil || !updated || needsApproval {
		t.Fatalf("updateGit (same perms) = (%v, %v, %v), want (true, false, nil)", updated, needsApproval, err)
	}
	gotList := mgr.List()
	if len(gotList) != 1 || gotList[0].Version != "0.2.0" || gotList[0].LocalizedDescription("ko") != "한국어 설명" {
		t.Fatalf("post-update ExtensionInfo = %+v", gotList)
	}

	// 3. Push v0.3.0 requesting a NEW permission ("network") -> pauses in StateWaiting!
	writeAndPush(`
schema_version = 1
id = "git-upd"
version = "0.3.0"
publisher = "acme"
name = "Git Upd"
permissions = ["storage", "network"]

[runtime]
type = "sandbox"
dockerfile = "Dockerfile"
`, "v0.3.0")

	updated, needsApproval, err = mgr.updateGit("git-upd", true)
	if err != nil || !updated || !needsApproval {
		t.Fatalf("updateGit (new perm) = (%v, %v, %v), want (true, true, nil)", updated, needsApproval, err)
	}
	gotList = mgr.List()
	if len(gotList) != 1 || gotList[0].State != StateWaiting {
		t.Fatalf("expected StateWaiting after permission escalation, got %+v", gotList)
	}
}

type fakeImageBuilder struct {
	buildFn func(ctx context.Context, req BuildRequest, onLine func(string)) (string, error)
}

func (f fakeImageBuilder) Build(ctx context.Context, req BuildRequest, onLine func(string)) (string, error) {
	if f.buildFn != nil {
		return f.buildFn(ctx, req, onLine)
	}
	return "sha256:fake", nil
}
