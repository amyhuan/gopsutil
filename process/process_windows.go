// +build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"

	cpu "github.com/amyhuan/gopsutil/cpu"
	"github.com/amyhuan/gopsutil/internal/common"
	net "github.com/amyhuan/gopsutil/net"
	"golang.org/x/sys/windows"
)

var (
	modntdll             = windows.NewLazySystemDLL("ntdll.dll")
	procNtResumeProcess  = modntdll.NewProc("NtResumeProcess")
	procNtSuspendProcess = modntdll.NewProc("NtSuspendProcess")

	modpsapi                     = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo     = modpsapi.NewProc("GetProcessMemoryInfo")
	procGetProcessImageFileNameW = modpsapi.NewProc("GetProcessImageFileNameW")

	advapi32                  = windows.NewLazySystemDLL("advapi32.dll")
	procLookupPrivilegeValue  = advapi32.NewProc("LookupPrivilegeValueW")
	procAdjustTokenPrivileges = advapi32.NewProc("AdjustTokenPrivileges")

	procQueryFullProcessImageNameW = common.Modkernel32.NewProc("QueryFullProcessImageNameW")
	procGetPriorityClass           = common.Modkernel32.NewProc("GetPriorityClass")
	procGetProcessIoCounters       = common.Modkernel32.NewProc("GetProcessIoCounters")
	procGetNativeSystemInfo        = common.Modkernel32.NewProc("GetNativeSystemInfo")

	processorArchitecture uint
)

const processQueryInformation = windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.PROCESS_QUERY_INFORMATION // WinXP doesn't know PROCESS_QUERY_LIMITED_INFORMATION

type SystemProcessInformation struct {
	NextEntryOffset   uint64
	NumberOfThreads   uint64
	Reserved1         [48]byte
	Reserved2         [3]byte
	UniqueProcessID   uintptr
	Reserved3         uintptr
	HandleCount       uint64
	Reserved4         [4]byte
	Reserved5         [11]byte
	PeakPagefileUsage uint64
	PrivatePageCount  uint64
	Reserved6         [6]uint64
}

type systemProcessorInformation struct {
	ProcessorArchitecture uint16
	ProcessorLevel        uint16
	ProcessorRevision     uint16
	Reserved              uint16
	ProcessorFeatureBits  uint16
}

type systemInfo struct {
	wProcessorArchitecture      uint16
	wReserved                   uint16
	dwPageSize                  uint32
	lpMinimumApplicationAddress uintptr
	lpMaximumApplicationAddress uintptr
	dwActiveProcessorMask       uintptr
	dwNumberOfProcessors        uint32
	dwProcessorType             uint32
	dwAllocationGranularity     uint32
	wProcessorLevel             uint16
	wProcessorRevision          uint16
}

// Memory_info_ex is different between OSes
type MemoryInfoExStat struct {
}

type MemoryMapsStat struct {
}

// ioCounters is an equivalent representation of IO_COUNTERS in the Windows API.
// https://docs.microsoft.com/windows/win32/api/winnt/ns-winnt-io_counters
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type processBasicInformation32 struct {
	Reserved1       uint32
	PebBaseAddress  uint32
	Reserved2       uint32
	Reserved3       uint32
	UniqueProcessId uint32
	Reserved4       uint32
}

type processBasicInformation64 struct {
	Reserved1       uint64
	PebBaseAddress  uint64
	Reserved2       uint64
	Reserved3       uint64
	UniqueProcessId uint64
	Reserved4       uint64
}

type winLUID struct {
	LowPart  winDWord
	HighPart winLong
}

// LUID_AND_ATTRIBUTES
type winLUIDAndAttributes struct {
	Luid       winLUID
	Attributes winDWord
}

// TOKEN_PRIVILEGES
type winTokenPriviledges struct {
	PrivilegeCount winDWord
	Privileges     [1]winLUIDAndAttributes
}

type winLong int32
type winDWord uint32

