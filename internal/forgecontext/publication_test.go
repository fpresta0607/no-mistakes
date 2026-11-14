package forgecontext

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

var publicationFakeDir string
var publicationFakeError error
var publicationFakeOnce sync.Once

func TestMain(m *testing.M) {
	os.Unsetenv("GIT_CONFIG_COUNT")
	os.Unsetenv("GIT_CONFIG_PARAMETERS")
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			os.Unsetenv(key)
		}
	}
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	code := m.Run()
	if publicationFakeDir != "" {
		os.RemoveAll(publicationFakeDir)
	}
	os.Exit(code)
}

func publicationFixture(t *testing.T) (PublicationSelection, string) {
	t.Helper()
	publicationFakeOnce.Do(func() {
		publicationFakeDir, publicationFakeError = os.MkdirTemp("", "nm-publication-fake-")
		if publicationFakeError != nil {
			return
		}
		source := filepath.Join(publicationFakeDir, "main.go")
		if publicationFakeError = os.WriteFile(source, []byte(PUBLICATION_FAKE_SOURCE), 0o600); publicationFakeError != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(publicationFakeDir, "fake.exe"), source)
		shellenv.ConfigureShellCommand(command)
		publicationFakeError = shellenv.RunShellCommand(command)
	})
	if publicationFakeError != nil {
		t.Fatalf("build publication fake: %v", publicationFakeError)
	}
	// Shell quoting must survive spaces, apostrophes and shell metacharacters.
	binDir := filepath.Join(t.TempDir(), "gh's tools $literal")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(publicationFakeDir, "fake.exe"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gh", "git"} {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		if err := os.WriteFile(filepath.Join(binDir, name), data, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte("github.com:\n  user: publisher\n  users:\n    publisher:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "calls.ndjson")
	t.Setenv("NM_PUBLICATION_TEST_LOG", log)
	t.Setenv("NM_PUBLICATION_TEST_SECRET", "private-"+filepath.Base(t.TempDir()))
	t.Setenv("NM_PUBLICATION_TEST_API_LOGIN", "publisher")
	t.Setenv("NM_PUBLICATION_TEST_GIT_LOGIN", "publisher")
	t.Setenv("NM_PUBLICATION_TEST_RESPONSE", "")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent-config"))
	t.Setenv("GIT_CONFIG", "")
	return PublicationSelection{ConfigDir: dir, ExpectedLogin: "publisher"}, log
}

func TestResolvePublicationIsAttemptLocal(t *testing.T) {
	selection, _ := publicationFixture(t)
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GIT_ASKPASS", "SSH_ASKPASS", "GH_DEBUG", "GIT_TRACE", "GIT_TRACE_CURL"} {
		t.Setenv(key, os.Getenv("NM_PUBLICATION_TEST_SECRET"))
	}
	t.Setenv("GIT_CONFIG_COUNT", "4")
	for index, pair := range [][2]string{
		{"safe.directory", "/trusted"},
		{"core.hooksPath", "/hooks"},
		{"credential.helper", os.Getenv("NM_PUBLICATION_TEST_SECRET")},
		{"http.https://github.com/.extraheader", os.Getenv("NM_PUBLICATION_TEST_SECRET")},
	} {
		t.Setenv("GIT_CONFIG_KEY_"+strconv.Itoa(index), pair[0])
		t.Setenv("GIT_CONFIG_VALUE_"+strconv.Itoa(index), pair[1])
	}
	original, err := Resolve(context.Background(), config.ForgeProfiles{"github.com": {GHConfigDir: selection.ConfigDir}}, "https://github.com/parent/repo.git", "")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(original)

	publication, err := ResolvePublication(context.Background(), "https://github.com/parent/repo.git", "https://github.com/fork/repo.git", selection)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(publication)
	if strings.Contains(string(encoded), os.Getenv("NM_PUBLICATION_TEST_SECRET")) {
		t.Fatal("publication metadata retained a credential")
	}
	environment := envMap(publication.Environment.Apply(os.Environ()))
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "SSH_ASKPASS", "GH_DEBUG", "GIT_TRACE", "GIT_TRACE_CURL"} {
		if environment[key] != "" {
			t.Fatalf("credential or tracing environment survived: %s", key)
		}
	}
	if environment["GH_CONFIG_DIR"] != selection.ConfigDir || environment["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatal("publication did not isolate config and prompts")
	}
	settings := publicationSettings(t, environment)
	if settings["safe.directory"] != "/trusted" || settings["core.hookspath"] != "/hooks" {
		t.Fatal("unrelated Git runtime settings were lost")
	}
	after, _ := json.Marshal(original)
	if string(before) != string(after) || os.Getenv("GH_TOKEN") != os.Getenv("NM_PUBLICATION_TEST_SECRET") || os.Getenv("GIT_CONFIG_COUNT") != "4" {
		t.Fatal("publication modified the ordinary run context or ambient environment")
	}
}

