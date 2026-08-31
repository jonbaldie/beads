package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// runFixes executes auto-fix operations for fixable preflight checks.
func runFixes(jsonOutput bool) error {
	results, hasError := collectFixResults()
	if jsonOutput {
		return outputFixResults(results, hasError)
	}

	printFixResults(results)
	if hasError {
		return SilentExit()
	}
	return nil
}

func collectFixResults() ([]fixResult, bool) {
	nixResult, nixError := buildNixFixResult()
	versionResult, versionError := buildVersionFixResult()
	return []fixResult{nixResult, versionResult}, nixError || versionError
}

func buildNixFixResult() (fixResult, bool) {
	nixFixed, nixOld, nixNew, nixErr := fixNixHash()
	result := fixResult{Name: "Nix vendorHash"}
	if nixErr != nil {
		result.Error = nixErr.Error()
		return result, true
	}
	if nixFixed {
		result.Fixed = true
		result.Detail = fmt.Sprintf("%s → %s", nixOld, nixNew)
		return result, false
	}
	result.Skipped = true
	return result, false
}

func buildVersionFixResult() (fixResult, bool) {
	versionFixed, versionOld, versionNew, versionErr := fixVersionSync()
	result := fixResult{Name: "Version sync"}
	if versionErr != nil {
		result.Error = versionErr.Error()
		return result, true
	}
	if versionFixed {
		result.Fixed = true
		result.Detail = fmt.Sprintf("version.go: %s → %s", versionOld, versionNew)
		return result, false
	}
	result.Skipped = true
	return result, false
}

func outputFixResults(results []fixResult, hasError bool) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(results); err != nil {
		fmt.Fprintf(os.Stderr, "Error encoding fix results: %v\n", err)
	}
	if hasError {
		return SilentExit()
	}
	return nil
}

func printFixResults(results []fixResult) {
	for _, result := range results {
		printFixResult(result)
	}
}

func printFixResult(result fixResult) {
	switch {
	case result.Error != "":
		fmt.Printf("✗ %s\n  %s\n\n", result.Name, result.Error)
	case result.Fixed:
		fmt.Printf("✓ %s\n", result.Name)
		if result.Detail != "" {
			fmt.Printf("  %s\n", result.Detail)
		}
		fmt.Println()
	default:
		fmt.Printf("- %s (nothing to fix)\n\n", result.Name)
	}
}

// fixNixHash computes and updates the vendorHash in default.nix.
// It uses a sentinel hash to trigger nix to report the correct hash.
// Returns (fixed, oldHash, newHash, err).
func fixNixHash() (bool, string, string, error) {
	if !goSumHasChanges() {
		return false, "", "", nil
	}
	return updateNixHash()
}

func goSumHasChanges() bool {
	cmd := exec.Command("git", "diff", "--name-only", "HEAD", "--", "go.sum")
	out1, _ := cmd.Output()
	cmd = exec.Command("git", "diff", "--name-only", "--cached", "--", "go.sum")
	out2, _ := cmd.Output()
	return len(strings.TrimSpace(string(out1))) > 0 || len(strings.TrimSpace(string(out2))) > 0
}

func updateNixHash() (bool, string, string, error) {
	if _, err := exec.LookPath("nix"); err != nil {
		return false, "", "", fmt.Errorf(
			"nix not found in PATH\n  Manual fix:\n" +
				"    1. Edit default.nix: set vendorHash = \"sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n" +
				"    2. Run: nix build .#default\n" +
				"    3. Copy the 'got:' hash from the error into default.nix",
		)
	}

	nixPath := "default.nix"
	content, nixPerm, err := readNixHashFile(nixPath)
	if err != nil {
		return false, "", "", err
	}

	loc, oldHash, err := findNixVendorHash(content)
	if err != nil {
		return false, "", "", err
	}
	return probeNixHash(nixPath, content, nixPerm, loc, oldHash)
}

func readNixHashFile(path string) ([]byte, os.FileMode, error) {
	nixInfo, err := os.Stat(path)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot stat default.nix: %v", err)
	}
	content, err := os.ReadFile(path) //nolint:gosec // G304: caller passes the fixed repository default.nix path.
	if err != nil {
		return nil, 0, fmt.Errorf("cannot read default.nix: %v", err)
	}
	return content, nixInfo.Mode().Perm(), nil
}

