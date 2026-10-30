package httpapi

import (
	"context"
	"log"
	"time"

	serviceRuntime "ly-route/backend/internal/runtime/service"
)

// captureRunningServices records the daemons that were healthy before a
// dataplane transition can bounce them. Only services that were already
// running are candidates for recovery: an apply must never invent a service
// the operator had deliberately stopped.
func (server *Server) captureRunningServices(ctx context.Context) map[serviceRuntime.ServiceName]struct{} {
	baseline := map[serviceRuntime.ServiceName]struct{}{}
	if !server.serviceRuntimeConfigured() {
		return baseline
	}
	server.services.InvalidateHealth()
	health, err := server.services.HealthCheck(ctx, serviceRuntime.SmartDNS, serviceRuntime.Dnsmasq, serviceRuntime.Xray, serviceRuntime.PPPoE)
	if err != nil {
		return baseline
	}
	for _, item := range health {
		if item.Available {
			baseline[item.Service] = struct{}{}
		}
	}
	return baseline
}

func (server *Server) restoreRunningServices(ctx context.Context, baseline map[serviceRuntime.ServiceName]struct{}) {
	if len(baseline) == 0 || !server.serviceRuntimeConfigured() {
		return
	}
	restarter, ok := server.services.Controller.(serviceRuntime.ServiceRestarter)
	if !ok {
		return
	}
	services := make([]serviceRuntime.ServiceName, 0, len(baseline))
	for _, service := range []serviceRuntime.ServiceName{serviceRuntime.SmartDNS, serviceRuntime.Dnsmasq, serviceRuntime.Xray, serviceRuntime.PPPoE} {
		if _, wanted := baseline[service]; wanted {
			services = append(services, service)
		}
	}
	// A VPP restart propagates to PartOf= daemons. Give systemd a short window
	// to finish that transition before issuing an explicit start, then recover
	// whichever service is still down.
	//
	// Recovery must not inherit the apply's deadline: a slow dataplane
	// bootstrap can exhaust the apply budget right before this point, and a
	// cancelled context would make every systemctl call fail immediately and
	// leave the daemon down. The restore therefore gets its own bounded
	// budget, independent of whether the apply context is still alive.
	recoveryCtx, cancelRecovery := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancelRecovery()
	deadline := time.Now().Add(10 * time.Second)
	for _, service := range services {
		server.services.InvalidateHealth()
		health, err := server.services.HealthCheck(recoveryCtx, service)
		if err == nil && len(health) > 0 && health[0].Available {
			continue
		}
		for {
			restartErr := restarter.Restart(recoveryCtx, service)
			server.services.InvalidateHealth()
			health, healthErr := server.services.HealthCheck(recoveryCtx, service)
			if restartErr == nil && healthErr == nil && len(health) > 0 && health[0].Available {
				log.Printf("runtime apply recovery: restarted %s after rejected dataplane transition", service)
				break
			}
			if time.Now().After(deadline) {
				log.Printf("runtime apply recovery: %s is still not active: restart=%v health=%v", service, restartErr, healthErr)
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}
