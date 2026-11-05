//go:build darwin

package main

import (
	"bytes"
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	procInfoCallPIDInfo = 2
	procPIDPathInfo     = 11
)

func getPeerPID(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("不是 Unix 连接")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var peerPID int
	var sysErr error
	err = raw.Control(func(fd uintptr) {
		peerPID, sysErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	})
	if err != nil {
		return 0, err
	}
	return peerPID, sysErr
}

func getProcessPPID(pid int) (int, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	return int(kp.Eproc.Ppid), nil
}

func getProcessPath(pid int) (string, error) {
	buf := make([]byte, 4096)
	_, _, err := syscall.Syscall6(
		unix.SYS_PROC_INFO,
		procInfoCallPIDInfo,
		uintptr(pid),
		procPIDPathInfo,
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if err != 0 {
		return "", err
	}
	n := bytes.IndexByte(buf, 0)
	if n <= 0 {
		return "", fmt.Errorf("proc_pidpath 返回空路径")
	}
	return string(buf[:n]), nil
}
