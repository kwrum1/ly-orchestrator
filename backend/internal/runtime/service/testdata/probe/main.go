// Command probe is the tiny TCP endpoint used by the independent netns
// harness. It can act as a normal echo server or as a TPROXY listener that
// proves a packet was diverted to the transparent socket.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

func main() {
	mode := flag.String("mode", "echo", "echo, tproxy, or client")
	listen := flag.String("listen", "0.0.0.0:0", "listen address")
	reply := flag.String("reply", "OK", "echo/tproxy reply prefix")
	address := flag.String("addr", "", "client destination")
	attempts := flag.Int("attempts", 1, "client attempts")
	flag.Parse()

	switch *mode {
	case "echo":
		serve(*listen, *reply, false)
	case "tproxy":
		serve(*listen, *reply, true)
	case "client":
		client(*address, *attempts)
	default:
		fail(fmt.Errorf("unknown mode %q", *mode))
	}
}

func serve(listen, reply string, transparent bool) {
	config := net.ListenConfig{Control: transparentControl(transparent)}
	listener, err := config.Listen(nil, "tcp4", listen)
	if err != nil {
		fail(err)
	}
	fmt.Fprintln(os.Stderr, "listening", listener.Addr().String())
	for {
		connection, err := listener.Accept()
		if err != nil {
			fail(err)
		}
		go func(connection net.Conn) {
			defer connection.Close()
			_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
			buffer := make([]byte, 64)
			_, _ = connection.Read(buffer)
			local := connection.LocalAddr().String()
			_, _ = fmt.Fprintf(connection, "%s %s\n", reply, local)
		}(connection)
	}
}

func transparentControl(enabled bool) func(string, string, syscall.RawConn) error {
	if !enabled {
		return nil
	}
	return func(_, _ string, raw syscall.RawConn) error {
		var controlErr error
		err := raw.Control(func(fd uintptr) {
			// IP_TRANSPARENT is 19 on Linux; it lets the socket accept a
			// connection whose original destination is not local.
			controlErr = syscall.SetsockoptInt(int(fd), syscall.SOL_IP, 19, 1)
		})
		if err != nil {
			return err
		}
		return controlErr
	}
}

func client(address string, attempts int) {
	succeeded := false
	for attempt := 0; attempt < attempts; attempt++ {
		connection, err := net.DialTimeout("tcp", address, 3*time.Second)
		if err != nil {
			fmt.Printf("ERROR %v\n", err)
			continue
		}
		_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _ = connection.Write([]byte("ping\n"))
		payload, err := io.ReadAll(io.LimitReader(connection, 256))
		_ = connection.Close()
		if err != nil {
			fmt.Printf("ERROR %v\n", err)
			continue
		}
		line := strings.TrimSpace(string(payload))
		fmt.Println(line)
		if strings.HasPrefix(line, "PROXIED") || strings.HasPrefix(line, "DIRECT") {
			succeeded = true
		}
	}
	if !succeeded {
		os.Exit(2)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
