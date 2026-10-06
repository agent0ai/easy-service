package supervisor

import (
	"context"
	"fmt"
	"log"
	"maps"
	"strings"

	"github.com/example/easy-service/internal/config"
	"github.com/example/easy-service/internal/health"
	"github.com/example/easy-service/internal/logs"
	"github.com/example/easy-service/internal/revision"
	"github.com/example/easy-service/internal/state"
)

func (c *Controller) deploySelection(ctx, watchCtx context.Context, sel revision.Selection) error {
	plan := c.Engine.snapshot()
	if plan.Rootfs == "" && c.Configure != nil {
		var err error
		plan, _, err = c.Configure(ctx, c.Cfg)
		if err != nil {
			return err
		}
		plan.Overrides = c.Engine.Overrides
	}
	return c.Engine.deployWith(ctx, watchCtx, sel, nil, plan)
}

func (c *Controller) apply(ctx, watchCtx context.Context) (string, error) {
	if c.Settings == nil {
		return "", fmt.Errorf("runtime configuration is unavailable")
	}
	oldValues, values := c.Settings.Values(false), c.Settings.Values(true)
	names := config.Changed(oldValues, values)
	if len(names) == 0 && maps.Equal(c.Settings.Pending(), c.Settings.Applied()) {
		log.Printf("configuration apply: no changes")
		if c.Engine.Logs != nil {
			if err := c.Engine.Logs.Trim(); err != nil {
				log.Printf("log collection failed: %v", err)
			}
		}
		return "", nil
	}
	log.Printf("configuration apply requested variables=%s", strings.Join(names, ","))
	cfg, err := config.Parse(values, true)
	if err != nil {
		return "", err
	}
	plan := c.Engine.snapshot()
	accepted, enteredDeploy := false, false
	defer func() {
		if !accepted && !enteredDeploy {
			c.Engine.collectFailed(plan, "")
		}
	}()
	plan.Cfg, plan.Overrides = cfg, c.Settings.Pending()
	plan.Health = health.New(cfg.HealthInterval, cfg.HealthPath)
	selector := c.Selector
	if c.Configure != nil {
		plan, selector, err = c.Configure(ctx, cfg)
		if err != nil {
			return "", err
		}
		plan.Overrides = c.Settings.Pending()
	}
	sha := c.Engine.Selected()
	replace, selectSource := sha == "", sha == ""
	for _, name := range names {
		switch name {
		case "GIT_URL", "GIT_BRANCH", "UPDATE_METHOD", "UPDATE_PATTERN":
			selectSource = true
			replace = true
		case "GIT_TOKEN":
			selectSource = true
		case "RUNTIME_IMAGE", "SETUP_COMMAND", "RUN_COMMAND", "HEALTH_PATH":
			replace = true
		default:
			if strings.HasPrefix(name, "APP_") {
				replace = true
			}
		}
	}
	if selectSource {
		if selector == nil {
			return "", fmt.Errorf("revision selector is unavailable")
		}
		selected, err := selector.Select(ctx, cfg.UpdateMethod, cfg.GitBranch, cfg.UpdatePattern)
		if err != nil {
			return "", err
		}
		if selected.SHA != sha {
			replace = true
		}
		sha = selected.SHA
	}
	if replace {
		log.Printf("configuration apply: replacing application revision=%s", sha)
		enteredDeploy = true
		err = c.Engine.deployWith(ctx, watchCtx, revision.Selection{SHA: sha}, nil, plan)
	} else {
		log.Printf("configuration apply: updating supervisor policy without application replacement")
		err = c.Engine.commitPolicy(ctx, watchCtx, plan)
	}
	if err != nil {
		return "", err
	}
	c.Cfg, c.Selector = cfg, selector
	accepted = true
	c.Settings.Accept(plan.Overrides)
	if c.Engine.Logs != nil {
		if err := c.Engine.Logs.Configure(logs.Policy{Days: cfg.LogRetentionDays, FileSize: cfg.LogMaxFileSize, TotalSize: cfg.LogMaxTotalSize}); err != nil {
			log.Printf("configuration applied; log collection failed: %v", err)
		}
	}
	log.Printf("configuration applied variables=%s", strings.Join(names, ","))
	return sha, nil
}

func (e *Engine) commitPolicy(ctx, watchCtx context.Context, plan Deployment) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	a := e.active
	v := state.State{GitURL: plan.Cfg.GitURL, ImageRef: plan.Cfg.RuntimeImage, Overrides: plan.Overrides}
	if a != nil {
		v.Revision, v.Digest = a.Revision, a.Digest
	}
	if err := e.Store.Save(v); err != nil {
		return err
	}
	e.Cfg, e.Overrides, e.Runtime, e.Checkout, e.Health, e.Rootfs, e.Digest = plan.Cfg, plan.Overrides, plan.Runtime, plan.Checkout, plan.Health, plan.Rootfs, plan.Digest
	if a != nil {
		if a.cancelWatch != nil {
			a.cancelWatch()
		}
		e.seq++
		next := *a
		next.generation = e.seq
		e.active = &next
		e.watch(watchCtx, &next)
	}
	return nil
}
