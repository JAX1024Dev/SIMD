package serial

import "golang.org/x/sys/unix"

// Darwin support is provided for development/probing; Linux is the deployment target.
func configure(fd, baud int, flow bool) error {
	t, e := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if e != nil {
		return e
	}
	t.Iflag = 0
	t.Oflag = 0
	t.Lflag = 0
	t.Cflag = unix.CS8 | unix.CREAD | unix.CLOCAL
	if flow {
		t.Cflag |= unix.CRTSCTS
	}
	t.Ispeed = uint64(baud)
	t.Ospeed = uint64(baud)
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(fd, unix.TIOCSETA, t)
}
