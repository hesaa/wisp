package app

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	psapi                 = windows.NewLazySystemDLL("psapi.dll")
	pGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

func currentMem() (wsMB, commitMB float64) {
	var pmc processMemoryCounters
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	h, _ := windows.GetCurrentProcess()
	pGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb))
	return float64(pmc.workingSetSize) / (1024 * 1024), float64(pmc.pagefileUsage) / (1024 * 1024)
}

func TestTrimMemoryReducesWorkingSet(t *testing.T) {
	// Allocate some transient data
	buf := make([][]byte, 100)
	for i := range buf {
		buf[i] = make([]byte, 1024*1024) // 100MB transient
		for j := 0; j < len(buf[i]); j += 4096 {
			buf[i][j] = 1 // touch pages so they are in working set
		}
	}

	wsBefore, commitBefore := currentMem()
	t.Logf("Before release: WorkingSet=%.2f MB, Commit=%.2f MB", wsBefore, commitBefore)

	// Release memory
	buf = nil
	trimMemory()

	wsAfter, commitAfter := currentMem()
	t.Logf("After trimMemory: WorkingSet=%.2f MB, Commit=%.2f MB", wsAfter, commitAfter)

	if wsAfter >= wsBefore {
		t.Errorf("trimMemory did not reduce working set: before=%.2f, after=%.2f", wsBefore, wsAfter)
	}
	if wsAfter > 30 {
		t.Logf("Notice: WorkingSet trimmed to %.2f MB", wsAfter)
	}
}
