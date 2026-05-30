//go:build darwin

package cli

import "golang.org/x/sys/unix"

// procStart returns pid's start-time as microseconds (sec*1e6+usec), read via
// sysctl KERN_PROC/KERN_PROC_PID → kinfo_proc.kp_proc.p_starttime. The second
// return is false when the value can't be read (pid gone, sysctl error) —
// caller treats false as UNKNOWN and falls back to a plain pid-alive check.
// The encoding is opaque: it is only ever compared for equality against a
// value read on the same host (see domain.LockRecord.ProcStart).
//
// x/sys/unix field path verified on this platform: KinfoProc.Proc is an
// ExternProc whose P_starttime is a unix.Timeval{Sec int64, Usec int32}.
func procStart(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return 0, false
	}
	tv := kp.Proc.P_starttime
	return tv.Sec*1_000_000 + int64(tv.Usec), true
}
