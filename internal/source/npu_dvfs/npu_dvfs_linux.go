//go:build linux && cgo

package npu_dvfs

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>

struct dvfs_idle_cfg { unsigned char idle_switch; };
struct dvfs_aic_cfg  { unsigned char type; unsigned char set_restore;
                       unsigned short value; char resv[32]; };

static void *g_handle;
static int dvfs_load(const char *path) {
	if (g_handle != NULL) return 0;
	g_handle = dlopen(path, RTLD_NOW);
	return g_handle == NULL ? -1 : 0;
}
static int dvfs_avail(void) { return g_handle != NULL; }
static int dvfs_get_device_count(const char *sym, int *out) {
	int (*f)(int *) = (int (*)(int *))dlsym(g_handle, sym);
	return f == NULL ? -9999 : f(out);
}
static int dvfs_list_device(const char *sym, int *ids, int count) {
	int (*f)(int *, int) = (int (*)(int *, int))dlsym(g_handle, sym);
	return f == NULL ? -9999 : f(ids, count);
}
static int dvfs_get_freq(const char *sym, int dev, int type, unsigned int *out) {
	int (*f)(int, int, unsigned int *) = (int (*)(int, int, unsigned int *))dlsym(g_handle, sym);
	return f == NULL ? -9999 : f(dev, type, out);
}
static int dvfs_set_idle(const char *sym, int dev, unsigned char on) {
	struct dvfs_idle_cfg cfg; cfg.idle_switch = on;
	int (*f)(int, unsigned int, unsigned int, void *, int) =
		(int (*)(int, unsigned int, unsigned int, void *, int))dlsym(g_handle, sym);
	return f == NULL ? -9999 : f(dev, 8, 11, &cfg, (int)sizeof(cfg));
}
static int dvfs_set_aic_freq(const char *sym, int dev, unsigned short freq) {
	struct dvfs_aic_cfg cfg; cfg.type = 0; cfg.set_restore = 2; cfg.value = freq;
	for (int i = 0; i < 32; i++) cfg.resv[i] = 0;
	int (*f)(int, unsigned int, unsigned int, void *, int) =
		(int (*)(int, unsigned int, unsigned int, void *, int))dlsym(g_handle, sym);
	return f == NULL ? -9999 : f(dev, 8, 12, &cfg, (int)sizeof(cfg));
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

// dsmiSoPath is the Ascend driver's DSMI host library, resolved at runtime.
const dsmiSoPath = "/usr/local/Ascend/driver/lib64/driver/libdrvdsmi_host.so"

var (
	loadOnce sync.Once
	loaded   bool
)

// loadDSMI dlopens the DSMI library once; false when it cannot be loaded
// (non-NPU host) — every operation then fails with errNotAvailable.
func loadDSMI() bool {
	loadOnce.Do(func() {
		cpath := C.CString(dsmiSoPath)
		defer C.free(unsafe.Pointer(cpath))
		loaded = C.dvfs_load(cpath) == 0
	})
	return loaded
}

// dlopenSource is the real Source: DSMI calls through dlsym'd function
// pointers, mirroring the former dvfs.py ctypes bindings exactly (same
// symbols, main/sub commands, and struct layouts).
type dlopenSource struct{}

var defaultSrc Source = dlopenSource{}

// callSym resolves `sym` in the loaded library and invokes fn with it.
func callSym(sym string, fn func(cs *C.char) C.int) error {
	if !loadDSMI() {
		return errNotAvailable
	}
	cs := C.CString(sym)
	defer C.free(unsafe.Pointer(cs))
	if ret := fn(cs); ret != 0 {
		return fmt.Errorf("npu_dvfs: %s returned %d", sym, int(ret))
	}
	return nil
}

func (dlopenSource) Available() bool { return loadDSMI() }

func (dlopenSource) DeviceIDs() ([]int, error) {
	var count C.int
	if err := callSym("dsmi_get_device_count", func(cs *C.char) C.int {
		return C.dvfs_get_device_count(cs, &count)
	}); err != nil {
		return nil, err
	}
	if count <= 0 {
		return nil, fmt.Errorf("npu_dvfs: dsmi_get_device_count returned %d devices", int(count))
	}
	ids := make([]C.int, count)
	if err := callSym("dsmi_list_device", func(cs *C.char) C.int {
		return C.dvfs_list_device(cs, &ids[0], count)
	}); err != nil {
		return nil, err
	}
	out := make([]int, int(count))
	for i, v := range ids {
		out[i] = int(v)
	}
	return out, nil
}

func (dlopenSource) RatedFreq(devID int) (int, error) {
	var freq C.uint
	if err := callSym("dsmi_get_device_frequency", func(cs *C.char) C.int {
		return C.dvfs_get_freq(cs, C.int(devID), 9, &freq) // 9 = AICore max (rated)
	}); err != nil {
		return 0, err
	}
	return int(freq), nil
}

func (dlopenSource) CloseIdle(devID int) error {
	return callSym("dsmi_set_device_info", func(cs *C.char) C.int {
		return C.dvfs_set_idle(cs, C.int(devID), 0)
	})
}

func (dlopenSource) OpenIdle(devID int) error {
	return callSym("dsmi_set_device_info", func(cs *C.char) C.int {
		return C.dvfs_set_idle(cs, C.int(devID), 1)
	})
}

func (dlopenSource) SetAicFreq(devID int, freqMHz int) error {
	return callSym("dsmi_set_device_info", func(cs *C.char) C.int {
		return C.dvfs_set_aic_freq(cs, C.int(devID), C.ushort(freqMHz))
	})
}