func findNixVendorHash(content []byte) ([]int, string, error) {
	re := regexp.MustCompile(`(vendorHash\s*=\s*)"([^"]+)"`)
	loc := re.FindSubmatchIndex(content)
	if loc == nil {
		return nil, "", fmt.Errorf("vendorHash not found in default.nix")
	}
	return loc, string(content[loc[4]:loc[5]]), nil
}

func probeNixHash(path string, content []byte, perm os.FileMode, loc []int, oldHash string) (bool, string, string, error) {
	const sentinel = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	probed := replaceNixHash(content, loc, sentinel)
	if err := os.WriteFile(path, probed, perm); err != nil {
		return false, "", "", fmt.Errorf("cannot write default.nix: %v", err)
	}

	restored := false
	defer func() {
		if !restored {
			_ = os.WriteFile(path, content, perm)
		}
	}()

	newHash, err := runNixHashBuild(sentinel)
	if err != nil {
		return false, oldHash, "", err
	}

	if newHash == oldHash {
		_ = os.WriteFile(path, content, perm)
		restored = true
		return false, oldHash, newHash, nil
	}

	updated := replaceNixHash(content, loc, newHash)
	if err := os.WriteFile(path, updated, perm); err != nil {
		return false, oldHash, newHash, fmt.Errorf("cannot update default.nix: %v", err)
	}
	restored = true
	return true, oldHash, newHash, nil
}

func replaceNixHash(content []byte, loc []int, replacement string) []byte {
	return append(append([]byte{}, content[:loc[4]]...), append([]byte(replacement), content[loc[5]:]...)...)
}

func runNixHashBuild(sentinel string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	nixCmd := exec.CommandContext(ctx, "nix", "build", ".#default", "--no-link")
	nixOut, _ := nixCmd.CombinedOutput()

	// Nix prints the correct hash in lines like "got:    sha256-..."
	hashRe := regexp.MustCompile(`got:\s+(sha256-[A-Za-z0-9+/]+=)`)
	matches := hashRe.FindSubmatch(nixOut)
	if matches != nil {
		return string(matches[1]), nil
	}

	// Fallback: pick any sha256-... that isn't our sentinel.
	altRe := regexp.MustCompile(`sha256-([A-Za-z0-9+/]+=)`)
	for _, match := range altRe.FindAllSubmatch(nixOut, -1) {
		hash := "sha256-" + string(match[1])
		if hash != sentinel {
			return hash, nil
		}
	}
	return "", fmt.Errorf("could not parse correct hash from nix output:\n%s", string(nixOut))
}

// fixVersionSync updates cmd/bd/version.go to match the version in default.nix.
// default.nix is the source of truth. Returns (fixed, oldVersion, newVersion, err).
func fixVersionSync() (bool, string, string, error) {
	vgoPath := "cmd/bd/version.go"
	vgoInfo, err := os.Stat(vgoPath)
	if err != nil {
		return false, "", "", fmt.Errorf("cannot read cmd/bd/version.go: %v", err)
	}
	vgoPerm := vgoInfo.Mode().Perm()

	vgoContent, err := os.ReadFile(vgoPath)
	if err != nil {
		return false, "", "", fmt.Errorf("cannot read cmd/bd/version.go: %v", err)
	}

	vgoRe := regexp.MustCompile(`(Version\s*=\s*)"([^"]+)"`)
	vgoLoc := vgoRe.FindSubmatchIndex(vgoContent)
	if vgoLoc == nil {
		return false, "", "", fmt.Errorf("cannot parse Version from cmd/bd/version.go")
	}
	oldVersion := string(vgoContent[vgoLoc[4]:vgoLoc[5]])

	nixContent, err := os.ReadFile("default.nix")
	if err != nil {
		// No default.nix — nothing to sync against
		return false, "", "", nil
	}

	nixRe := regexp.MustCompile(`version\s*=\s*"([^"]+)"`)
	nixM := nixRe.FindSubmatch(nixContent)
	if nixM == nil {
		return false, "", "", fmt.Errorf("cannot parse version from default.nix")
	}
	newVersion := string(nixM[1])

	if oldVersion == newVersion {
		return false, oldVersion, newVersion, nil
	}

	updated := append(append([]byte{}, vgoContent[:vgoLoc[4]]...), append([]byte(newVersion), vgoContent[vgoLoc[5]:]...)...)
	if err := os.WriteFile(vgoPath, updated, vgoPerm); err != nil {
		return false, oldVersion, newVersion, fmt.Errorf("cannot update cmd/bd/version.go: %v", err)
	}
	return true, oldVersion, newVersion, nil
}