func TestResolvePublicationRefusesInvalidSelection(t *testing.T) {
	selection, _ := publicationFixture(t)
	for _, test := range []struct {
		name     string
		upstream string
		fork     string
		dir      string
		login    string
	}{
		{"missing_dir", "https://github.com/parent/repo", "", "", "publisher"},
		{"relative_dir", "https://github.com/parent/repo", "", "relative", "publisher"},
		{"nonexistent_dir", "https://github.com/parent/repo", "", filepath.Join(selection.ConfigDir, "missing"), "publisher"},
		{"missing_login", "https://github.com/parent/repo", "", selection.ConfigDir, " "},
		{"wrong_login", "https://github.com/parent/repo", "", selection.ConfigDir, "other"},
		{"ssh", "git@github.com:parent/repo", "", selection.ConfigDir, "publisher"},
		{"http", "http://github.com/parent/repo", "", selection.ConfigDir, "publisher"},
		{"credentials", "https://someone:private@github.com/parent/repo", "", selection.ConfigDir, "publisher"},
		{"query", "https://github.com/parent/repo?token=private", "", selection.ConfigDir, "publisher"},
		{"missing_repository", "https://github.com/parent", "", selection.ConfigDir, "publisher"},
		{"not_a_directory", "https://github.com/parent/repo", "", filepath.Join(selection.ConfigDir, "hosts.yml"), "publisher"},
		{"different_host", "https://github.com/parent/repo", "https://elsewhere.test/fork/repo", selection.ConfigDir, "publisher"},
		{"other_provider", "https://gitlab.com/parent/repo", "", selection.ConfigDir, "publisher"},
		{"empty_url", "", "", selection.ConfigDir, "publisher"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ResolvePublication(context.Background(), test.upstream, test.fork, PublicationSelection{ConfigDir: test.dir, ExpectedLogin: test.login}); err == nil {
				t.Fatal("invalid selection succeeded")
			} else if strings.Contains(err.Error(), "private") {
				t.Fatal("selection refusal exposed input credentials")
			}
		})
	}
	for _, hosts := range []string{
		"github.com:\n  user: publisher\n  users:\n    publisher:\n    other:\n",
		"github.com:\n  user: publisher\n  oauth_token: [malformed-private\n",
	} {
		if err := os.WriteFile(filepath.Join(selection.ConfigDir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolvePublication(context.Background(), "https://github.com/parent/repo", "", selection); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid profile was accepted or exposed credential data")
		}
	}
}

func TestResolvePublicationRefusesAmbiguousGitEnvironment(t *testing.T) {
	selection, _ := publicationFixture(t)
	for _, test := range []struct{ name, key, value string }{
		{"opaque_parameters", "GIT_CONFIG_PARAMETERS", "'credential.helper=secret'"},
		{"invalid_count", "GIT_CONFIG_COUNT", "-1"},
		{"missing_pair", "GIT_CONFIG_COUNT", "1"},
		{"orphan_key", "GIT_CONFIG_KEY_2", "credential.helper"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			if _, err := ResolvePublication(context.Background(), "https://github.com/parent/repo", "", selection); err == nil {
				t.Fatal("ambiguous credential environment succeeded")
			}
		})
	}
}

