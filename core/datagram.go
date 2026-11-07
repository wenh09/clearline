package main

// Datagram framing is used only on the authenticated, loopback worker pipe.
// Internet UDP is carried by Hysteria datagrams (or the AWG netstack), never
// by a direct local UDP socket. A dead worker cannot fall back to the ISP.
import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

const workerUDPCommand = 0xf0

type datagramStream struct {
	net.Conn
	readMu, writeMu sync.Mutex
}

func readDatagram(r io.Reader) ([]byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	b := make([]byte, int(binary.BigEndian.Uint16(h[:])))
	_, err := io.ReadFull(r, b)
	return b, err
}
func writeDatagram(w io.Writer, b []byte) error {
	if len(b) > 65535 {
		return errors.New("UDP 数据包过大")
	}
	frame := make([]byte, len(b)+2)
	binary.BigEndian.PutUint16(frame, uint16(len(b)))
	copy(frame[2:], b)
	for len(frame) > 0 {
		n, err := w.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}
func (c *datagramStream) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	packet, err := readDatagram(c.Conn)
	if err != nil {
		return 0, err
	}
	return copy(b, packet), nil
}
func (c *datagramStream) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := writeDatagram(c.Conn, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func dialWorkerUDP(ctx context.Context, address, token, target string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(target)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 || len(host) > 255 || token == "" {
		return nil, errors.New("UDP 目标无效")
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	if d, set := ctx.Deadline(); set && d.Before(deadline) {
		deadline = d
	}
	c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if _, err = c.Write([]byte{5, 1, 2}); err != nil {
		return nil, err
	}
	var reply [10]byte
	if _, err = io.ReadFull(c, reply[:2]); err != nil || reply[0] != 5 || reply[1] != 2 {
		return nil, errors.New("UDP 模块认证失败")
	}
	auth := append([]byte{1, 9}, []byte("clearline")...)
	auth = append(auth, byte(len(token)))
	auth = append(auth, token...)
	if _, err = c.Write(auth); err != nil {
		return nil, err
	}
	if _, err = io.ReadFull(c, reply[:2]); err != nil || reply[1] != 0 {
		return nil, errors.New("UDP 模块认证失败")
	}
	request := append([]byte{5, workerUDPCommand, 0, 3, byte(len(host))}, host...)
	request = append(request, byte(n>>8), byte(n))
	if _, err = c.Write(request); err != nil {
		return nil, err
	}
	if _, err = io.ReadFull(c, reply[:]); err != nil || reply[0] != 5 || reply[1] != 0 {
		return nil, errors.New("服务器不支持 UDP 转发")
	}
	c.SetDeadline(time.Time{})
	ok = true
	return &datagramStream{Conn: c}, nil
}

func relayDatagrams(a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{}, 2)
	copyPackets := func(dst, src net.Conn) {
		buffer := make([]byte, 65535)
		for {
			src.SetReadDeadline(time.Now().Add(90 * time.Second))
			n, err := src.Read(buffer)
			if err != nil {
				break
			}
			if _, err = dst.Write(buffer[:n]); err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go copyPackets(a, b)
	go copyPackets(b, a)
	<-done
}
