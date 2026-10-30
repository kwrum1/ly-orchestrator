package httpapi

import "testing"

func TestWANLinkAppliedAfterCommit(t *testing.T) {
	tests := []struct {
		name      string
		committed bool
		item      map[string]any
		want      bool
	}{
		{
			name:      "static WAN pending after commit becomes applied",
			committed: true,
			item:      map[string]any{"id": "wan", "type": "static", "enabled": true, "runtime_state": "desired_not_applied"},
			want:      true,
		},
		{
			name:      "dhcp WAN pending after commit becomes applied",
			committed: true,
			item:      map[string]any{"id": "wan", "wan_type": "dhcp", "enabled": true, "runtime_state": "desired_not_applied"},
			want:      true,
		},
		{
			name:      "pppoe keeps live session state",
			committed: true,
			item:      map[string]any{"id": "wan", "type": "pppoe", "enabled": true, "runtime_state": "desired_not_applied"},
			want:      false,
		},
		{
			name:      "disabled WAN stays pending",
			committed: true,
			item:      map[string]any{"id": "wan", "type": "static", "enabled": false, "runtime_state": "desired_not_applied"},
			want:      false,
		},
		{
			name:      "already applied is untouched",
			committed: true,
			item:      map[string]any{"id": "wan", "type": "static", "enabled": true, "runtime_state": "applied"},
			want:      false,
		},
		{
			name:      "uncommitted transaction does not promote",
			committed: false,
			item:      map[string]any{"id": "wan", "type": "static", "enabled": true, "runtime_state": "desired_not_applied"},
			want:      false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := wanLinkAppliedAfterCommit(test.committed, test.item); got != test.want {
				t.Fatalf("wanLinkAppliedAfterCommit() = %t, want %t", got, test.want)
			}
		})
	}
}
