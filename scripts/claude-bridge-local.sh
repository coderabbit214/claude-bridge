#!/usr/bin/env zsh
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
MUX="$ROOT_DIR/scripts/claude_bridge_mux.py"

sid="local-$(date +%s%N)"
input_pipe="/tmp/claude-input-$sid.pipe"
output_pipe="/tmp/claude-hook-$sid.pipe"
registry_dir="/tmp/claude-bridge-sessions"
manifest_path="$registry_dir/$sid.json"

mkdir -p "$registry_dir"
rm -f "$input_pipe" "$output_pipe" "$manifest_path"
mkfifo "$input_pipe"
mkfifo "$output_pipe"

cleanup() {
  rm -f "$input_pipe" "$output_pipe" "$manifest_path"
}
trap cleanup EXIT INT TERM

cat > "$manifest_path" <<EOF
{"id":"$sid","pid":$$,"work_dir":"$PWD","input_pipe":"$input_pipe","output_pipe":"$output_pipe"}
EOF

export CLAUDE_SESSION_ID="$sid"
export CLAUDE_HOOK_PIPE="$output_pipe"

printf '[claude-bridge] local interactive session /%s\n' "$sid"
python3 "$MUX" "$input_pipe" -- claude
