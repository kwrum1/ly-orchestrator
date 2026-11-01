package httpapi

import (
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// recoveryTriggerInterval bounds how often a failed apply may ask the recovery
// unit to reconcile the device. Recovery replays the last committed
// generation, so a burst of failures would only thrash the dataplane.
const recoveryTriggerInterval = 30 * time.Second

var (
	recoveryTriggerMu   sync.Mutex
	recoveryTriggerLast time.Time
)

// recoveryTriggered reports whether the environment allows spawning the
// recovery unit. Tests and non-systemd hosts must not fork systemctl.
func recoveryTriggerEnabled() bool {
	if os.Getenv("LY_ROUTE_RECOVERY_ON_FAILURE") == "disabled" {
		return false
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	_, err := exec.LookPath("systemctl")
	return err == nil
}

// requestRecoveryAfterFailure asks ly-route-recovery.service to replay the
// last committed generation after a failed apply. It is asynchronous, fired at
// most once per recoveryTriggerInterval, and never blocks the HTTP response.
func (server *Server) requestRecoveryAfterFailure(result RuntimeApplyResult) {
	if !recoveryTriggerEnabled() {
		return
	}
	now := time.Now()
	recoveryTriggerMu.Lock()
	if !recoveryTriggerLast.IsZero() && now.Sub(recoveryTriggerLast) < recoveryTriggerInterval {
		recoveryTriggerMu.Unlock()
		return
	}
	recoveryTriggerLast = now
	recoveryTriggerMu.Unlock()

	failureReason := strings.TrimSpace(result.Reason)
	if failureReason == "" {
		failureReason = "no reason reported"
	}
	log.Printf("runtime apply: %s failed (%s): %s; asking ly-route-recovery.service to replay the last committed generation",
		result.Status, result.TransactionID, failureReason)
	cmd := exec.Command("systemctl", "start", "--no-block", "ly-route-recovery.service")
	if err := cmd.Start(); err != nil {
		log.Printf("runtime apply: could not start ly-route-recovery.service: %v", err)
		return
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("runtime apply: ly-route-recovery.service start failed: %v", err)
		}
	}()
}