func init() {
	var systemInfo systemInfo

	procGetNativeSystemInfo.Call(uintptr(unsafe.Pointer(&systemInfo)))
	processorArchitecture = uint(systemInfo.wProcessorArchitecture)

	// enable SeDebugPrivilege https://github.com/midstar/proci/blob/6ec79f57b90ba3d9efa2a7b16ef9c9369d4be875/proci_windows.go#L80-L119
	handle, err := syscall.GetCurrentProcess()
	if err != nil {
		return
	}

	var token syscall.Token
	err = syscall.OpenProcessToken(handle, 0x0028, &token)
	if err != nil {
		return
	}
	defer token.Close()

	tokenPriviledges := winTokenPriviledges{PrivilegeCount: 1}
	lpName := syscall.StringToUTF16("SeDebugPrivilege")
	ret, _, _ := procLookupPrivilegeValue.Call(
		0,
		uintptr(unsafe.Pointer(&lpName[0])),
		uintptr(unsafe.Pointer(&tokenPriviledges.Privileges[0].Luid)))
	if ret == 0 {
		return
	}

	tokenPriviledges.Privileges[0].Attributes = 0x00000002 // SE_PRIVILEGE_ENABLED

	procAdjustTokenPrivileges.Call(
		uintptr(token),
		0,
		uintptr(unsafe.Pointer(&tokenPriviledges)),
		uintptr(unsafe.Sizeof(tokenPriviledges)),
		0,
		0)
}

func pidsWithContext(ctx context.Context) ([]int32, error) {
	// inspired by https://gist.github.com/henkman/3083408
	// and https://github.com/giampaolo/psutil/blob/1c3a15f637521ba5c0031283da39c733fda53e4c/psutil/arch/windows/process_info.c#L315-L329
	var ret []int32
	var read uint32 = 0
	var psSize uint32 = 1024
	const dwordSize uint32 = 4

	for {
		ps := make([]uint32, psSize)
		if err := windows.EnumProcesses(ps, &read); err != nil {
			return nil, err
		}
		if uint32(len(ps)) == read { // ps buffer was too small to host every results, retry with a bigger one
			psSize += 1024
			continue
		}
		for _, pid := range ps[:read/dwordSize] {
			ret = append(ret, int32(pid))
		}
		return ret, nil

	}

}

func PidExistsWithContext(ctx context.Context, pid int32) (bool, error) {
	if pid == 0 { // special case for pid 0 System Idle Process
		return true, nil
	}
	if pid < 0 {
		return false, fmt.Errorf("invalid pid %v", pid)
	}
	if pid%4 != 0 {
		// OpenProcess will succeed even on non-existing pid here https://devblogs.microsoft.com/oldnewthing/20080606-00/?p=22043
		// so we list every pid just to be sure and be future-proof
		pids, err := PidsWithContext(ctx)
		if err != nil {
			return false, err
		}
		for _, i := range pids {
			if i == pid {
				return true, err
			}
		}
		return false, err
	}
	const STILL_ACTIVE = 259 // https://docs.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-getexitcodeprocess
	h, err := windows.OpenProcess(processQueryInformation, false, uint32(pid))
	if err == windows.ERROR_ACCESS_DENIED {
		return true, nil
	}
	if err == windows.ERROR_INVALID_PARAMETER {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	var exitCode uint32
	err = windows.GetExitCodeProcess(h, &exitCode)
	return exitCode == STILL_ACTIVE, err
}

func (p *Process) PpidWithContext(ctx context.Context) (int32, error) {
	// if cached already, return from cache
	cachedPpid := p.getPpid()
	if cachedPpid != 0 {
		return cachedPpid, nil
	}

	ppid, _, _, err := getFromSnapProcess(p.Pid)
	if err != nil {
		return 0, err
	}

	// no errors and not cached already, so cache it
	p.setPpid(ppid)

	return ppid, nil
}

func (p *Process) NameWithContext(ctx context.Context) (string, error) {
	ppid, _, name, err := getFromSnapProcess(p.Pid)
	if err != nil {
		return "", fmt.Errorf("could not get Name: %s", err)
	}

	// if no errors and not cached already, cache ppid
	p.parent = ppid
	if 0 == p.getPpid() {
		p.setPpid(ppid)
	}

	return name, nil
}

func (p *Process) TgidWithContext(ctx context.Context) (int32, error) {
	return 0, common.ErrNotImplementedError
}

func (p *Process) ExeWithContext(ctx context.Context) (string, error) {
	c, err := windows.OpenProcess(processQueryInformation, false, uint32(p.Pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(c)
	buf := make([]uint16, syscall.MAX_LONG_PATH)
	size := uint32(syscall.MAX_LONG_PATH)
	if err := procQueryFullProcessImageNameW.Find(); err == nil { // Vista+
		ret, _, err := procQueryFullProcessImageNameW.Call(
			uintptr(c),
			uintptr(0),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&size)))
		if ret == 0 {
			return "", err
		}
		return windows.UTF16ToString(buf[:]), nil
	}
	// XP fallback
	ret, _, err := procGetProcessImageFileNameW.Call(uintptr(c), uintptr(unsafe.Pointer(&buf[0])), uintptr(size))
	if ret == 0 {
		return "", err
	}
	return common.ConvertDOSPath(windows.UTF16ToString(buf[:])), nil
}

func (p *Process) CmdlineWithContext(_ context.Context) (string, error) {
	cmdline, err := getProcessCommandLine(p.Pid)
	if err != nil {
		return "", fmt.Errorf("could not get CommandLine: %s", err)
	}
	return cmdline, nil
}

func (p *Process) CmdlineSliceWithContext(ctx context.Context) ([]string, error) {
	cmdline, err := p.CmdlineWithContext(ctx)
	if err != nil {
		return nil, err
	}
	return strings.Split(cmdline, " "), nil
}

func (p *Process) createTimeWithContext(ctx context.Context) (int64, error) {
	ru, err := getRusage(p.Pid)
	if err != nil {
		return 0, fmt.Errorf("could not get CreationDate: %s", err)
	}

	return ru.CreationTime.Nanoseconds() / 1000000, nil
}

func (p *Process) CwdWithContext(ctx context.Context) (string, error) {
	return "", common.ErrNotImplementedError
}

func (p *Process) ParentWithContext(ctx context.Context) (*Process, error) {
	ppid, err := p.PpidWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not get ParentProcessID: %s", err)
	}

	return NewProcessWithContext(ctx, ppid)
}

