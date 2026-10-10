//go:build windows

package browser

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// group is a job object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: when the last handle to it
// closes, even because m365crawl was killed, Windows ends every process in it.
type group struct {
	mu  sync.Mutex
	job windows.Handle
}

var completionDuplicateHandle = windows.DuplicateHandle

// prepareCmd starts the child suspended, so it cannot start a process of its own before it is
// inside the job.
func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP}
}

// adopt creates the job, puts the suspended child in it, then resumes the child's main thread.
func adopt(cmd *exec.Cmd) (*group, error) {
	job, err := windows.CreateJobObject(nil, nil) // nil attributes: the handle is not inheritable
	if err != nil {
		return nil, err
	}
	g := &group{job: job}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		g.release()
		return nil, err
	}
	pid := uint32(cmd.Process.Pid)
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		g.release()
		return nil, err
	}
	defer windows.CloseHandle(proc) //nolint:errcheck // nothing to do about a failed close
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		g.release()
		return nil, err
	}
	if err := resumeMainThread(pid); err != nil {
		g.release()
		return nil, err
	}
	return g, nil
}

// resumeMainThread finds the only thread of a suspended process and resumes it.
func resumeMainThread(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap) //nolint:errcheck // nothing to do about a failed close
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snap, &entry); err != nil {
		return err
	}
	for {
		if entry.OwnerProcessID == pid {
			th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return err
			}
			defer windows.CloseHandle(th) //nolint:errcheck // nothing to do about a failed close
			_, err = windows.ResumeThread(th)
			return err
		}
		if err := windows.Thread32Next(snap, &entry); err != nil {
			return err
		}
	}
}

// processStart is the process's creation time, the identity that survives pid reuse.
func processStart(cmd *exec.Cmd) int64 {
	return creationTime(uint32(cmd.Process.Pid))
}

func creationTime(pid uint32) int64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h) //nolint:errcheck // nothing to do about a failed close
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return 0
	}
	return created.Nanoseconds()
}

// jobBasicAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, which x/sys does not define.
type jobBasicAccounting struct {
	TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
	TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
}

const jobObjectBasicAccountingInformation = 1

func (g *group) alive() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return false
	}
	var info jobBasicAccounting
	err := windows.QueryInformationJobObject(g.job, jobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	return err == nil && info.ActiveProcesses > 0
}

// term and kill are the same on Windows: there is no polite signal for a whole job.
func (g *group) term() { g.kill() }
func (g *group) kill() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job != 0 {
		_ = windows.TerminateJobObject(g.job, 1)
	}
}

func (g *group) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job != 0 {
		_ = windows.CloseHandle(g.job)
		g.job = 0
	}
}

func (g *group) duplicateJob() (windows.Handle, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return 0, &completionFailure{code: "browser_completion_job_unavailable"}
	}
	var duplicate windows.Handle
	err := completionDuplicateHandle(windows.CurrentProcess(), g.job, windows.CurrentProcess(), &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS)
	if err != nil {
		return 0, &completionFailure{code: "browser_completion_job_duplicate_failed", cause: err}
	}
	return duplicate, nil
}
