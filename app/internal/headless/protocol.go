package headless

import (
	"context"
	"io"
	"net"
)

// Relay bytes without introducing a hello, parsing replies, or resending a
// request. Native socket EOF ends passive waits, so duplex clients should keep
// stdin open until the last reply they need has arrived.
func relay(ctx context.Context, path string, in io.Reader, out io.Writer) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	unix := conn.(*net.UnixConn)
	upload := make(chan error, 1)
	go func() {
		_, err := io.Copy(unix, in)
		if err == nil {
			err = unix.CloseWrite()
		}
		upload <- err
		if err != nil {
			_ = unix.Close()
		}
	}()
	_, err = io.Copy(out, unix)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	select {
	case writeErr := <-upload:
		if writeErr != nil {
			return writeErr
		}
	default:
		// The peer may close before local stdin. This CLI exits without
		// waiting for another input byte.
	}
	return err
}
