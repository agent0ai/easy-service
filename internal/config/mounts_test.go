package config

import (
	"slices"
	"testing"
)

func TestSandboxMounts(t *testing.T) {
	cfg, err := Parse(map[string]string{"SANDBOX_MOUNTS": "/mounted-db:/var/lib/myapp, /srv/uploads"}, false)
	want := []Mount{{"/srv/uploads", "/srv/uploads"}, {"/mounted-db", "/var/lib/myapp"}}
	if err != nil || !slices.Equal(cfg.SandboxMounts, want) || len(cfg.AppEnv) != 0 {
		t.Fatalf("mounts=%v app env=%v err=%v", cfg.SandboxMounts, cfg.AppEnv, err)
	}
	for _, value := range []string{
		"relative", "/", "/a/../b", "/a:", "/a:/b:/c", "/a:/b!", "/a,",
		"/data/images:/b", "/run:/b", "/etc:/b", "/a:/etc", "/a:/app", "/a:/proc/x",
		"/a:/a,/b:/a/sub", "/a:/x,/b:/x",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := Parse(map[string]string{"SANDBOX_MOUNTS": value}, false); err == nil {
				t.Fatalf("accepted invalid mounts %q", value)
			}
		})
	}
}
