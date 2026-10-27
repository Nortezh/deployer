package k8s

// podLabels returns base merged with extra in a new map. base wins on conflict, so extra labels
// can never change the labels a selector matches on.
func podLabels(base, extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range extra {
		out[k] = v
	}
	for k, v := range base {
		out[k] = v
	}
	return out
}
