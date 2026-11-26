package daemon

import (
	"fmt"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Uninstall stops this instance's managed daemon and removes its service
// definition. It returns the removed definition, or an empty string when no
// managed service is installed. Application data is kept.
func Uninstall(p *paths.Paths) (string, error) {
	return UninstallGuarded(p, func() (func(), error) { return PrepareShutdownIfIdle(p) })
}

// UninstallGuarded invokes beforeStop only when a managed service exists,
// before any service mutation. The caller must supply the preparation callback.
// The definition is removed only after the managed service stopped and the
// daemon exited, so a failed stop keeps it.
func UninstallGuarded(p *paths.Paths, beforeStop func() (func(), error)) (string, error) {
	definition, err := installedServiceDefinition(p)
	if err != nil || definition == "" {
		return "", err
	}
	release, err := beforeStop()
	if err != nil {
		return "", err
	}
	defer release()
	instance := captureRunningDaemon(p)
	if _, err := stopManagedService(p); err != nil {
		return "", fmt.Errorf("stop managed service: %w", err)
	}
	if alive, _ := daemonHealthCheck(p); alive {
		if err := stopDetachedDaemon(p); err != nil {
			return "", fmt.Errorf("stop daemon outside the managed service: %w", err)
		}
	}
	if err := waitForDaemonStop(p, instance); err != nil {
		return "", err
	}
	if err := removeManagedService(p); err != nil {
		return "", fmt.Errorf("remove managed service %s: %w", definition, err)
	}
	return definition, nil
}

// InstalledServiceDefinition names the managed service that starts this
// instance's daemon again at the next login, or returns an empty string when
// none is installed.
func InstalledServiceDefinition(p *paths.Paths) string {
	definition, _ := installedServiceDefinition(p)
	return definition
}

// installedServiceDefinition returns the LaunchAgent plist, the systemd user
// unit, or the scheduled task name of this instance's managed service, or an
// empty string when none is installed.
func installedServiceDefinition(p *paths.Paths) (string, error) {
	if serviceManagerBypassed() {
		return "", nil
	}
	switch runtimeGOOS {
	case "darwin":
		return installedServiceFile(p, launchAgentPath)
	case "linux":
		return installedServiceFile(p, systemdUserServicePath)
	case "windows":
		if !managedServiceInstalled(p) {
			return "", nil
		}
		return windowsTaskName(p), nil
	default:
		return "", nil
	}
}

func installedServiceFile(p *paths.Paths, servicePath func(*paths.Paths) string) (string, error) {
	if _, err := serviceUserHomeDir(); err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	definition := servicePath(p)
	if _, err := os.Stat(definition); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("inspect managed service %s: %w", definition, err)
	}
	return definition, nil
}
