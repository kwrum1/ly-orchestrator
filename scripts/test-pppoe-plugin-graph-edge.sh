#!/bin/bash
# Regression guard for the AC reconnect crash:
# pppoeclient-session-output must never invalidate a graph next slot while a
# worker can still hold that slot. Reusing an existing edge is safe; writing
# ~0 into node->next_nodes[old_slot] is not.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
source_file=$root/runtime/vpp-pppoe-client-hijiajun/pppoeclient/pppoeclient.c
grep -q 'pppoeclient_force_hw_output' "$source_file"
grep -q 'pppoeclient_refresh_hw_output (pem, c);' "$source_file"
if grep -q 'node->next_nodes\[old_slot\] = ~0' "$source_file"; then
  echo "FAIL: reconnect path invalidates an in-flight graph next slot" >&2
  exit 1
fi
echo "PASS: pppoeclient reconnect path reuses graph next slots without invalidating in-flight workers"
