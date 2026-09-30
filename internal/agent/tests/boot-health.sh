#!/bin/bash
# Waits for boot to settle, then records system state, failed units and
# kernel errors. Fails only if systemd ended up in emergency/maintenance
# or never finished starting; labd diffs the kernel errors against the baseline.
set -u
out=${MACLAB_OUT:-.}
state=starting
for _ in $(seq 1 60); do
    state=$(systemctl is-system-running 2>/dev/null)
    [ "$state" = running ] || [ "$state" = degraded ] && break
    sleep 2
done
echo "system state: $state"
systemctl --failed --no-legend --plain > "$out/failed-units.txt" 2>&1
echo "failed units: $(wc -l < "$out/failed-units.txt")"
cat "$out/failed-units.txt"
echo "tainted: $(cat /proc/sys/kernel/tainted)"
journalctl -k -b 0 -p err -o cat --no-pager > "$out/kernel-errors.txt" 2>&1
echo "kernel errors: $(wc -l < "$out/kernel-errors.txt")"
systemd-analyze > "$out/boot-time.txt" 2>&1 && cat "$out/boot-time.txt"
case "$state" in
    running|degraded) exit 0 ;;
    *) echo "FAIL: system state is $state"; exit 1 ;;
esac
