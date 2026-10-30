package service

import (
	"context"
	"strings"
	"testing"
)

func TestFilesystemControllerStopsXrayWithoutDisabling(t *testing.T) {
	runner := &fakeRunner{}
	controller := FilesystemController{Runner: runner}
	if err := controller.Stop(context.Background(), Xray, nil); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.commands, "\n")
	if !strings.Contains(got, "systemctl stop xray.service") {
		t.Fatalf("xray stop commands = %q", got)
	}
	if strings.Contains(got, "disable") {
		t.Fatalf("xray stop must not disable the unit: %q", got)
	}
}
