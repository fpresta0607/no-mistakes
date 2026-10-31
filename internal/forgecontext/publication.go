package forgecontext

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// PublicationSelection selects an existing account for one publication attempt.
type PublicationSelection struct {
	ConfigDir     string
	ExpectedLogin string
}

// ResolvePublication builds an attempt-local context without changing ordinary
// run routing, account configuration, or the process environment.
func ResolvePublication(ctx context.Context, upstreamURL, forkURL string, selection PublicationSelection) (*Context, error) {
	if !filepath.IsAbs(selection.ConfigDir) || strings.TrimSpace(selection.ExpectedLogin) == "" {
		return nil, errors.New("publication requires an absolute GitHub config directory and expected login")
	}
	host, err := publicationHost(upstreamURL)
	if err != nil {
		return nil, err
	}
	if forkURL != "" {
		forkHost, err := publicationHost(forkURL)
		if err != nil || forkHost != host {
			return nil, errors.New("publication requires HTTPS GitHub remotes on the same host")
		}
	}
	publication, err := Resolve(ctx, config.ForgeProfiles{
		host: {GHConfigDir: selection.ConfigDir, ExpectedLogin: selection.ExpectedLogin},
	}, upstreamURL, forkURL)
	if err != nil || publication == nil || publication.Provider != scm.ProviderGitHub {
		// Profile parsing errors can include fragments of a credential-bearing file.
		return nil, errors.New("publication GitHub account configuration is invalid")
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		return nil, errors.New("publication GitHub executable is unavailable")
	}
	gh, err = filepath.Abs(gh)
	if err != nil {
		return nil, errors.New("publication GitHub executable is unavailable")
	}
	environment, err := publicationEnvironment(selection.ConfigDir, host, gh)
	if err != nil {
		return nil, err
	}
	publication.Environment = environment
	return publication, nil
}

func publicationHost(remote string) (string, error) {
	parsed, err := url.Parse(remote)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" || parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("publication requires credential-free HTTPS GitHub remotes")
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parsed.RawPath != "" {
		return "", errors.New("publication requires HTTPS GitHub repository URLs")
	}
	return strings.ToLower(parsed.Hostname()), nil
}

func publicationEnvironment(dir, host, gh string) (runenv.Overlay, error) {
	environment := githubEnvironment(dir)
	environment.Unset = append(environment.Unset,
		"GIT_CONFIG_PARAMETERS", "GIT_CONFIG", "SSH_ASKPASS", "GH_DEBUG", "DEBUG", "GIT_CURL_VERBOSE",
	)
	environment.Set["GIT_TERMINAL_PROMPT"] = "0"
	environment.Set["GIT_ASKPASS"] = ""
	environment.Set["GH_PROMPT_DISABLED"] = "1"
	if os.Getenv("GIT_CONFIG_PARAMETERS") != "" {
		return runenv.Overlay{}, errors.New("publication refuses ambiguous Git credential environment")
	}
	count := 0
	if raw := os.Getenv("GIT_CONFIG_COUNT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > 1024 {
			return runenv.Overlay{}, errors.New("publication refuses ambiguous Git credential environment")
		}
		count = parsed
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "GIT_TRACE") {
			environment.Unset = append(environment.Unset, key)
		}
		for _, prefix := range []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"} {
			if !strings.HasPrefix(upper, prefix) {
				continue
			}
			index, err := strconv.Atoi(strings.TrimPrefix(upper, prefix))
			if err != nil || index < 0 || index >= count || upper != prefix+strconv.Itoa(index) {
				return runenv.Overlay{}, errors.New("publication refuses ambiguous Git credential environment")
			}
			environment.Unset = append(environment.Unset, key)
		}
	}
	settings := make([][2]string, 0, count+6)
	for index := range count {
		key, hasKey := os.LookupEnv("GIT_CONFIG_KEY_" + strconv.Itoa(index))
		value, hasValue := os.LookupEnv("GIT_CONFIG_VALUE_" + strconv.Itoa(index))
		if !hasKey || !hasValue || key == "" {
			return runenv.Overlay{}, errors.New("publication refuses ambiguous Git credential environment")
		}
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "include.") || strings.HasPrefix(lower, "includeif.") || publicationUnsafeRewrite(key) {
			return runenv.Overlay{}, errors.New("publication refuses ambiguous Git credential environment")
		}
		if strings.HasPrefix(lower, "credential.") || lower == "core.askpass" || publicationHTTPAuthKey(lower) {
			continue
		}
		settings = append(settings, [2]string{key, value})
	}
	settings = append(settings,
		[2]string{"credential.helper", ""},
		[2]string{"credential.https://" + host + ".helper", ""},
		[2]string{"credential.https://" + host + ".helper", publicationHelper(gh)},
		[2]string{"credential.interactive", "false"},
		[2]string{"core.askPass", ""},
		[2]string{"http.extraHeader", ""},
	)
	for index, pair := range settings {
		environment.Set["GIT_CONFIG_KEY_"+strconv.Itoa(index)] = pair[0]
		environment.Set["GIT_CONFIG_VALUE_"+strconv.Itoa(index)] = pair[1]
	}
	environment.Set["GIT_CONFIG_COUNT"] = strconv.Itoa(len(settings))
	return environment, nil
}

func publicationHelper(gh string) string {
	return "!'" + strings.ReplaceAll(filepath.ToSlash(gh), "'", "'\\''") + "' auth git-credential"
}

