package httpapi

import (
	"context"
	"testing"

	serviceRuntime "ly-route/backend/internal/runtime/service"
)

type restartingServiceController struct {
	*httpServiceController
	restarted []serviceRuntime.ServiceName
}

func (controller *restartingServiceController) Restart(_ context.Context, service serviceRuntime.ServiceName) error {
	controller.restarted = append(controller.restarted, service)
	if controller.health == nil {
		controller.health = map[serviceRuntime.ServiceName]serviceRuntime.Health{}
	}
	controller.health[service] = serviceRuntime.Health{Service: service, Available: true}
	return nil
}

func TestRestoreRunningServicesRecoversDaemonStoppedByAbortedApply(t *testing.T) {
	base := &httpServiceController{health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
		serviceRuntime.SmartDNS: {Service: serviceRuntime.SmartDNS, Available: true},
		serviceRuntime.Dnsmasq:  {Service: serviceRuntime.Dnsmasq, Available: true},
		serviceRuntime.Xray:     {Service: serviceRuntime.Xray, Available: true},
		serviceRuntime.PPPoE:    {Service: serviceRuntime.PPPoE, Available: true},
	}}
	controller := &restartingServiceController{httpServiceController: base}
	server := New(WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}), WithClock(fixedClock()))
	ctx := context.Background()

	baseline := server.captureRunningServices(ctx)
	if len(baseline) != 4 {
		t.Fatalf("baseline = %#v, want all four daemons", baseline)
	}
	// A VPP restart propagates through PartOf= and leaves SmartDNS stopped.
	base.health[serviceRuntime.SmartDNS] = serviceRuntime.Health{Service: serviceRuntime.SmartDNS, Available: false, Reason: "inactive after vpp restart"}
	server.services.InvalidateHealth()

	server.restoreRunningServices(ctx, baseline)

	if len(controller.restarted) != 1 || controller.restarted[0] != serviceRuntime.SmartDNS {
		t.Fatalf("restarted = %#v, want only smartdns", controller.restarted)
	}
	health, err := server.services.HealthCheck(ctx, serviceRuntime.SmartDNS)
	if err != nil || len(health) != 1 || !health[0].Available {
		t.Fatalf("smartdns health after recovery = %#v, %v", health, err)
	}
	// The next apply uses the normal service stage; it must now succeed without
	// a manual systemctl start.
	if err := server.services.Controller.ReloadOrRestart(ctx, serviceRuntime.SmartDNS, []serviceRuntime.RenderedArtifact{{Service: serviceRuntime.SmartDNS}}); err != nil {
		t.Fatalf("subsequent smartdns apply stage failed: %v", err)
	}
}

// TestRestoreRunningServicesSurvivesExpiredApplyContext pins the abort path
// more tightly: by the time a dataplane bootstrap hands back control the apply
// budget may already be spent, and the restore must still start the daemon.
func TestRestoreRunningServicesSurvivesExpiredApplyContext(t *testing.T) {
	base := &httpServiceController{health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
		serviceRuntime.SmartDNS: {Service: serviceRuntime.SmartDNS, Available: true},
	}}
	controller := &restartingServiceController{httpServiceController: base}
	server := New(WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}), WithClock(fixedClock()))
	ctx, cancel := context.WithCancel(context.Background())
	baseline := server.captureRunningServices(ctx)
	if len(baseline) != 1 {
		t.Fatalf("baseline = %#v, want smartdns", baseline)
	}
	base.health[serviceRuntime.SmartDNS] = serviceRuntime.Health{Service: serviceRuntime.SmartDNS, Available: false, Reason: "stopped by vpp restart"}
	server.services.InvalidateHealth()
	// The apply context is already dead when recovery runs.
	cancel()

	server.restoreRunningServices(ctx, baseline)

	if len(controller.restarted) != 1 || controller.restarted[0] != serviceRuntime.SmartDNS {
		t.Fatalf("restarted = %#v, want smartdns even with an expired apply context", controller.restarted)
	}
	if health, err := server.services.HealthCheck(context.Background(), serviceRuntime.SmartDNS); err != nil || len(health) != 1 || !health[0].Available {
		t.Fatalf("smartdns health after recovery = %#v, %v", health, err)
	}
}

func TestRestoreRunningServicesDoesNotStartPreviouslyStoppedDaemon(t *testing.T) {
	base := &httpServiceController{health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
		serviceRuntime.SmartDNS: {Service: serviceRuntime.SmartDNS, Available: true},
		serviceRuntime.Dnsmasq:  {Service: serviceRuntime.Dnsmasq, Available: false, Reason: "operator stopped"},
	}}
	controller := &restartingServiceController{httpServiceController: base}
	server := New(WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}), WithClock(fixedClock()))
	baseline := server.captureRunningServices(context.Background())
	if _, found := baseline[serviceRuntime.Dnsmasq]; found {
		t.Fatalf("inactive dnsmasq was captured in baseline: %#v", baseline)
	}
	server.restoreRunningServices(context.Background(), baseline)
	for _, service := range controller.restarted {
		if service == serviceRuntime.Dnsmasq {
			t.Fatalf("recovery started a daemon that was already stopped: %#v", controller.restarted)
		}
	}
}
