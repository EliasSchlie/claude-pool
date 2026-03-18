package pool

import "github.com/EliasSchlie/claude-pool/internal/api"

// parentFromReq reads the parent field with backward compat fallback.
func parentFromReq(req api.Msg) string {
	if v, _ := req["parent"].(string); v != "" {
		return v
	}
	v, _ := req["parentId"].(string)
	return v
}

func verbosityFromReq(req api.Msg, fallback string) string {
	if v, _ := req["verbosity"].(string); v != "" {
		return v
	}
	return fallback
}

// parseCaptureParams extracts source/turns/detail from a request with defaults.
func parseCaptureParams(req api.Msg) (source string, turns int, detail string) {
	if format, ok := req["format"].(string); ok && format != "" {
		switch format {
		case "jsonl-short", "jsonl-last":
			return "jsonl", 1, "last"
		case "jsonl-long":
			return "jsonl", 1, "tools"
		case "jsonl-full":
			return "jsonl", 0, "raw"
		case "buffer-last":
			return "buffer", 1, "last"
		case "buffer-full":
			return "buffer", 0, "last"
		}
	}

	source, _ = req["source"].(string)
	if source == "" {
		source = "jsonl"
	}
	turns = 1
	if t, ok := req["turns"].(float64); ok {
		turns = int(t)
	}
	detail, _ = req["detail"].(string)
	if detail == "" {
		detail = "last"
	}
	return
}