func (p *Process) StatusWithContext(ctx context.Context) (string, error) {
	return "", common.ErrNotImplementedError
}

func (p *Process) ForegroundWithContext(ctx context.Context) (bool, error) {
	return false, common.ErrNotImplementedError
}

func (p *Process) UsernameWithContext(ctx context.Context) (string, error) {
	pid := p.Pid
	c, err := windows.OpenProcess(processQueryInformation, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(c)

	var token syscall.Token
	err = syscall.OpenProcessToken(syscall.Handle(c), syscall.TOKEN_QUERY, &token)
	if err != nil {
		return "", err
	}
	defer token.Close()
	tokenUser, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}

	user, domain, _, err := tokenUser.User.Sid.LookupAccount("")
	return domain + "\\" + user, err
}

func (p *Process) UidsWithContext(ctx context.Context) ([]int32, error) {
	return nil, common.ErrNotImplementedError
}

func (p *Process) GidsWithContext(ctx context.Context) ([]int32, error) {
	return nil, common.ErrNotImplementedError
}

func (p *Process) GroupsWithContext(ctx context.Context) ([]int32, error) {
	return nil, common.ErrNotImplementedError
}

func (p *Process) TerminalWithContext(ctx context.Context) (string, error) {
	return "", common.ErrNotImplementedError
}

// priorityClasses maps a win32 priority class to its WMI equivalent Win32_Process.Priority
// https://docs.microsoft.com/en-us/windows/desktop/api/processthreadsapi/nf-processthreadsapi-getpriorityclass
// https://docs.microsoft.com/en-us/windows/desktop/cimwin32prov/win32-process
var priorityClasses = map[int]int32{
	0x00008000: 10, // ABOVE_NORMAL_PRIORITY_CLASS
	0x00004000: 6,  // BELOW_NORMAL_PRIORITY_CLASS
	0x00000080: 13, // HIGH_PRIORITY_CLASS
	0x00000040: 4,  // IDLE_PRIORITY_CLASS
	0x00000020: 8,  // NORMAL_PRIORITY_CLASS
	0x00000100: 24, // REALTIME_PRIORITY_CLASS
}

func (p *Process) NiceWithContext(ctx context.Context) (int32, error) {
	c, err := windows.OpenProcess(processQueryInformation, false, uint32(p.Pid))
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(c)
	ret, _, err := procGetPriorityClass.Call(uintptr(c))
	if ret == 0 {
		return 0, err
	}
	priority, ok := priorityClasses[int(ret)]
	if !ok {
		return 0, fmt.Errorf("unknown priority class %v", ret)
	}
	return priority, nil
}

