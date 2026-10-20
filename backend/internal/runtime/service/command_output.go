package service

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (SystemctlRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	timeout := 15 * time.Second
	if name == "systemctl" || name == "journalctl" {
		timeout = 10 * time.Second
	}
	output, err := runCommandBounded(ctx, timeout, name, args...)
	message := strings.TrimSpace(output)
	if err != nil {
		if err == ErrCommandTimeout {
			return "", fmt.Errorf("%s %s timed out after %s", name, strings.Join(args, " "), timeout)
		}
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("%s %s failed: %s", name, strings.Join(args, " "), message)
	}
	return message, nil
}
