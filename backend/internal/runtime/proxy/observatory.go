package proxy

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// ProxyProbeURLEnv overrides the observatory probe target without rebuilding
// the product. The value must be an http(s) URL; invalid values are ignored so
// a typo cannot break the data path.
const ProxyProbeURLEnv = "LY_ROUTE_PROXY_PROBE_URL"

// ValidateObservatoryProbeURL rejects probe targets the control plane cannot
// hand to Xray: a non-http scheme, an empty host, or embedded credentials.
func ValidateObservatoryProbeURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("probe URL is empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("probe URL is invalid: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("probe URL must use http or https")
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return fmt.Errorf("probe URL requires a host")
	}
	if parsed.User != nil {
		return fmt.Errorf("probe URL must not embed credentials")
	}
	return nil
}

// ObservatoryProbeURL resolves the probe target in product priority order:
// the environment override, then the compiled default. Xray probes this URL
// through every outbound, so it must be reachable from the proxy nodes, not
// just from the gateway.
func ObservatoryProbeURL() string {
	if raw := strings.TrimSpace(os.Getenv(ProxyProbeURLEnv)); raw != "" {
		if ValidateObservatoryProbeURL(raw) == nil {
			return raw
		}
	}
	return ProxyProbeURL
}

// ObservatoryProbeURLForGroup applies a group-level override before the
// product-wide default. An invalid stored value falls back instead of
// generating an unusable Xray config.
func ObservatoryProbeURLForGroup(group NodeGroup) string {
	if raw := strings.TrimSpace(group.ProbeURL); raw != "" {
		if ValidateObservatoryProbeURL(raw) == nil {
			return raw
		}
	}
	return ObservatoryProbeURL()
}
