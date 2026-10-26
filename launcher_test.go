package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// launcherCopy copies the launcher into a temp repo with one Go source and,
// when withBinary is true, a prebuilt binary stub older than that source.
func launcherCopy(t *testing.T, withBinary bool) string {
	t.Helper()
	root := t.TempDir()
	script, err := os.ReadFile("claude-sessions")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "claude-sessions"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	if withBinary {
		bin := filepath.Join(root, "bin", "claude-sessions-"+runtime.GOOS+"-"+runtime.GOARCH)
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho ran \"$@\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(bin, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func runLauncher(t *testing.T, root string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := exec.Command(filepath.Join(root, "claude-sessions"), args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

func TestTheLauncherRunsAStaleBinaryWhenGoIsMissing(t *testing.T) {
	if _, err := exec.LookPath("/usr/bin/go"); err == nil {
		t.Skip("go is on /usr/bin")
	}
	root := launcherCopy(t, true)

	stdout, stderr, err := runLauncher(t, root, "--ingest", "pc")
	if err != nil {
		t.Fatalf("launcher failed: %v: %s", err, stderr)
	}
	if stdout != "ran --ingest pc\n" {
		t.Errorf("stdout = %q, want only the binary's output", stdout)
	}
	if stderr != "" {
		t.Errorf("--ingest got a note on stderr: %q", stderr)
	}

	_, stderr, err = runLauncher(t, root, "--export")
	if err != nil || stderr != "" {
		t.Errorf("--export: err %v, stderr %q", err, stderr)
	}

	stdout, stderr, err = runLauncher(t, root)
	if err != nil {
		t.Fatalf("launcher failed: %v: %s", err, stderr)
	}
	if stdout != "ran\n" {
		t.Errorf("stdout = %q, want only the binary's output", stdout)
	}
	if stderr == "" {
		t.Error("no note on stderr about the skipped rebuild")
	}
}

func TestTheLauncherFailsWithoutGoOrABinary(t *testing.T) {
	if _, err := exec.LookPath("/usr/bin/go"); err == nil {
		t.Skip("go is on /usr/bin")
	}
	root := launcherCopy(t, false)
	_, stderr, err := runLauncher(t, root)
	if err == nil {
		t.Fatal("launcher succeeded with neither Go nor a binary")
	}
	if !bytes.Contains([]byte(stderr), []byte("Go is required")) {
		t.Errorf("stderr = %q", stderr)
	}
}
