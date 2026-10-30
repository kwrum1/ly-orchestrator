package httpapi

import (
	"bufio"
	"os"
	"strings"
)

// defaultRoutePresent reports whether the host has an IPv4 default route.
//
// A subscription download is performed by the control plane in the host
// namespace. Without a default route it can only time out, so refresh refuses
// before attempting the fetch and tells the operator to bring up a WAN line.
// An unreadable table (non-Linux development hosts, unit tests) is treated as
// present: the guard must never block an operation it cannot judge.
func defaultRoutePresent(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return true
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || fields[0] == "Iface" {
			continue
		}
		if fields[1] == "00000000" {
			return true
		}
	}
	return false
}
