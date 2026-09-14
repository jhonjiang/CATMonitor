// Package npu_dvfs provides native DVFS control over Ascend NPUs via the
// driver's DSMI host library (libdrvdsmi_host.so), loaded at runtime with
// dlopen. It reimplements the semantics of the former external dvfs.py tool:
//
//   - CloseIdle/OpenIdle: disable/enable a device's idle downclocking
//     (DSMI LP idle switch, main cmd 8 / sub cmd 11)
//   - SetAicFreq: pin a device's AICore frequency via the DSMI stress-test
//     interface (main cmd 8 / sub cmd 12, STRESS_ADJ_AIC + STRESS_FREQ_SET)
//   - RatedFreq: a device's rated AICore frequency (frequency type 9)
//   - DeviceIDs: enumerate all device ids (global device numbering, matches
//     the snapshot's npu_id × chips_per_card + chip_id)
//
// Setting a frequency ABOVE a device's rated value via these per-device calls
// is not supported; the external npu_turbo tool (see internal/source/npu_turbo)
// is the only supported way to raise above rated, and it raises ALL devices
// at once.
//
// Hardware constraints honored by callers (from the former dvfs.py notes): on
// dual-chip cards the slave die (odd device id) cannot run above the master
// die (even device id) of the same card.
//
// The CGo dlopen binding lives in npu_dvfs_linux.go behind `linux && cgo`,
// so default builds (and non-Linux cross-compiles) get the not-available stub
// in npu_dvfs_stub.go and degrade gracefully. dlopen means build machines do
// NOT need the Ascend driver installed; the library is resolved at runtime on
// the NPU host. Tests must never call the mutating operations (SetAicFreq /
// CloseIdle / OpenIdle) — they touch real hardware.
package npu_dvfs

import "errors"

// errNotAvailable is returned when no DSMI library could be loaded (no CGo
// build, or the Ascend driver library is absent on this host).
var errNotAvailable = errors.New("npu_dvfs: not available (libdrvdsmi_host.so not loaded — is this an Ascend NPU host?)")

// Source is the native DVFS control surface consumed by the nputurbo
// actuator. All methods are synchronous in-process DSMI calls.
type Source interface {
	Available() bool
	// DeviceIDs enumerates all NPU device ids (global numbering).
	DeviceIDs() ([]int, error)
	// RatedFreq returns the device's rated AICore frequency (MHz).
	RatedFreq(devID int) (int, error)
	// CloseIdle disables the device's idle downclocking (pin precondition).
	CloseIdle(devID int) error
	// OpenIdle re-enables the device's idle downclocking.
	OpenIdle(devID int) error
	// SetAicFreq pins the device's AICore frequency (MHz, at or below rated).
	SetAicFreq(devID int, freqMHz int) error
}

// Default returns the platform-default Source: the CGo dlopen binding on
// Linux with CGo enabled, a not-available stub otherwise.
func Default() Source { return defaultSrc }
