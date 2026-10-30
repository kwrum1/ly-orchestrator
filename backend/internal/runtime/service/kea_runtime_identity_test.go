package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDnsmasqRuntimeIdentityTracksInterfaceRecreation(t *testing.T) {
	root := t.TempDir()
	controller := FilesystemController{RootDir: root}
	interfacePath := filepath.Join(root, "sys", "class", "net", "lylan-ens34")
	if err := os.MkdirAll(interfacePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(interfacePath, "ifindex"), []byte("17\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifacts := []RenderedArtifact{NewArtifact(Dnsmasq, "/etc/dnsmasq.d/ly-route-dhcp.conf", "port=0\ninterface=lylan-ens34\n", "restart")}
	if err := controller.saveApplyRecord(Dnsmasq, artifacts, "runtime-a"); err != nil {
		t.Fatal(err)
	}
	if !controller.serviceRuntimeIdentityMatches(Dnsmasq, artifacts) {
		t.Fatal("unchanged Dnsmasq interface identity did not match")
	}
	if err := os.WriteFile(filepath.Join(interfacePath, "ifindex"), []byte("46\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if controller.serviceRuntimeIdentityMatches(Dnsmasq, artifacts) {
		t.Fatal("recreated Dnsmasq interface retained a stale runtime identity")
	}
}
