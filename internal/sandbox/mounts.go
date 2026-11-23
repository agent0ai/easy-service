package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/example/easy-service/internal/config"
	"github.com/example/easy-service/internal/state"
)

// The data group is outside the workload UID/GID allocation range.
const dataGroup = 19000

type launchMount struct {
	config.Mount
	Device, Inode uint64
}

func ValidateMounts(cfg config.Config) error {
	if len(cfg.SandboxMounts) == 0 {
		return nil
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	protected := []string{cfg.DataDir, cfg.ConfigDir, cfg.LogDir, binary}
	if filepath.IsAbs(cfg.GitURL) {
		protected = append(protected, cfg.GitURL)
	}
	for i, path := range protected {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			protected[i] = resolved
		}
	}
	for _, mount := range cfg.SandboxMounts {
		for _, path := range protected {
			if mount.Source == path || state.Within(path, mount.Source) || state.Within(mount.Source, path) {
				return fmt.Errorf("SANDBOX_MOUNTS source %q overlaps a protected path", mount.Source)
			}
		}
		if _, err := mountSource(mount.Source); err != nil {
			return err
		}
	}
	return nil
}

func mountSource(path string) (*syscall.Stat_t, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("SANDBOX_MOUNTS source %q: %w", path, err)
	}
	if resolved != path {
		return nil, fmt.Errorf("SANDBOX_MOUNTS source %q must not traverse symlinks", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("SANDBOX_MOUNTS source %q: %w", path, err)
	}
	stat := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || stat.Gid != dataGroup || st.Mode()&(os.ModeSetgid|0070) != os.ModeSetgid|0070 {
		return nil, fmt.Errorf("SANDBOX_MOUNTS source %q must be a setgid directory with rwx for group %d", path, dataGroup)
	}
	return stat, nil
}

func launchMounts(rootfs string, mounts []config.Mount) ([]launchMount, error) {
	var result []launchMount
	for _, mount := range mounts {
		for path := mount.Destination; path != "/"; path = filepath.Dir(path) {
			info, err := os.Lstat(filepath.Join(rootfs, path))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil || !info.IsDir() {
				return nil, fmt.Errorf("SANDBOX_MOUNTS destination %q needs directories without symlinks", mount.Destination)
			}
		}
		stat, err := mountSource(mount.Source)
		if err != nil {
			return nil, err
		}
		result = append(result, launchMount{Mount: mount, Device: stat.Dev, Inode: stat.Ino})
	}
	return result, nil
}