func publicationHTTPAuthKey(key string) bool {
	return strings.HasPrefix(key, "http.") && (strings.HasSuffix(key, ".extraheader") || strings.HasSuffix(key, ".cookiefile") || strings.HasSuffix(key, ".savecookies") || strings.HasSuffix(key, ".proxy"))
}

func publicationUnsafeRewrite(key string) bool {
	lower := strings.ToLower(key)
	if !strings.HasPrefix(lower, "url.") {
		return false
	}
	for _, suffix := range []string{".insteadof", ".pushinsteadof"} {
		if !strings.HasSuffix(lower, suffix) {
			continue
		}
		target := key[len("url.") : len(key)-len(suffix)]
		if filepath.IsAbs(target) && !strings.HasPrefix(filepath.ToSlash(target), "//") {
			return false
		}
		parsed, err := url.Parse(target)
		return err != nil || parsed.Scheme != "file" || parsed.Host != "" || parsed.User != nil || !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(filepath.ToSlash(parsed.Path), "//") || parsed.RawQuery != "" || parsed.Fragment != ""
	}
	return true
}

// VerifyPublication proves the effective API account and independently checks
// the credential Git selects. Credential material stays in private memory
// and subprocess environments, never argv, errors or the returned context.
func VerifyPublication(ctx context.Context, workDir string, publication *Context, expectedLogin string) error {
	if publication == nil || publication.Provider != scm.ProviderGitHub || publication.Host == "" || strings.TrimSpace(expectedLogin) == "" {
		return errors.New("publication identity context is invalid")
	}
	// Recover only the frozen executable from the exact helper form we emitted.
	gh := ""
	for key, value := range publication.Environment.Set {
		if strings.HasPrefix(key, "GIT_CONFIG_KEY_") && value == "credential.https://"+publication.Host+".helper" {
			helper := publication.Environment.Set[strings.Replace(key, "_KEY_", "_VALUE_", 1)]
			if strings.HasPrefix(helper, "!'") && strings.HasSuffix(helper, "' auth git-credential") {
				gh = strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(helper, "!'"), "' auth git-credential"), "'\\''", "'")
				if !filepath.IsAbs(gh) || publicationHelper(gh) != helper {
					return errors.New("publication credential helper is invalid")
				}
			}
		}
	}
	if gh == "" {
		return errors.New("publication credential helper is invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	environment := publication.Environment.Apply(os.Environ())
	apiArgs := []string{"api", "--hostname", publication.Host, "user", "--jq", ".login"}
	login, err := publicationOutput(ctx, workDir, environment, "", gh, apiArgs...)
	if err != nil || !strings.EqualFold(strings.TrimSpace(string(login)), strings.TrimSpace(expectedLogin)) {
		return errors.New("publication API identity could not be verified")
	}
	// HTTP headers/cookies and remote URL rewrites can bypass the credential helper.
	// Local filesystem rewrites carry no remote authentication and remain usable.
	settings, err := publicationOutput(ctx, workDir, environment, "", "git", "config", "--null", "--list")
	if err != nil {
		return errors.New("publication Git credential configuration could not be verified")
	}
	for _, entry := range strings.Split(string(settings), "\x00") {
		if entry == "" {
			continue
		}
		key, value, _ := strings.Cut(entry, "\n")
		if value != "" && (publicationHTTPAuthKey(strings.ToLower(key)) || publicationUnsafeRewrite(key)) {
			return errors.New("publication Git credential configuration is ambiguous")
		}
	}
	credentials, err := publicationOutput(ctx, workDir, environment, "protocol=https\nhost="+publication.Host+"\n\n", "git", "credential", "fill")
	if err != nil {
		return errors.New("publication Git credential identity could not be verified")
	}
	fields := make(map[string]string, 4)
	for _, line := range strings.Split(strings.TrimRight(string(credentials), "\n"), "\n") {
		key, value, hasSeparator := strings.Cut(line, "=")
		if !hasSeparator || value == "" || strings.ContainsAny(value, "\r\x00") {
			return errors.New("publication Git credential response is invalid")
		}
		if _, hasField := fields[key]; hasField {
			return errors.New("publication Git credential response is invalid")
		}
		fields[key] = value
	}
	if len(fields) != 4 || fields["protocol"] != "https" || fields["host"] != publication.Host || fields["username"] == "" || fields["password"] == "" {
		return errors.New("publication Git credential response is invalid")
	}
	// Never put this token in argv, the returned context, or an error. Setting both
	// gh token classes also supports enterprise hosts without an ambient fallback.
	credentialEnvironment := runenv.Overlay{Set: map[string]string{
		"GH_TOKEN": fields["password"], "GH_ENTERPRISE_TOKEN": fields["password"],
	}}.Apply(environment)
	login, err = publicationOutput(ctx, workDir, credentialEnvironment, "", gh, apiArgs...)
	if err != nil || !strings.EqualFold(strings.TrimSpace(string(login)), strings.TrimSpace(expectedLogin)) {
		return errors.New("publication Git credential identity could not be verified")
	}
	return nil
}

func publicationOutput(ctx context.Context, dir string, environment []string, input, executable string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, executable, args...)
	command.Dir = dir
	command.Env = environment
	command.Stdin = strings.NewReader(input)
	shellenv.ConfigureShellCommand(command)
	return shellenv.OutputShellCommand(command)
}
