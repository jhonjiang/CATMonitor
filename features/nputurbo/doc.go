//go:build linux

// Package nputurbo implements the NPU slow-card upclock actuator.
// It periodically runs the external straggler detector to obtain a slow-card
// list (jsonl), uses a fixed baseline current frequency A = 1800 MHz (it does
// NOT query aicore_freq from snapshot_npu.json), computes a target frequency
// B = round50(min(A*score, M)) = round50(min(1800*score, M)), and execs
// /var/npu_turbo -i <id> -f <B> to raise the card's frequency. Cards that drop
// off the slow list are restored to their pre-boost frequency.
//
// Linux-only: actuation execs an external binary expected on Ascend hosts.
// Non-Linux builds do not import this package; cmd/catmonitor provides a
// no-op nputurbo stub for other platforms.
package nputurbo
