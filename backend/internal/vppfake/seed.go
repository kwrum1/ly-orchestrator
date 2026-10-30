package vppfake

// Seed replaces the state file with a fresh device that already owns the given
// interfaces. The gateway proof harness uses it to represent the prior
// committed generation before the transaction under test runs.
func Seed(statePath string, interfaces []*Interface, lanInterface, lanPrefix string) error {
	state := NewState()
	state.PreNAT.Interface = lanInterface
	state.PreNAT.LANPrefix = lanPrefix
	state.DNS.Interface = lanInterface
	for _, item := range interfaces {
		if item == nil || item.Name == "" {
			continue
		}
		copyOf := *item
		copyOf.Index = state.NextIfIndex
		state.NextIfIndex++
		copyOf.Hardware = true
		state.Interfaces = append(state.Interfaces, &copyOf)
	}
	return state.Save(statePath)
}
