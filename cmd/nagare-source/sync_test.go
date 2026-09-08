package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nagare-project/Nagare_Source/internal/repository"
)

func TestVerifyApprovedCheckoutRequiresCleanDescendantAndUnchangedLicense(t *testing.T) {
	checkout := t.TempDir()
	licenseData := []byte("fixture license\n")
	if err := os.WriteFile(filepath.Join(checkout, "LICENSE"), licenseData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(checkout, "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "rules", "source.json"), []byte(`{"name":"fixture"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.name", "Nagare Source Test")
	runGit(t, checkout, "config", "user.email", "test@example.invalid")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/example/rules.git")
	runGit(t, checkout, "add", "LICENSE", "rules/source.json")
	runGit(t, checkout, "commit", "-m", "fixture")
	revision := runGit(t, checkout, "rev-parse", "HEAD")
	digest := sha256.Sum256(licenseData)
	approval := repository.UpstreamApproval{
		ID: "fixture", Repository: "https://github.com/example/rules", ReviewedRevision: revision,
		InputPath: "rules", License: repository.UpstreamLicense{
			UpstreamPath: "LICENSE", NoticeSHA256: "sha256:" + hex.EncodeToString(digest[:]),
		},
	}
	actualRevision, inputPath, err := verifyApprovedCheckout(checkout, approval)
	expectedInput, resolveErr := filepath.EvalSymlinks(filepath.Join(checkout, "rules"))
	if err != nil || resolveErr != nil || actualRevision != revision || inputPath != expectedInput {
		t.Fatalf("clean approved checkout was rejected: revision=%q input=%q err=%v", actualRevision, inputPath, err)
	}

	if err := os.WriteFile(filepath.Join(checkout, "untracked"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifyApprovedCheckout(checkout, approval); err == nil || !strings.Contains(err.Error(), "clean") {
		t.Fatalf("dirty checkout returned unexpected error: %v", err)
	}
	if err := os.Remove(filepath.Join(checkout, "untracked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "LICENSE"), []byte("changed license\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, checkout, "add", "LICENSE")
	runGit(t, checkout, "commit", "-m", "change license")
	if _, _, err := verifyApprovedCheckout(checkout, approval); err == nil || !strings.Contains(err.Error(), "license changed") {
		t.Fatalf("changed license returned unexpected error: %v", err)
	}
}

func TestInstallApprovedSourcesRollsBackInvalidReplacement(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		repository.SourceSchemaName, repository.RequestSchemaName, repository.CandidateSchemaName,
		repository.IndexSchemaName, repository.HealthSchemaName, repository.ApprovalsSchemaName,
	} {
		copyTestFile(t, filepath.Join(repositoryRootForCommandTest(t), "schema", name), filepath.Join(root, "schema", name))
	}
	output := filepath.Join(root, "sources", "upstreams", "fixture")
	if err := os.MkdirAll(output, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(output, "keep.txt")
	if err := os.WriteFile(marker, []byte("previous\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stagingRoot := t.TempDir()
	generated := filepath.Join(stagingRoot, "generated")
	if err := os.MkdirAll(filepath.Join(generated, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(generated, "web", "broken.yaml"), []byte("not: [valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installApprovedSources(root, output, generated); err == nil || !strings.Contains(err.Error(), "validate synchronized sources") {
		t.Fatalf("invalid replacement returned unexpected error: %v", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "previous\n" {
		t.Fatalf("previous approved sources were not restored: data=%q err=%v", data, err)
	}
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func copyTestFile(t *testing.T, source, destination string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func repositoryRootForCommandTest(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}
