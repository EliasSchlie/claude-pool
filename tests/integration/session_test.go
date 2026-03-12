package integration

// TestSession — Core session workflow flow
//
// Pool size: 3
//
// This flow tests the fundamental session operations: starting sessions, waiting
// for results, reading output in different formats, sending followups, and raw
// input. It exercises the most common usage patterns.
//
// Flow:
//
//   1. "start returns sessionId and status"
//      Call start with prompt "respond with exactly: hello world".
//      Assert: response type is "started", sessionId is a non-empty string,
//      status is "processing" (or "queued" if slots aren't ready yet, but with
//      size 3 and only 1 session, it should get a slot immediately).
//      Save sessionId as s1.
//
//   2. "wait returns result"
//      Call wait with s1.
//      Assert: response type is "result", sessionId matches s1, content is
//      non-empty and contains "hello world".
//
//   3. "info shows session details"
//      Call info on s1.
//      Assert: sessionId matches, status is "idle", claudeUUID is non-null (discovered
//      after processing), pid is a positive integer, pinned is false, priority is 0,
//      spawnCwd is non-empty, cwd is non-empty, createdAt is a valid timestamp,
//      children is an empty array.
//
//   4. "wait on already-idle returns immediately"
//      Call wait on s1 again (it's already idle from step 2).
//      Assert: returns immediately with content (the previous response).
//
//   5. "capture while idle — jsonl-short (default)"
//      Call capture on s1 with no format (default jsonl-short).
//      Assert: content is non-empty, contains the assistant's response text.
//
//   6. "capture — jsonl-last"
//      Call capture on s1 with format "jsonl-last".
//      Assert: content is non-empty. Should be just the last assistant message.
//
//   7. "capture — jsonl-long"
//      Call capture on s1 with format "jsonl-long".
//      Assert: content is non-empty. Should include more context than jsonl-short
//      but with repetitive fields stripped.
//
//   8. "capture — jsonl-full"
//      Call capture on s1 with format "jsonl-full".
//      Assert: content is non-empty. Should be the complete unfiltered transcript.
//      Should be the longest of all JSONL formats.
//
//   9. "capture — buffer-last"
//      Call capture on s1 with format "buffer-last".
//      Assert: content is non-empty. Terminal buffer content since last user message.
//
//  10. "capture — buffer-full"
//      Call capture on s1 with format "buffer-full".
//      Assert: content is non-empty. Full terminal scrollback. Should be longer
//      than buffer-last (includes earlier output).
//
//  11. "followup to idle session"
//      Call followup on s1 with prompt "respond with exactly: goodbye".
//      Assert: response type is "started", sessionId matches s1, status field is present.
//      Call wait on s1.
//      Assert: content contains "goodbye".
//
//  12. "followup on processing errors without force"
//      Start a new session s2 with a slow prompt "write a 200-word essay about trees".
//      Immediately call followup on s2 (should still be processing).
//      Assert: error response (session is processing, use force).
//
//  13. "followup with force on processing"
//      Call followup on s2 with force: true and prompt "respond with exactly: interrupted".
//      Assert: response type is "started".
//      Wait for s2 to become idle.
//      Assert: the response relates to the forced followup, not the original prompt.
//
//  14. "wait with no sessionId — returns first idle"
//      Start two sessions s3 and s4 with prompts.
//      Call wait with no sessionId.
//      Assert: returns whichever finishes first. Response includes the sessionId
//      of the completed session.
//
//  15. "wait with no sessionId — errors if none busy"
//      Ensure all sessions are idle.
//      Call wait with no sessionId.
//      Assert: error (no owned sessions are busy).
//
//  16. "wait with timeout"
//      Start a session with a slow prompt.
//      Call wait with timeout: 1 (1ms — will expire immediately).
//      Assert: error response with "timeout".
//
//  17. "input sends raw bytes"
//      Use s1 (idle). Call input with data "\x1b" (Escape — should be harmless on idle).
//      Assert: response is {type: "ok"}.
//      Call input on an offloaded session (offload s1 first, then try input).
//      Assert: error (no live terminal).
//
//  18. "session prefix resolution"
//      Get s1's full sessionId. Call info with just the first 3 characters.
//      Assert: resolves to the full session and returns its info.
//
//  19. "ambiguous prefix errors"
//      If any two sessions share a prefix, call info with that prefix.
//      Assert: error (ambiguous).
//      (If no natural collision exists, skip this sub-test.)

import "testing"

func TestSession(t *testing.T) {
	t.Skip("not yet implemented")
}
