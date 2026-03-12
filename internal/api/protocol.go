package api

// Msg is a generic JSON message (same type as the protocol wire format).
type Msg = map[string]any

// Response builds a Msg with the given type and optional id.
func Response(id any, msgType string, fields ...Msg) Msg {
	m := Msg{"type": msgType}
	if id != nil {
		m["id"] = id
	}
	for _, f := range fields {
		for k, v := range f {
			m[k] = v
		}
	}
	return m
}

func OkResponse(id any) Msg {
	return Response(id, "ok")
}

func ErrorResponse(id any, message string) Msg {
	return Response(id, "error", Msg{"error": message})
}

func ConfigResponse(id any, cfg Msg) Msg {
	return Response(id, "config", Msg{"config": cfg})
}
