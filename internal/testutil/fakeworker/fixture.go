package fakeworker

// UnstableFields are the session event payload fields whose values change
// from run to run (or are bulky metrics) in the Web UI's recording of the
// fake worker (web/src/lib/session-events/fake-worker.fixture.json). The
// recording keeps them as type markers (MaskUnstable), and the e2e tests
// mask the real worker's events the same way before comparing them with it.
var UnstableFields = map[string]bool{
	"timestamp": true, "duration_ms": true, "duration_api_ms": true, "total_cost_usd": true,
	"usage": true, "modelUsage": true, "diagnostics": true, "context_management": true, "container": true,
}

// MaskUnstable replaces, in place and at any depth, the values of
// UnstableFields by type markers: a string becomes "<string>", a number 0
// and a boolean false; objects keep their keys and arrays their elements,
// masked the same way; null stays null. The key and its JSON type remain.
func MaskUnstable(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			if UnstableFields[k] {
				v[k] = typeMarker(child)
			} else {
				v[k] = MaskUnstable(child)
			}
		}
	case []any:
		for i, child := range v {
			v[i] = MaskUnstable(child)
		}
	}
	return v
}

func typeMarker(v any) any {
	switch v := v.(type) {
	case string:
		return "<string>"
	case float64:
		return float64(0)
	case bool:
		return false
	case map[string]any:
		for k, child := range v {
			v[k] = typeMarker(child)
		}
	case []any:
		for i, child := range v {
			v[i] = typeMarker(child)
		}
	}
	return v
}
