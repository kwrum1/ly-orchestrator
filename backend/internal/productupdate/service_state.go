package productupdate

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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
	Run func(context.Context, string, ...string) ([]byte, error)
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
		output, err := controller.run(ctx, "systemctl", "is-enabled", unit)
		value := strings.TrimSpace(string(output))
		switch {
		case strings.Contains(value, "enabled"):
			state[unit] = "enabled"
		case strings.Contains(value, "masked"):
			state[unit] = "masked"
		case strings.Contains(value, "disabled"):
			state[unit] = "disabled"
		case strings.Contains(value, "static"), strings.Contains(value, "indirect"), strings.Contains(value, "alias"), strings.Contains(value, "generated"):
			state[unit] = "static"
		case err != nil:
			// Missing units are normal during conversion across product
			// profiles.  Record them explicitly so restore is a no-op.
			state[unit] = "missing"
		default:
			state[unit] = "unknown"
		}
	}
	return state, nil
}

func (controller SystemctlController) Restore(ctx context.Context, state map[string]string) error {
	for unit, value := range state {
		unit = strings.TrimSpace(unit)
		if unit == "" {
			continue
		}
		var args []string
		switch strings.ToLower(strings.TrimSpace(value)) {
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
		if output, err := controller.run(ctx, "systemctl", args...); err != nil {
			return fmt.Errorf("restore %s state %s: %w: %s", unit, value, err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}
