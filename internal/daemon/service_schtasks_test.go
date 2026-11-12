package daemon

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestStartInstallsWindowsTaskAndStartsManagedDaemon(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	exe := `C:\Program Files\no-mistakes\no-mistakes.exe`
	serviceExecutablePath = func() (string, error) { return exe, nil }
	serviceCurrentUser = stubWindowsTaskUser

	var commands []string
	var definition string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		if name == "schtasks" && len(args) > 0 && args[0] == "/Create" {
			definition = readWindowsTaskDefinition(t, args)
		}
		commands = append(commands, name+" "+strings.Join(args, " "))
		if name == "schtasks" && len(args) == 3 && args[0] == "/Query" && args[2] == windowsTaskName(p) {
			return nil, fmt.Errorf("task not found")
		}
		// Simulate fresh-install: the legacy unsuffixed task is absent, so
		// the pre-install cleanup query fails and cleanupLegacyWindowsTask
		// returns without issuing End/Delete.
		if name == "schtasks" && len(args) >= 4 && args[0] == "/Query" && args[2] == legacyWindowsTaskName && args[3] == "/XML" {
			return nil, fmt.Errorf("task not found")
		}
		return nil, nil
	}
	checks := 0
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		checks++
		return checks >= 2, nil
	}

	if err := Start(p); err != nil {
		t.Fatal(err)
	}

	wantQueryLegacy := "schtasks /Query /TN " + legacyWindowsTaskName + " /XML"
	wantCreatePrefix := "schtasks /Create /TN " + windowsTaskName(p) + " /XML "
	wantRun := "schtasks /Run /TN " + windowsTaskName(p)
	if len(commands) != 4 {
		t.Fatalf("expected scoped query, create, legacy query, and run, got %v", commands)
	}
	if want := "schtasks /Query /TN " + windowsTaskName(p); commands[0] != want {
		t.Fatalf("scoped query command = %q, want %q", commands[0], want)
	}
	if !strings.HasPrefix(commands[1], wantCreatePrefix) || !strings.HasSuffix(commands[1], " /F") {
		t.Fatalf("create command = %q, want %q<file> /F", commands[1], wantCreatePrefix)
	}
	if want := renderWindowsTaskXML(exe, p.Root(), stubWindowsTaskAccount); definition != want {
		t.Fatalf("registered definition = %q, want %q", definition, want)
	}
	if commands[2] != wantQueryLegacy {
		t.Fatalf("legacy query command = %q, want %q", commands[2], wantQueryLegacy)
	}
	if commands[3] != wantRun {
		t.Fatalf("run command = %q, want %q", commands[3], wantRun)
	}
}

// Issue #253: a task registered before the definition named a priority runs
// at Task Scheduler's default 7. The next start of a stopped daemon must
// re-register it, so an existing install is corrected, not only a new one.
func TestStartReregistersAnExistingWindowsTaskAtNormalPriority(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	exe := `C:\Program Files\no-mistakes\no-mistakes.exe`
	serviceExecutablePath = func() (string, error) { return exe, nil }
	serviceCurrentUser = stubWindowsTaskUser

	registered := `<Task><Settings><Priority>7</Priority></Settings><Actions><Exec><Command>` + exe + `</Command><Arguments>daemon run --root "` + p.Root() + `"</Arguments></Exec></Actions></Task>`
	var definitions []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		if name == "schtasks" && len(args) > 0 && args[0] == "/Create" {
			definitions = append(definitions, readWindowsTaskDefinition(t, args))
		}
		if name == "schtasks" && len(args) >= 3 && args[0] == "/Query" && args[2] == windowsTaskName(p) {
			return []byte(registered), nil
		}
		if name == "schtasks" && len(args) >= 3 && args[0] == "/Query" && args[2] == legacyWindowsTaskName {
			return nil, fmt.Errorf("task not found")
		}
		return nil, nil
	}
	checks := 0
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		checks++
		return checks >= 2, nil
	}

	if err := Start(p); err != nil {
		t.Fatal(err)
	}

	if len(definitions) != 1 {
		t.Fatalf("start registered %d task definitions, want 1", len(definitions))
	}
	if got := parseWindowsTaskDefinition(t, definitions[0]).Priority; got != "4" {
		t.Fatalf("re-registered task priority = %q, want 4", got)
	}
}

func TestInstallWindowsTaskDoesNotRemoveLegacyTaskForDifferentRoot(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	serviceCurrentUser = stubWindowsTaskUser

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		if name == "schtasks" && len(args) >= 4 && args[0] == "/Query" && args[2] == legacyWindowsTaskName && args[3] == "/XML" {
			otherRoot := filepath.Join(t.TempDir(), "other-nm-home")
			return []byte(`<Task><Exec><Command>C:\nm.exe</Command><Arguments>daemon run --root ` + otherRoot + `</Arguments></Exec></Task>`), nil
		}
		return nil, nil
	}

	if err := installWindowsTask(p, `C:\Program Files\no-mistakes\no-mistakes.exe`); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 {
		t.Fatalf("install should not end or delete unrelated legacy task, got commands %v", commands)
	}
	if commands[1] != "schtasks /Query /TN "+legacyWindowsTaskName+" /XML" {
		t.Fatalf("legacy query command = %q", commands[1])
	}
}

func TestInstallWindowsTaskKeepsLegacyTaskOnCreateFailure(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	serviceCurrentUser = stubWindowsTaskUser

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		command := name + " " + strings.Join(args, " ")
		commands = append(commands, command)
		if name == "schtasks" && len(args) > 0 && args[0] == "/Create" {
			return nil, fmt.Errorf("create failed")
		}
		return nil, nil
	}

	err := installWindowsTask(p, `C:\Program Files\no-mistakes\no-mistakes.exe`)
	if err == nil {
		t.Fatal("installWindowsTask should fail when schtasks create fails")
	}
	for _, command := range commands {
		if strings.Contains(command, "/End /TN "+legacyWindowsTaskName) || strings.Contains(command, "/Delete /TN "+legacyWindowsTaskName+" /F") {
			t.Fatalf("legacy cleanup should not run before successful scoped install, got %q", command)
		}
	}
}

func TestWindowsManagedDaemonStateUsesRunGeneration(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"

	var command string
	output := "3|100|0\r\n"
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		command = name + " " + strings.Join(args, " ")
		return []byte(output), nil
	}

	launch, err := managedDaemonLaunch(p)
	if err != nil {
		t.Fatal(err)
	}
	if launch.windowsRunGeneration != "100" {
		t.Fatalf("launch generation = %q, want 100", launch.windowsRunGeneration)
	}
	state, err := managedDaemonServiceState(p, launch)
	if err != nil {
		t.Fatal(err)
	}
	if state != managedServiceUnknown {
		t.Fatalf("unchanged Ready generation state = %v, want unknown", state)
	}

	output = "3|101|0\r\n"
	state, err = managedDaemonServiceState(p, launch)
	if err != nil {
		t.Fatal(err)
	}
	if state != managedServiceExited {
		t.Fatalf("completed new generation state = %v, want exited", state)
	}
	if !strings.HasPrefix(command, "powershell.exe -NoLogo -NoProfile -NonInteractive -Command ") {
		t.Fatalf("unexpected task state command: %q", command)
	}
	if strings.Contains(command, "/FO LIST") {
		t.Fatalf("task state command uses localized schtasks output: %q", command)
	}
}