func TestResolvePublicationRefusesIndirectGitConfiguration(t *testing.T) {
	selection, _ := publicationFixture(t)
	for _, key := range []string{"include.path", "includeIf.gitdir:/work.path", "url.https://elsewhere.test/.insteadOf", "url.file:///fixture/.insteadOf", "url.file:///fixture/.pushInsteadOf", "url." + filepath.ToSlash(t.TempDir()) + "/.insteadOf"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("GIT_CONFIG_KEY_0", key)
			t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/")
			if _, err := ResolvePublication(context.Background(), "https://github.com/parent/repo", "", selection); err == nil {
				t.Fatal("indirect credential configuration succeeded")
			}
		})
	}
}

func TestVerifyPublicationProvesBothIdentities(t *testing.T) {
	selection, log := publicationFixture(t)
	t.Setenv("GH_TOKEN", os.Getenv("NM_PUBLICATION_TEST_SECRET"))
	t.Setenv("GH_ENTERPRISE_TOKEN", os.Getenv("NM_PUBLICATION_TEST_SECRET"))
	publication, err := ResolvePublication(context.Background(), "https://github.com/parent/repo", "", selection)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPublication(context.Background(), t.TempDir(), publication, "Publisher"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), os.Getenv("NM_PUBLICATION_TEST_SECRET")) {
		t.Fatal("subprocess argv or metadata exposed a credential")
	}
	metadata, err := json.Marshal(publication)
	if err != nil || strings.Contains(string(metadata), os.Getenv("NM_PUBLICATION_TEST_SECRET")) {
		t.Fatal("verification saved a credential in the publication context")
	}
	if os.Getenv("GH_TOKEN") != os.Getenv("NM_PUBLICATION_TEST_SECRET") {
		t.Fatal("verification changed the ambient account")
	}
	if !strings.Contains(string(data), `"kind":"api-config"`) || !strings.Contains(string(data), `"kind":"credential-fill"`) || !strings.Contains(string(data), `"kind":"api-git"`) {
		t.Fatal("verification did not independently prove both effective identities")
	}
}