func (p *Process) IOniceWithContext(ctx context.Context) (int32, error) {
	return 0, common.ErrNotImplementedError
}

func (p *Process) RlimitWithContext(ctx context.Context) ([]RlimitStat, error) {
	return nil, common.ErrNotImplementedError
}

func (p *Process) RlimitUsageWithContext(ctx context.Context, gatherUsed bool) ([]RlimitStat, error) {
	return nil, common.ErrNotImplementedError
}

func (p *Process) IOCountersWithContext(ctx context.Context) (*IOCountersStat, error) {
	c, err := windows.OpenProcess(processQueryInformation, false, uint32(p.Pid))
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(c)
	var ioCounters ioCounters
	ret, _, err := procGetProcessIoCounters.Call(uintptr(c), uintptr(unsafe.Pointer(&ioCounters)))
	if ret == 0 {
		return nil, err
	}
	stats := &IOCountersStat{
		ReadCount:  ioCounters.ReadOperationCount,
		ReadBytes:  ioCounters.ReadTransferCount,
		WriteCount: ioCounters.WriteOperationCount,
		WriteBytes: ioCounters.WriteTransferCount,
	}

	return stats, nil
}

func (p *Process) NumCtxSwitchesWithContext(ctx context.Context) (*NumCtxSwitchesStat, error) {
	return nil, common.ErrNotImplementedError
}

func (p *Process) NumFDsWithContext(ctx context.Context) (int32, error) {
	return 0, common.ErrNotImplementedError
}

func (p *Process) NumThreadsWithContext(ctx context.Context) (int32, error) {
	ppid, ret, _, err := getFromSnapProcess(p.Pid)
	if err != nil {
		return 0, err
	}

	// if no errors and not cached already, cache ppid
	p.parent = ppid
	if 0 == p.getPpid() {
		p.setPpid(ppid)
	}

	return ret, nil
}

func (p *Process) ThreadsWithContext(ctx context.Context) (map[int32]*cpu.TimesStat, error) {
	return nil, common.ErrNotImplementedError
}

func (p *Process) TimesWithContext(ctx context.Context) (*cpu.TimesStat, error) {
	sysTimes, err := getProcessCPUTimes(p.Pid)
	if err != nil {
		return nil, err
	}

	// User and kernel times are represented as a FILETIME structure
	// which contains a 64-bit value representing the number of
	// 100-nanosecond intervals since January 1, 1601 (UTC):
	// http://msdn.microsoft.com/en-us/library/ms724284(VS.85).aspx
	// To convert it into a float representing the seconds that the
	// process has executed in user/kernel mode I borrowed the code
	// below from psutil's _psutil_windows.c, and in turn from Python's
	// Modules/posixmodule.c

	user := float64(sysTimes.UserTime.HighDateTime)*429.4967296 + float64(sysTimes.UserTime.LowDateTime)*1e-7
	kernel := float64(sysTimes.KernelTime.HighDateTime)*429.4967296 + float64(sysTimes.KernelTime.LowDateTime)*1e-7

	return &cpu.TimesStat{
		User:   user,
		System: kernel,
	}, nil
}

func (p *Process) CPUAffinityWithContext(ctx context.Context) ([]int32, error) {
	return nil, common.ErrNotImplementedError
}

func (p *Process) MemoryInfoWithContext(ctx context.Context) (*MemoryInfoStat, error) {
	mem, err := getMemoryInfo(p.Pid)
	if err != nil {
		return nil, err
	}

	ret := &MemoryInfoStat{
		RSS: uint64(mem.WorkingSetSize),
		VMS: uint64(mem.PagefileUsage),
	}

	return ret, nil
}

func (p *Process) MemoryInfoExWithContext(ctx context.Context) (*MemoryInfoExStat, error) {
	return nil, common.ErrNotImplementedError
}

func (p *Process) PageFaultsWithContext(ctx context.Context) (*PageFaultsStat, error) {
	return nil, common.ErrNotImplementedError
}

