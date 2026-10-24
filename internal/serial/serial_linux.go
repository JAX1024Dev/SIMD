package serial

import "golang.org/x/sys/unix"

func configure(fd, baud int, flow bool) error {
	t, e := unix.IoctlGetTermios(fd, unix.TCGETS)
	if e != nil {
		return e
	}
	speed := map[int]uint32{300: unix.B300, 1200: unix.B1200, 2400: unix.B2400, 4800: unix.B4800, 9600: unix.B9600, 19200: unix.B19200, 38400: unix.B38400, 57600: unix.B57600, 115200: unix.B115200, 230400: unix.B230400}[baud]
	t.Iflag = 0
	t.Oflag = 0
	t.Lflag = 0
	t.Cflag = unix.CS8 | unix.CREAD | unix.CLOCAL | speed
	if flow {
		t.Cflag |= unix.CRTSCTS
	}
	t.Ispeed = speed
	t.Ospeed = speed
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(fd, unix.TCSETS, t)
}
