package api

// Msg is a generic JSON message (same type as the protocol wire format).
type Msg = map[string]any

// Response helpers.

func OkResponse(id any) Msg {
	m := Msg{"type": "ok"}
	if id != nil {
		m["id"] = id
	}
	return m
}

func ErrorResponse(id any, message string) Msg {
	m := Msg{"type": "error", "error": message}
	if id != nil {
		m["id"] = id
	}
	return m
}

func ConfigResponse(id any, cfg Msg) Msg {
	m := Msg{"type": "config", "config": cfg}
	if id != nil {
		m["id"] = id
	}
	return m
}
