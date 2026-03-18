#!/bin/bash
# Writes idle signal files for session lifecycle events.
#
# Processing state transitions (idle↔processing) are handled entirely by
# the daemon's screen content monitoring — no hooks needed. This script
# only handles startup/lifecycle signals.
#
# Usage: idle-signal.sh write [session-start|session-clear]

source "$(dirname "$0")/common.sh"

mkdir -p "$SIGNAL_DIR"

claude_pid="$PPID"
signal_file="$SIGNAL_DIR/$claude_pid"

# Read hook input from stdin
read_input() {
    local line=""
    read -t 1 -r line 2>/dev/null || true
    if [ -n "$line" ]; then
        echo "$line"
        cat 2>/dev/null
    fi
}

trigger="${2:-unknown}"
input=$(read_input)
session_id=""
transcript=""
hook_cwd=""
if [ -n "$input" ]; then
    session_id=$(json_get "$input" "session_id")
    transcript=$(json_get "$input" "transcript_path")
    hook_cwd=$(json_get "$input" "cwd")
fi

json_esc() {
    printf '%s' "$1" | awk '
        BEGIN { ORS="" }
        {
            if (NR > 1) printf "\\n"
            gsub(/\\/, "\\\\")
            gsub(/"/, "\\\"")
            gsub(/\t/, "\\t")
            gsub(/\r/, "\\r")
            printf "%s", $0
        }
    '
}

effective_cwd="${hook_cwd:-$(pwd)}"
printf '{"cwd":"%s","session_id":"%s","transcript":"%s","ts":%d,"trigger":"%s"}\n' \
    "$(json_esc "$effective_cwd")" "$(json_esc "$session_id")" "$(json_esc "$transcript")" "$(date +%s)" "$(json_esc "$trigger")" \
    > "$signal_file"
