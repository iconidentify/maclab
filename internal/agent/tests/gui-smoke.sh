#!/bin/bash
# Runs inside the GUI user's Hyprland session: checks the compositor answers,
# a monitor is up, and takes a screenshot.
set -u
out=${MACLAB_OUT:-.}
for _ in $(seq 1 30); do
    hyprctl -j monitors > "$out/monitors.json" 2>/dev/null && break
    sleep 2
done
if ! grep -q '"name"' "$out/monitors.json" 2>/dev/null; then
    echo "FAIL: hyprctl reports no monitors"; exit 1
fi
echo "monitors: $(grep -c '"name"' "$out/monitors.json")"
hyprctl -j clients > "$out/clients.json" 2>&1
hyprctl version > "$out/hyprland-version.txt" 2>&1
if ! grim "$out/screenshot.png"; then
    echo "FAIL: grim could not take a screenshot"; exit 1
fi
echo "screenshot: $(stat -c %s "$out/screenshot.png") bytes"