func (p *Process) ChildrenWithContext(ctx context.Context) ([]*Process, error) {
	out := []*Process{}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, uint32(0))
	if err != nil {
		return out, err
	}
	defer windows.CloseHandle(snap)
	var pe32 windows.ProcessEntry32
	pe32.Size = uint32(unsafe.Sizeof(pe32))
	if err := windows.Process32First(snap, &pe32); err != nil {
		return out, err
	}
	for {
		if pe32.ParentProcessID == uint32(p.Pid) {
			p, err := NewProcessWithContext(ctx, int32(pe32.ProcessID))
			if err == nil {
				out = append(out, p)
			}
		}
		if err = windows.Process32Next(snap, &pe32); err != nil {
			break
		}
	}
	return out, nil
}

func (p *Process) OpenFilesWithContext(ctx context.Context) ([]OpenFilesStat, error) {
	return nil, common.ErrNotImplementedError
}

func (p *Process) ConnectionsWithContext(ctx context.Context) ([]net.ConnectionStat, error) {
	return net.ConnectionsPidWithContext(ctx, "all", p.Pid)
}

func (p *Process) ConnectionsMaxWithContext(ctx context.Context, max int) ([]net.ConnectionStat, error) {
	return nil, common.ErrNotImplementedError
}

func (p *Process) NetIOCountersWithContext(ctx context.Context, pernic bool) ([]net.IOCountersStat, error) {
	return nil, common.ErrNotImplementedError
}

func (p *Process) MemoryMapsWithContext(ctx context.Context, grouped bool) (*[]MemoryMapsStat, error) {
	return nil, common.ErrNotImplementedError
}

func (p *Process) SendSignalWithContext(ctx context.Context, sig syscall.Signal) error {
	return common.ErrNotImplementedError
}

