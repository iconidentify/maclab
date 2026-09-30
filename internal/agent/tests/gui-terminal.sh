#!/bin/bash
# Opens a terminal, types into it, and screenshots the result: exercises
# input, window creation and rendering end to end.
set -u
out=${MACLAB_OUT:-.}
term=$(command -v alacritty || command -v foot || command -v kitty || command -v ghostty)
[ -n "$term" ] || { echo "FAIL: no terminal emulator found"; exit 1; }
before=$(hyprctl -j clients | grep -c '"pid"')
hyprctl dispatch exec "$term" >/dev/null
for _ in $(seq 1 20); do
    now=$(hyprctl -j clients | grep -c '"pid"')
    [ "$now" -gt "$before" ] && break
    sleep 0.5
done
[ "$now" -gt "$before" ] || { echo "FAIL: terminal window never appeared"; exit 1; }
sleep 1
if command -v wtype >/dev/null; then
    wtype "echo maclab-gui-ok" && wtype -k Return
fi
sleep 1
grim "$out/screenshot.png" || { echo "FAIL: grim failed"; exit 1; }
hyprctl dispatch killactive >/dev/null
echo "terminal opened ($term), screenshot taken"