func TestVerifyPublicationRefusesWrongOrMalformedIdentity(t *testing.T) {
	selection, _ := publicationFixture(t)
	publication, err := ResolvePublication(context.Background(), "https://github.com/parent/repo", "", selection)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, key, value string }{
		{"api_mismatch", "NM_PUBLICATION_TEST_API_LOGIN", "other"},
		{"git_mismatch", "NM_PUBLICATION_TEST_GIT_LOGIN", "other"},
		{"duplicate", "NM_PUBLICATION_TEST_RESPONSE", "duplicate"},
		{"missing", "NM_PUBLICATION_TEST_RESPONSE", "missing"},
		{"wrong_host", "NM_PUBLICATION_TEST_RESPONSE", "wrong-host"},
		{"unknown_field", "NM_PUBLICATION_TEST_RESPONSE", "unknown-field"},
		{"split_response", "NM_PUBLICATION_TEST_RESPONSE", "split-response"},
		{"malformed", "NM_PUBLICATION_TEST_RESPONSE", "malformed"},
		{"api_failure", "NM_PUBLICATION_TEST_RESPONSE", "api-failure"},
		{"config_failure", "NM_PUBLICATION_TEST_RESPONSE", "config-failure"},
		{"header_bypass", "NM_PUBLICATION_TEST_RESPONSE", "header-bypass"},
		{"rewrite_bypass", "NM_PUBLICATION_TEST_RESPONSE", "rewrite-bypass"},
		{"network_file_bypass", "NM_PUBLICATION_TEST_RESPONSE", "network-file-bypass"},
		{"failed_command", "NM_PUBLICATION_TEST_RESPONSE", "failure"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			if err := VerifyPublication(context.Background(), t.TempDir(), publication, selection.ExpectedLogin); err == nil {
				t.Fatal("unproven identity succeeded")
			} else if strings.Contains(err.Error(), os.Getenv("NM_PUBLICATION_TEST_SECRET")) {
				t.Fatal("verification error exposed a credential")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := VerifyPublication(ctx, t.TempDir(), publication, selection.ExpectedLogin); err == nil {
		t.Fatal("cancelled verification succeeded")
	}
	if err := VerifyPublication(context.Background(), t.TempDir(), nil, selection.ExpectedLogin); err == nil {
		t.Fatal("missing publication context succeeded")
	}
	ordinary, err := Resolve(context.Background(), config.ForgeProfiles{"github.com": {GHConfigDir: selection.ConfigDir}}, "https://github.com/parent/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPublication(context.Background(), t.TempDir(), ordinary, selection.ExpectedLogin); err == nil {
		t.Fatal("ordinary context without a frozen Git helper succeeded")
	}
}

func TestVerifyPublicationUsesFrozenHelperWithRealGit(t *testing.T) {
	selection, log := publicationFixture(t)
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	publication, err := ResolvePublication(context.Background(), "https://github.com/parent/repo", "", selection)
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	command := exec.CommandContext(context.Background(), realGit, "init", workDir)
	shellenv.ConfigureShellCommand(command)
	if err := shellenv.RunShellCommand(command); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".git", "config"), []byte("[credential]\n helper = !exit 87\n[credential \"https://github.com/parent\"]\n helper = !exit 88\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrongBin := t.TempDir()
	wrongGH := "gh"
	if runtime.GOOS == "windows" {
		wrongGH += ".exe"
	}
	if err := os.WriteFile(filepath.Join(wrongBin, wrongGH), []byte("invalid executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(realGit)+string(os.PathListSeparator)+wrongBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := VerifyPublication(context.Background(), workDir, publication, selection.ExpectedLogin); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil || !strings.Contains(string(data), `"kind":"helper"`) {
		t.Fatal("real Git did not execute the quoted, frozen gh helper")
	}
}

func TestVerifyPublicationRefusesLocalGitURLRewrite(t *testing.T) {
	selection, _ := publicationFixture(t)
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	localRepository := t.TempDir()
	for _, target := range []string{filepath.ToSlash(localRepository) + "/", "file:///" + strings.TrimPrefix(filepath.ToSlash(localRepository), "/") + "/"} {
		t.Run(target, func(t *testing.T) {
			globalConfig := filepath.Join(t.TempDir(), "gitconfig")
			command := exec.CommandContext(context.Background(), realGit, "config", "--file", globalConfig, "url."+target+".insteadOf", "https://github.com/")
			shellenv.ConfigureShellCommand(command)
			if err := shellenv.RunShellCommand(command); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(globalConfig)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
			publication, err := ResolvePublication(context.Background(), "https://github.com/parent/repo", "", selection)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", filepath.Dir(realGit)+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err := VerifyPublication(context.Background(), localRepository, publication, selection.ExpectedLogin); err == nil {
				t.Fatal("publication accepted a local transport rewrite")
			}
			after, err := os.ReadFile(globalConfig)
			if err != nil || string(before) != string(after) {
				t.Fatal("publication modified shared Git configuration")
			}
		})
	}
}

func TestPublicationGitUsesSelectedHelperForTheActualRepositoryPath(t *testing.T) {
	selection, _ := publicationFixture(t)
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	publication, err := ResolvePublication(t.Context(), "https://github.com/parent/repo.git", "https://github.com/fork/repo.git", selection)
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	command := exec.CommandContext(t.Context(), realGit, "init", workDir)
	shellenv.ConfigureShellCommand(command)
	if err := shellenv.RunShellCommand(command); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".git", "config"), []byte("[credential]\n helper = !exit 87\n[credential \"https://github.com/fork\"]\n helper = !exit 88\n[credential \"https://github.com/fork/repo.git\"]\n helper = !exit 89\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command = exec.CommandContext(t.Context(), realGit, "credential", "fill")
	command.Dir = workDir
	command.Env = publication.Environment.Apply(nil)
	command.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=fork/repo.git\n\n")
	shellenv.ConfigureShellCommand(command)
	output, err := shellenv.OutputShellCommand(command)
	if err != nil || !strings.Contains(string(output), os.Getenv("NM_PUBLICATION_TEST_SECRET")) {
		t.Fatal("the actual push repository path did not use the selected frozen helper")
	}
}

func publicationSettings(t *testing.T, environment map[string]string) map[string]string {
	t.Helper()
	count, err := strconv.Atoi(environment["GIT_CONFIG_COUNT"])
	if err != nil {
		t.Fatal("missing numbered Git config")
	}
	settings := make(map[string]string)
	for index := range count {
		settings[strings.ToLower(environment["GIT_CONFIG_KEY_"+strconv.Itoa(index)])] = environment["GIT_CONFIG_VALUE_"+strconv.Itoa(index)]
	}
	return settings
}

const PUBLICATION_FAKE_SOURCE = `package main
import ("encoding/json"; "fmt"; "io"; "os"; "path/filepath"; "strings")
func main() {
 args := os.Args[1:]
 secret := os.Getenv("NM_PUBLICATION_TEST_SECRET")
 for _, arg := range args { if secret != "" && strings.Contains(arg, secret) { os.Exit(90) } }
 kind := ""
 isGit := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == "git"
 if isGit {
  switch strings.Join(args, " ") {
  case "config --null --list": kind = "config"
  case "credential fill": kind = "credential-fill"
  default: os.Exit(91)
  }
 } else if len(args) == 3 && args[0] == "auth" && args[1] == "git-credential" && args[2] == "get" {
  kind = "helper"
 } else if len(args) == 6 && args[0] == "api" && args[1] == "--hostname" && args[2] == "github.com" && args[3] == "user" && args[4] == "--jq" && args[5] == ".login" {
  kind = "api-config"
  if os.Getenv("GH_TOKEN") == secret { kind = "api-git" }
 } else { os.Exit(92) }
 for _, key := range []string{"GITHUB_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_DEBUG", "GIT_TRACE", "GIT_TRACE_CURL"} { if os.Getenv(key) != "" { os.Exit(93) } }
 if os.Getenv("GH_CONFIG_DIR") == "" { os.Exit(94) }
 if f, err := os.OpenFile(os.Getenv("NM_PUBLICATION_TEST_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
  json.NewEncoder(f).Encode(struct { Kind string ` + "`json:\"kind\"`" + `; Args []string ` + "`json:\"args\"`" + ` }{kind, args}); f.Close()
 }
 switch kind {
 case "config":
  switch os.Getenv("NM_PUBLICATION_TEST_RESPONSE") {
  case "config-failure": fmt.Fprintln(os.Stderr, secret); os.Exit(97)
  case "header-bypass": fmt.Printf("http.https://github.com/.extraheader\n%s\x00", secret)
  case "rewrite-bypass": fmt.Println("url.ssh://other/.insteadof\nhttps://github.com/\x00")
  case "network-file-bypass": fmt.Println("url.file:////server/share/.insteadof\nhttps://github.com/\x00")
  }
 case "api-config":
  if os.Getenv("NM_PUBLICATION_TEST_RESPONSE") == "api-failure" { fmt.Fprintln(os.Stderr, secret); os.Exit(98) }
  fmt.Println(os.Getenv("NM_PUBLICATION_TEST_API_LOGIN"))
 case "api-git": fmt.Println(os.Getenv("NM_PUBLICATION_TEST_GIT_LOGIN"))
 case "credential-fill", "helper":
  input, _ := io.ReadAll(os.Stdin)
  if !strings.Contains(string(input), "protocol=https\n") || !strings.Contains(string(input), "host=github.com\n") { os.Exit(95) }
  mode := os.Getenv("NM_PUBLICATION_TEST_RESPONSE")
  if mode == "failure" { fmt.Fprintln(os.Stderr, secret); os.Exit(96) }
  host := "github.com"; if mode == "wrong-host" { host = "elsewhere.test" }
  fmt.Printf("protocol=https\nhost=%s\nusername=publisher\n", host)
  if mode == "split-response" { fmt.Println() }
  if mode != "missing" { fmt.Printf("password=%s\n", secret) }
  if mode == "duplicate" { fmt.Printf("password=%s\n", secret) }
  if mode == "unknown-field" { fmt.Println("unexpected=field") }
  if mode == "malformed" { fmt.Println(secret) }
  fmt.Println()
 }
}`
