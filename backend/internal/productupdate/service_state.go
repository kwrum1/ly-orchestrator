package productupdate

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ServiceStateController preserves the enable/disable state of the systemd
// units shared by both products.  Unit active/running state is intentionally
// not "restored" as enabled: a conversion must not silently start a service
// that the operator had stopped.
type ServiceStateController interface {
	Capture(context.Context, []string) (map[string]string, error)
	Restore(context.Context, map[string]string) error
}

type SystemctlController struct {
	Run     func(context.Context, string, ...string) ([]byte, error)
	Timeout time.Duration
}

func (controller SystemctlController) commandContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := controller.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

func (controller SystemctlController) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if controller.Run != nil {
		return controller.Run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (controller SystemctlController) Capture(ctx context.Context, units []string) (map[string]string, error) {
	state := make(map[string]string, len(units))
	for _, unit := range units {
		unit = strings.TrimSpace(unit)
		if unit == "" {
			continue
		}
		state[unit] = controller.captureUnit(ctx, unit)
	}
	return state, nil
}

func (controller SystemctlController) captureUnit(ctx context.Context, unit string) string {
	runCtx, cancel := controller.commandContext(ctx)
	output, err := controller.run(runCtx, "systemctl", "is-enabled", unit)
	cancel()
	value := strings.TrimSpace(string(output))
	switch {
	case strings.Contains(value, "enabled"):
		return "enabled"
	case strings.Contains(value, "masked"):
		return "masked"
	case strings.Contains(value, "disabled"):
		return "disabled"
	case strings.Contains(value, "static"), strings.Contains(value, "indirect"), strings.Contains(value, "alias"), strings.Contains(value, "generated"):
		return "static"
	case err != nil:
		// Missing units are normal during conversion across product profiles.
		return "missing"
	default:
		return "unknown"
	}
}

func (controller SystemctlController) Restore(ctx context.Context, state map[string]string) error {
	for unit, value := range state {
		unit = strings.TrimSpace(unit)
		if unit == "" {
			continue
		}
		desired := strings.ToLower(strings.TrimSpace(value))
		var args []string
		switch desired {
		case "enabled":
			args = []string{"enable", unit}
		case "disabled":
			args = []string{"disable", unit}
		case "masked":
			args = []string{"mask", unit}
		default:
			// static, indirect, missing, unknown: enabling is undefined and
			// disabling could change semantics; leave it untouched.
			continue
		}
		// A conversion usually changes only one or two units.  Avoid needless
		// systemctl mutations (and their latency under TCG) for units that are
		// already in the desired state.
		if current := controller.captureUnit(ctx, unit); current == desired {
			continue
		}
		runCtx, cancel := controller.commandContext(ctx)
		output, err := controller.run(runCtx, "systemctl", args...)
		cancel()
		if err != nil {
			return fmt.Errorf("restore %s state %s: %w: %s", unit, value, err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}