func (p *Process) SuspendWithContext(ctx context.Context) error {
	c, err := windows.OpenProcess(windows.PROCESS_SUSPEND_RESUME, false, uint32(p.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(c)

	r1, _, _ := procNtSuspendProcess.Call(uintptr(c))
	if r1 != 0 {
		// See https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-erref/596a1078-e883-4972-9bbc-49e60bebca55
		return fmt.Errorf("NtStatus='0x%.8X'", r1)
	}

	return nil
}

func (p *Process) ResumeWithContext(ctx context.Context) error {
	c, err := windows.OpenProcess(windows.PROCESS_SUSPEND_RESUME, false, uint32(p.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(c)

	r1, _, _ := procNtResumeProcess.Call(uintptr(c))
	if r1 != 0 {
		// See https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-erref/596a1078-e883-4972-9bbc-49e60bebca55
		return fmt.Errorf("NtStatus='0x%.8X'", r1)
	}

	return nil
}

func (p *Process) TerminateWithContext(ctx context.Context) error {
	proc, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return err
	}
	err = windows.TerminateProcess(proc, 0)
	windows.CloseHandle(proc)
	return err
}

func (p *Process) KillWithContext(ctx context.Context) error {
	process := os.Process{Pid: int(p.Pid)}
	return process.Kill()
}

// retrieve Ppid in a thread-safe manner
func (p *Process) getPpid() int32 {
	p.parentMutex.RLock()
	defer p.parentMutex.RUnlock()
	return p.parent
}

// cache Ppid in a thread-safe manner (WINDOWS ONLY)
// see https://psutil.readthedocs.io/en/latest/#psutil.Process.ppid
func (p *Process) setPpid(ppid int32) {
	p.parentMutex.Lock()
	defer p.parentMutex.Unlock()
	p.parent = ppid
}

func getFromSnapProcess(pid int32) (int32, int32, string, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, uint32(pid))
	if err != nil {
		return 0, 0, "", err
	}
	defer windows.CloseHandle(snap)
	var pe32 windows.ProcessEntry32
	pe32.Size = uint32(unsafe.Sizeof(pe32))
	if err = windows.Process32First(snap, &pe32); err != nil {
		return 0, 0, "", err
	}
	for {
		if pe32.ProcessID == uint32(pid) {
			szexe := windows.UTF16ToString(pe32.ExeFile[:])
			return int32(pe32.ParentProcessID), int32(pe32.Threads), szexe, nil
		}
		if err = windows.Process32Next(snap, &pe32); err != nil {
			break
		}
	}
	return 0, 0, "", fmt.Errorf("couldn't find pid: %d", pid)
}

func ProcessesWithContext(ctx context.Context) ([]*Process, error) {
	out := []*Process{}

	pids, err := PidsWithContext(ctx)
	if err != nil {
		return out, fmt.Errorf("could not get Processes %s", err)
	}

	for _, pid := range pids {
		p, err := NewProcessWithContext(ctx, pid)
		if err != nil {
			continue
		}
		out = append(out, p)
	}

	return out, nil
}

func getRusage(pid int32) (*windows.Rusage, error) {
	var CPU windows.Rusage

	c, err := windows.OpenProcess(processQueryInformation, false, uint32(pid))
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(c)

	if err := windows.GetProcessTimes(c, &CPU.CreationTime, &CPU.ExitTime, &CPU.KernelTime, &CPU.UserTime); err != nil {
		return nil, err
	}

	return &CPU, nil
}

func getMemoryInfo(pid int32) (PROCESS_MEMORY_COUNTERS, error) {
	var mem PROCESS_MEMORY_COUNTERS
	c, err := windows.OpenProcess(processQueryInformation, false, uint32(pid))
	if err != nil {
		return mem, err
	}
	defer windows.CloseHandle(c)
	if err := getProcessMemoryInfo(c, &mem); err != nil {
		return mem, err
	}

	return mem, err
}

func getProcessMemoryInfo(h windows.Handle, mem *PROCESS_MEMORY_COUNTERS) (err error) {
	r1, _, e1 := syscall.Syscall(procGetProcessMemoryInfo.Addr(), 3, uintptr(h), uintptr(unsafe.Pointer(mem)), uintptr(unsafe.Sizeof(*mem)))
	if r1 == 0 {
		if e1 != 0 {
			err = error(e1)
		} else {
			err = syscall.EINVAL
		}
	}
	return
}

type SYSTEM_TIMES struct {
	CreateTime syscall.Filetime
	ExitTime   syscall.Filetime
	KernelTime syscall.Filetime
	UserTime   syscall.Filetime
}

func getProcessCPUTimes(pid int32) (SYSTEM_TIMES, error) {
	var times SYSTEM_TIMES

	h, err := windows.OpenProcess(processQueryInformation, false, uint32(pid))
	if err != nil {
		return times, err
	}
	defer windows.CloseHandle(h)

	err = syscall.GetProcessTimes(
		syscall.Handle(h),
		&times.CreateTime,
		&times.ExitTime,
		&times.KernelTime,
		&times.UserTime,
	)

	return times, err
}

func is32BitProcess(procHandle syscall.Handle) bool {
	var wow64 uint

	ret, _, _ := common.ProcNtQueryInformationProcess.Call(
		uintptr(procHandle),
		uintptr(common.ProcessWow64Information),
		uintptr(unsafe.Pointer(&wow64)),
		uintptr(unsafe.Sizeof(wow64)),
		uintptr(0),
	)
	if int(ret) >= 0 {
		if wow64 != 0 {
			return true
		}
	} else {
		//if the OS does not support the call, we fallback into the bitness of the app
		if unsafe.Sizeof(wow64) == 4 {
			return true
		}
	}
	return false
}

func getProcessCommandLine(pid int32) (string, error) {
	h, err := windows.OpenProcess(processQueryInformation|windows.PROCESS_VM_READ, false, uint32(pid))
	if err == windows.ERROR_ACCESS_DENIED || err == windows.ERROR_INVALID_PARAMETER {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(syscall.Handle(h))

	const (
		PROCESSOR_ARCHITECTURE_INTEL = 0
		PROCESSOR_ARCHITECTURE_ARM   = 5
		PROCESSOR_ARCHITECTURE_ARM64 = 12
		PROCESSOR_ARCHITECTURE_IA64  = 6
		PROCESSOR_ARCHITECTURE_AMD64 = 9
	)

	procIs32Bits := true
	switch processorArchitecture {
	case PROCESSOR_ARCHITECTURE_INTEL:
		fallthrough
	case PROCESSOR_ARCHITECTURE_ARM:
		procIs32Bits = true

	case PROCESSOR_ARCHITECTURE_ARM64:
		fallthrough
	case PROCESSOR_ARCHITECTURE_IA64:
		fallthrough
	case PROCESSOR_ARCHITECTURE_AMD64:
		procIs32Bits = is32BitProcess(syscall.Handle(h))

	default:
		//for other unknown platforms, we rely on process platform
		if unsafe.Sizeof(processorArchitecture) == 8 {
			procIs32Bits = false
		}
	}

	pebAddress := queryPebAddress(syscall.Handle(h), procIs32Bits)
	if pebAddress == 0 {
		return "", errors.New("cannot locate process PEB")
	}

	if procIs32Bits {
		buf := readProcessMemory(syscall.Handle(h), procIs32Bits, pebAddress+uint64(16), 4)
		if len(buf) != 4 {
			return "", errors.New("cannot locate process user parameters")
		}
		userProcParams := uint64(buf[0]) | (uint64(buf[1]) << 8) | (uint64(buf[2]) << 16) | (uint64(buf[3]) << 24)

		//read CommandLine field from PRTL_USER_PROCESS_PARAMETERS
		remoteCmdLine := readProcessMemory(syscall.Handle(h), procIs32Bits, userProcParams+uint64(64), 8)
		if len(remoteCmdLine) != 8 {
			return "", errors.New("cannot read cmdline field")
		}

		//remoteCmdLine is actually a UNICODE_STRING32
		//the first two bytes has the length
		cmdLineLength := uint(remoteCmdLine[0]) | (uint(remoteCmdLine[1]) << 8)
		if cmdLineLength > 0 {
			//and, at offset 4, is the pointer to the buffer
			bufferAddress := uint32(remoteCmdLine[4]) | (uint32(remoteCmdLine[5]) << 8) |
				(uint32(remoteCmdLine[6]) << 16) | (uint32(remoteCmdLine[7]) << 24)

			cmdLine := readProcessMemory(syscall.Handle(h), procIs32Bits, uint64(bufferAddress), cmdLineLength)
			if len(cmdLine) != int(cmdLineLength) {
				return "", errors.New("cannot read cmdline")
			}

			return convertUTF16ToString(cmdLine), nil
		}
	} else {
		buf := readProcessMemory(syscall.Handle(h), procIs32Bits, pebAddress+uint64(32), 8)
		if len(buf) != 8 {
			return "", errors.New("cannot locate process user parameters")
		}
		userProcParams := uint64(buf[0]) | (uint64(buf[1]) << 8) | (uint64(buf[2]) << 16) | (uint64(buf[3]) << 24) |
			(uint64(buf[4]) << 32) | (uint64(buf[5]) << 40) | (uint64(buf[6]) << 48) | (uint64(buf[7]) << 56)

		//read CommandLine field from PRTL_USER_PROCESS_PARAMETERS
		remoteCmdLine := readProcessMemory(syscall.Handle(h), procIs32Bits, userProcParams+uint64(112), 16)
		if len(remoteCmdLine) != 16 {
			return "", errors.New("cannot read cmdline field")
		}

		//remoteCmdLine is actually a UNICODE_STRING64
		//the first two bytes has the length
		cmdLineLength := uint(remoteCmdLine[0]) | (uint(remoteCmdLine[1]) << 8)
		if cmdLineLength > 0 {
			//and, at offset 8, is the pointer to the buffer
			bufferAddress := uint64(remoteCmdLine[8]) | (uint64(remoteCmdLine[9]) << 8) |
				(uint64(remoteCmdLine[10]) << 16) | (uint64(remoteCmdLine[11]) << 24) |
				(uint64(remoteCmdLine[12]) << 32) | (uint64(remoteCmdLine[13]) << 40) |
				(uint64(remoteCmdLine[14]) << 48) | (uint64(remoteCmdLine[15]) << 56)

			cmdLine := readProcessMemory(syscall.Handle(h), procIs32Bits, bufferAddress, cmdLineLength)
			if len(cmdLine) != int(cmdLineLength) {
				return "", errors.New("cannot read cmdline")
			}

			return convertUTF16ToString(cmdLine), nil
		}
	}

	//if we reach here, we have no command line
	return "", nil
}

func convertUTF16ToString(src []byte) string {
	srcLen := len(src) / 2

	codePoints := make([]uint16, srcLen)

	srcIdx := 0
	for i := 0; i < srcLen; i++ {
		codePoints[i] = uint16(src[srcIdx]) | uint16(src[srcIdx+1])<<8
		srcIdx += 2
	}
	return syscall.UTF16ToString(codePoints)
}
