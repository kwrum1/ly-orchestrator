package httpapi

import (
	"context"
	"testing"
)

func TestInterfaceMirrorSourcesOfParsesObjectForm(t *testing.T) {
	item := map[string]any{
		"mirror": true,
		"mirror_sources": []any{
			map[string]any{"interface": "eth1", "direction": "rx"},
			map[string]any{"interface": "eth2", "direction": "both", "l2": false},
			"eth3",
		},
	}
	sources := interfaceMirrorSourcesOf(item)
	if len(sources) != 3 {
		t.Fatalf("sources = %#v", sources)
	}
	if sources[0].Interface != "eth1" || sources[0].Direction != "rx" || sources[0].L2 {
		t.Fatalf("first source = %#v", sources[0])
	}
	if sources[1].Interface != "eth2" || sources[1].Direction != "both" || sources[1].L2 {
		t.Fatalf("second source = %#v", sources[1])
	}
	if sources[2].Interface != "eth3" || sources[2].Direction != "both" || sources[2].L2 {
		t.Fatalf("plain string source must default to both/l3: %#v", sources[2])
	}
}

func TestInterfaceMirrorSourcesOfDefaultsEmptyDirectionToBoth(t *testing.T) {
	sources := interfaceMirrorSourcesOf(map[string]any{
		"mirror_sources": []any{map[string]any{"interface": "eth1"}},
	})
	if len(sources) != 1 || sources[0].Direction != "both" {
		t.Fatalf("sources = %#v", sources)
	}
}

func TestValidateInterfaceMirrorClearingAlsoClearsSources(t *testing.T) {
	server := &Server{}
	payload := map[string]any{"mirror": false, "mirror_sources": []any{map[string]any{"interface": "eth1"}}}
	if err := server.validateInterfaceMirror(context.Background(), payload); err != nil {
		t.Fatalf("clear mirror: %v", err)
	}
	if payload["mirror"] != false {
		t.Fatalf("mirror flag not cleared: %#v", payload)
	}
	if sources, ok := payload["mirror_sources"].([]any); !ok || len(sources) != 0 {
		t.Fatalf("sources not cleared: %#v", payload["mirror_sources"])
	}
}

func TestValidateInterfaceMirrorIgnoresPayloadsWithoutMirror(t *testing.T) {
	server := &Server{}
	payload := map[string]any{"id": "eth1", "gateway_role": "lan", "cidr": "192.0.2.1/24"}
	if err := server.validateInterfaceMirror(context.Background(), payload); err != nil {
		t.Fatalf("plain interface patch: %v", err)
	}
	if _, present := payload["mirror"]; present {
		t.Fatalf("plain patch gained a mirror field: %#v", payload)
	}
}
