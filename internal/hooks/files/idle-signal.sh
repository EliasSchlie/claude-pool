#!/bin/bash
# Signals when a Claude session becomes idle or starts processing.
#
# IMPORTANT: Idle signals must have NO FALSE POSITIVES. The "stop" trigger
# defers writing for IDLE_VERIFY_DELAY seconds and verifies the transcript
# size hasn't changed.
#
# Usage: idle-signal.sh write [stop|tool|permission|session-clear]
#        idle-signal.sh clear

source "$(dirname "$0")/common.sh"

SYSTEM_ENTRY_WAIT=4
IDLE_VERIFY_DELAY=3
mkdir -p "$SIGNAL_DIR"

claude_pid="$PPID"
signal_file="$SIGNAL_DIR/$claude_pid"

# Cross-platform file size in bytes
file_size() {
    stat -f %z "$1" 2>/dev/null || stat -c %s "$1" 2>/dev/null || echo 0
}

# Read hook input from stdin
read_input() {
    local line=""
    read -t 1 -r line 2>/dev/null || true
    if [ -n "$line" ]; then
        echo "$line"
        cat 2>/dev/null
    fi
}

case "${1:-}" in
    write)
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

        # Prefer hook-reported cwd (tracks Bash tool cd), fall back to process cwd
        effective_cwd="${hook_cwd:-$(pwd)}"
        signal_json=$(printf '{"cwd":"%s","session_id":"%s","transcript":"%s","ts":%d,"trigger":"%s"}\n' \
            "$(json_esc "$effective_cwd")" "$(json_esc "$session_id")" "$(json_esc "$transcript")" "$(date +%s)" "$(json_esc "$trigger")")

        if [ "$trigger" = "stop" ]; then
            pending="$signal_file.pending"
            echo "$$" > "$pending"

            (
                trap 'rm -f "$pending"' EXIT
                sleep "$SYSTEM_ENTRY_WAIT"

                [ ! -f "$pending" ] && exit 0
                [ "$(cat "$pending" 2>/dev/null)" != "$$" ] && exit 0

                before=""
                if [ -n "$transcript" ] && [ -f "$transcript" ]; then
                    before=$(file_size "$transcript")
                fi

                sleep "$IDLE_VERIFY_DELAY"

                [ ! -f "$pending" ] && exit 0
                [ "$(cat "$pending" 2>/dev/null)" != "$$" ] && exit 0

                if [ -n "$before" ] && [ -f "$transcript" ]; then
                    after=$(file_size "$transcript")
                    [ "$before" != "$after" ] && exit 0
                fi

                if [ "$(cat "$pending" 2>/dev/null)" = "$$" ]; then
                    printf '%s\n' "$signal_json" > "$signal_file"
                fi
            ) &
            disown
        else
            rm -f "$signal_file.pending"
            printf '%s\n' "$signal_json" > "$signal_file"
        fi
        ;;
    clear)
        rm -f "$signal_file" "$signal_file.pending"
        ;;
esac
