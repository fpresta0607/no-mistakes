package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestMakeBuildPrioritizesDotEnvUmamiWebsiteID(t *testing.T) {
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("NO_MISTAKES_UMAMI_WEBSITE_ID=website-from-dotenv\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, map[string]string{
		"UMAMI_WEBSITE_ID": "website-from-env",
	})

	if !strings.Contains(output, "TelemetryWebsiteID=website-from-dotenv") {
		t.Fatalf("make build output should embed .env website id, got:\n%s", output)
	}
	if strings.Contains(output, "TelemetryWebsiteID=website-from-env") {
		t.Fatalf("make build output should not prefer env website id when .env exists, got:\n%s", output)
	}
}

func TestMakeBuildUsesEnvUmamiWebsiteIDWhenDotEnvMissing(t *testing.T) {
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	output := runMakeDryBuild(t, makePath, workDir, map[string]string{
		"UMAMI_WEBSITE_ID": "website-from-env",
	})

	if !strings.Contains(output, "TelemetryWebsiteID=website-from-env") {
		t.Fatalf("make build output should embed env website id when .env is absent, got:\n%s", output)
	}
}

func TestMakeBuildEmbedsDefaultSelfHostedTelemetryConfig(t *testing.T) {
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "TelemetryHost=https://a.kunchenguid.com") {
		t.Fatalf("make build output should embed default telemetry host, got:\n%s", output)
	}
	if !strings.Contains(output, "TelemetryWebsiteID=f959e889-92f5-4121-8a1f-571b10861198") {
		t.Fatalf("make build output should embed default telemetry website id, got:\n%s", output)
	}
}

func TestMakeBuildPrioritizesDotEnvUmamiHost(t *testing.T) {
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("NO_MISTAKES_UMAMI_HOST=https://dotenv.example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, map[string]string{
		"UMAMI_HOST": "https://env.example",
	})

	if !strings.Contains(output, "TelemetryHost=https://dotenv.example") {
		t.Fatalf("make build output should embed .env telemetry host, got:\n%s", output)
	}
	if strings.Contains(output, "TelemetryHost=https://env.example") {
		t.Fatalf("make build output should not prefer env telemetry host when .env exists, got:\n%s", output)
	}
}

func TestMakeBuildIgnoresUnrelatedDotEnvEntries(t *testing.T) {
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("VERSION=from-dotenv\nNO_MISTAKES_UMAMI_WEBSITE_ID=website-from-dotenv\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "TelemetryWebsiteID=website-from-dotenv") {
		t.Fatalf("make build output should still embed dotenv website id, got:\n%s", output)
	}
	if strings.Contains(output, "/internal/buildinfo.Version=from-dotenv") {
		t.Fatalf("make build should ignore unrelated dotenv entries, got:\n%s", output)
	}
}

func TestMakeBuildStripsInlineCommentsFromDotEnvUmamiWebsiteID(t *testing.T) {
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("NO_MISTAKES_UMAMI_WEBSITE_ID=website-from-dotenv # dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "TelemetryWebsiteID=website-from-dotenv") {
		t.Fatalf("make build output should strip inline comments from dotenv website id, got:\n%s", output)
	}
	if strings.Contains(output, "TelemetryWebsiteID=website-from-dotenv # dev") {
		t.Fatalf("make build output should not embed inline comments in website id, got:\n%s", output)
	}
}

func TestMakeBuildPreservesQuotedHashInDotEnvUmamiWebsiteID(t *testing.T) {
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("NO_MISTAKES_UMAMI_WEBSITE_ID=\"website # dev\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "TelemetryWebsiteID=website # dev") {
		t.Fatalf("make build output should preserve quoted hashes in dotenv website id, got:\n%s", output)
	}
	if strings.Contains(output, "TelemetryWebsiteID=\"website") {
		t.Fatalf("make build output should not truncate quoted dotenv website id, got:\n%s", output)
	}
}

func TestMakeBuildProbesWorkWhenWindowsMakeFallsBackToCmd(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only GNU make for Windows falls back to cmd.exe")
	}

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commit := commitTestRepo(t, workDir)

	output := runMakeDryBuild(t, makePath, workDir, nil)

	for _, shellError := range []string{"is not recognized", "cannot find the path", "cannot accept the date"} {
		if strings.Contains(output, shellError) {
			t.Fatalf("make probes should not reach cmd.exe, got:\n%s", output)
		}
	}
	for _, want := range []string{"buildinfo.Version=" + commit, "buildinfo.Commit=" + commit} {
		if !strings.Contains(output, want) {
			t.Fatalf("make build output should contain %q, got:\n%s", want, output)
		}
	}
	if !regexp.MustCompile(`buildinfo\.Date=\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z `).MatchString(output) {
		t.Fatalf("make build output should embed a UTC build date, got:\n%s", output)
	}
}

func TestMakeBuildRefusesWithoutPosixShellOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only GNU make for Windows falls back to cmd.exe")
	}

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	cmd := exec.Command(makePath, "-n", "build")
	cmd.Dir = writeTestMakeWorkspace(t)
	cmd.Env = append(os.Environ(), "PATH="+pathWithout("sh.exe", "git.exe"))

	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("make -n build should fail without a POSIX shell, got:\n%s", out)
	}
	if !strings.Contains(string(out), "needs a POSIX shell") {
		t.Fatalf("make -n build should name the missing shell, got:\n%s", out)
	}
}

func TestMakeInstallPathSurvivesThePosixShellOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows paths hold backslashes")
	}

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	output := runMakeDry(t, makePath, writeTestMakeWorkspace(t), "install", nil)

	if strings.Contains(output, `\`) {
		t.Fatalf("sh strips backslashes from unquoted install paths, got:\n%s", output)
	}
}

// pathWithout returns PATH minus every directory holding one of executables.
func pathWithout(executables ...string) string {
	var kept []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		isHolder := slices.ContainsFunc(executables, func(name string) bool {
			_, err := os.Stat(filepath.Join(dir, name))
			return err == nil
		})
		if !isHolder {
			kept = append(kept, dir)
		}
	}
	return strings.Join(kept, string(os.PathListSeparator))
}

// commitTestRepo makes dir a repository with one commit and returns its short ID.
func commitTestRepo(t *testing.T, dir string) string {
	t.Helper()

	var commit string
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init"},
		{"rev-parse", "--short", "HEAD"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		commit = strings.TrimSpace(string(out))
	}
	return commit
}

func writeTestMakeWorkspace(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "Makefile"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return workDir
}

func runMakeDryBuild(t *testing.T, makePath, workDir string, extraEnv map[string]string) string {
	t.Helper()
	return runMakeDry(t, makePath, workDir, "build", extraEnv)
}

func runMakeDry(t *testing.T, makePath, workDir, target string, extraEnv map[string]string) string {
	t.Helper()

	cmd := exec.Command(makePath, "-n", target)
	cmd.Dir = workDir
	cmd.Env = filteredEnv(os.Environ(), "UMAMI_HOST", "UMAMI_WEBSITE_ID", "NO_MISTAKES_UMAMI_HOST", "NO_MISTAKES_UMAMI_WEBSITE_ID")
	if runtime.GOOS == "windows" {
		// A hosted runner has sh.exe on PATH; a default Git for Windows install does not.
		cmd.Env = append(cmd.Env, "PATH="+pathWithout("sh.exe"))
	}
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n %s failed: %v\n%s", target, err, out)
	}
	return string(out)
}
