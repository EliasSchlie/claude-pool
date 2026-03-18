package pty

// SanitizeReplay prepares ring buffer contents for replay to a newly attached
// client. Prepends an SGR reset to clear inherited text attributes (bold,
// color, etc.) from escape sequences that completed before the ring buffer's
// wrap point.
//
// Note: orphaned CSI interior bytes (e.g., "31m" from a truncated \x1b[31m)
// are harmless — terminals require the ESC introducer to enter escape parsing,
// so bare "31m" is printed as literal text.
func SanitizeReplay(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	reset := []byte("\x1b[0m")
	result := make([]byte, len(reset)+len(data))
	copy(result, reset)
	copy(result[len(reset):], data)
	return result
}
