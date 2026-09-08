package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nagare-project/Nagare_Source/internal/repository"
)

var gitRevisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

func syncApproved(arguments []string) error {
	flags := flag.NewFlagSet("sync-approved", flag.ContinueOnError)
	root := flags.String("root", ".", "Nagare Source repository root")
	id := flags.String("id", "", "approved upstream ID")
	checkout := flags.String("checkout", "", "clean local checkout of the approved upstream")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *id == "" || *checkout == "" {
		return errors.New("--id and --checkout are required")
	}
	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	absoluteCheckout, err := filepath.Abs(*checkout)
	if err != nil {
		return err
	}
	approval, err := repository.ApprovedUpstream(absoluteRoot, *id)
	if err != nil {
		return err
	}
	if !approval.Redistribution.Rules || !approval.Redistribution.NormalizedSources {
		return fmt.Errorf("upstream %q is not approved for rule and normalized-source redistribution", approval.ID)
	}
	revision, inputPath, err := verifyApprovedCheckout(absoluteCheckout, approval)
	if err != nil {
		return err
	}
	outputPath := filepath.Join(absoluteRoot, filepath.FromSlash(approval.OutputPath))
	parent := filepath.Dir(outputPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(parent, ".sync-"+approval.ID+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	generated := filepath.Join(temporary, "generated")
	diagnostics := filepath.Join(temporary, "diagnostics.jsonl")
	upstreamTemplate := strings.ReplaceAll(approval.SourceURLTemplate, "{revision}", revision)
	if err := importSources([]string{
		approval.Ecosystem,
		"--root", absoluteRoot,
		"--input", inputPath,
		"--upstream", upstreamTemplate,
		"--license", approval.License.SPDX,
		"--out", generated,
		"--overlays", filepath.Join(absoluteRoot, "overlays"),
		"--diagnostics", diagnostics,
	}); err != nil {
		return fmt.Errorf("import approved upstream %q: %w", approval.ID, err)
	}
	if err := installApprovedSources(absoluteRoot, outputPath, generated); err != nil {
		return err
	}
	fmt.Printf("synced approved upstream %s at %s into %s\n", approval.ID, revision, outputPath)
	return nil
}

func verifyApprovedCheckout(checkout string, approval repository.UpstreamApproval) (string, string, error) {
	info, err := os.Stat(checkout)
	if err != nil || !info.IsDir() {
		return "", "", errors.New("approved upstream checkout does not exist or is not a directory")
	}
	remote, err := gitOutput(checkout, "remote", "get-url", "origin")
	if err != nil {
		return "", "", fmt.Errorf("read upstream git remote: %w", err)
	}
	if normalizeGitRemote(remote) != normalizeGitRemote(approval.Repository) {
		return "", "", fmt.Errorf("upstream git remote %q does not match approved repository", remote)
	}
	status, err := gitOutput(checkout, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return "", "", fmt.Errorf("read upstream git status: %w", err)
	}
	if status != "" {
		return "", "", errors.New("approved upstream checkout must be clean")
	}
	revision, err := gitOutput(checkout, "rev-parse", "HEAD")
	if err != nil || !gitRevisionPattern.MatchString(revision) {
		return "", "", errors.New("approved upstream HEAD is not a full Git revision")
	}
	if revision != approval.ReviewedRevision {
		command := exec.Command("git", "merge-base", "--is-ancestor", approval.ReviewedRevision, revision)
		command.Dir = checkout
		if err := command.Run(); err != nil {
			return "", "", errors.New("upstream HEAD does not descend from the reviewed revision; fetch full history or review the rewritten history")
		}
	}
	licensePath, err := pathWithinCheckout(checkout, approval.License.UpstreamPath, false)
	if err != nil {
		return "", "", fmt.Errorf("resolve upstream license: %w", err)
	}
	licenseData, err := os.ReadFile(licensePath)
	if err != nil {
		return "", "", fmt.Errorf("read upstream license: %w", err)
	}
	digest := sha256.Sum256(licenseData)
	actual := "sha256:" + hex.EncodeToString(digest[:])
	if actual != approval.License.NoticeSHA256 {
		return "", "", fmt.Errorf("upstream license changed: got %s, approval expects %s", actual, approval.License.NoticeSHA256)
	}
	inputPath, err := pathWithinCheckout(checkout, approval.InputPath, true)
	if err != nil {
		return "", "", fmt.Errorf("resolve upstream input: %w", err)
	}
	return revision, inputPath, nil
}

func gitOutput(directory string, arguments ...string) (string, error) {
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func normalizeGitRemote(value string) string {
	value = strings.TrimSpace(strings.TrimSuffix(value, "/"))
	return strings.TrimSuffix(value, ".git")
}

func pathWithinCheckout(checkout, name string, directory bool) (string, error) {
	root, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return "", err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", errors.New("path escapes the upstream checkout")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if directory != info.IsDir() {
		if directory {
			return "", errors.New("path is not a directory")
		}
		return "", errors.New("path is not a regular file")
	}
	if !directory && !info.Mode().IsRegular() {
		return "", errors.New("path is not a regular file")
	}
	return path, nil
}

func installApprovedSources(root, outputPath, generated string) error {
	if info, err := os.Lstat(outputPath); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("approved source output must be a real directory")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	backup := filepath.Join(filepath.Dir(generated), "previous")
	hadPrevious := false
	if _, err := os.Stat(outputPath); err == nil {
		if err := os.Rename(outputPath, backup); err != nil {
			return fmt.Errorf("stage previous approved sources: %w", err)
		}
		hadPrevious = true
	}
	if err := os.Rename(generated, outputPath); err != nil {
		if hadPrevious {
			_ = os.Rename(backup, outputPath)
		}
		return fmt.Errorf("install approved sources: %w", err)
	}
	validator, err := repository.NewValidator(root)
	if err == nil {
		_, err = repository.LoadSources(root, validator)
	}
	if err != nil {
		_ = os.RemoveAll(outputPath)
		if hadPrevious {
			_ = os.Rename(backup, outputPath)
		}
		return fmt.Errorf("validate synchronized sources: %w", err)
	}
	if hadPrevious {
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("remove previous approved sources: %w", err)
		}
	}
	return nil
}
