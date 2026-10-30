package httpapi

import (
	"fmt"
	"net/http"

	"ly-route/backend/internal/thermal"
)

const thermalBlockSuggestion = "stop heavy configuration and wait for the CPU to cool; check the fan, heatsink and airflow before retrying. Setting LY_ROUTE_THERMAL_BLOCK_C=0 disables blocking on hardware whose cooling cannot recover otherwise."

// thermalSnapshot returns the current observation. A server built before the
// guard was wired still publishes an explicit unknown/ok shape instead of
// omitting the health field.
func (server *Server) thermalSnapshot() thermal.Snapshot {
	if server == nil || server.thermal == nil {
		return thermal.UnknownSnapshot()
	}
	return server.thermal.Snapshot()
}

// rejectDestructiveThermal refuses an apply or firmware install before it
// touches the apply gate, a transaction, a stage directory or any service.
// Read-only, health, log and stop operations deliberately do not call this.
func (server *Server) rejectDestructiveThermal(w http.ResponseWriter, r *http.Request, session Session, resource, action string) bool {
	if server == nil || server.thermal == nil {
		return false
	}
	snapshot, blocked := server.thermal.Blocked()
	if !blocked {
		return false
	}
	temperature := 0.0
	if snapshot.TemperatureC != nil {
		temperature = *snapshot.TemperatureC
	}
	message := fmt.Sprintf("CPU temperature %.1fC reached the %.0fC thermal block threshold; %s is refused until the device cools", temperature, snapshot.BlockC, action)
	server.recordAudit(session.Username, session.Role, resource, action, "denied", message, r)
	writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: ErrorBody{
		Code:         "thermal_throttle",
		Message:      message,
		RequestID:    requestID(r),
		TemperatureC: snapshot.TemperatureC,
		WarnC:        &snapshot.WarnC,
		BlockC:       &snapshot.BlockC,
		Suggestion:   thermalBlockSuggestion,
		Thermal:      snapshot,
	}})
	return true
}
