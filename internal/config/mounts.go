package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

type Mount struct{ Source, Destination string }

func parseMounts(value string, protected ...string) ([]Mount, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	protected = append(protected, "/proc", "/dev", "/sys", "/run/easy-service", "/etc", "/usr/local/bin/easy-service")
	var mounts []Mount
	for index, entry := range strings.Split(value, ",") {
		source, destination, mapped := strings.Cut(strings.TrimSpace(entry), ":")
		source, destination = strings.TrimSpace(source), strings.TrimSpace(destination)
		if !mapped {
			destination = source
		}
		for _, path := range []string{source, destination} {
			if !filepath.IsAbs(path) || path == "/" || filepath.Clean(path) != path || strings.ContainsAny(path, ":!\x00\r\n") {
				return nil, fmt.Errorf("SANDBOX_MOUNTS entry %d requires clean absolute directory paths below /", index+1)
			}
		}
		for _, path := range protected {
			if statePathsOverlap(path, source) {
				return nil, fmt.Errorf("SANDBOX_MOUNTS entry %d overlaps a protected source", index+1)
			}
		}
		for _, path := range []string{"/proc", "/dev", "/sys", "/.easy-service", "/etc/resolv.conf", "/bin/sh"} {
			if statePathsOverlap(path, destination) {
				return nil, fmt.Errorf("SANDBOX_MOUNTS entry %d overlaps a protected destination", index+1)
			}
		}
		if destination == "/app" {
			return nil, fmt.Errorf("SANDBOX_MOUNTS entry %d would hide the checkout", index+1)
		}
		for _, mount := range mounts {
			if statePathsOverlap(mount.Destination, destination) {
				return nil, fmt.Errorf("SANDBOX_MOUNTS entry %d overlaps another destination", index+1)
			}
		}
		mounts = append(mounts, Mount{Source: source, Destination: destination})
	}
	slices.SortFunc(mounts, func(a, b Mount) int { return strings.Compare(a.Destination, b.Destination) })
	return mounts, nil
}
