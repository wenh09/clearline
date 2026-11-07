package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const tunnelAgentSocket = "/var/run/org.clearline.tunnel.sock"

type darwinPacketDevice struct {
	file       *os.File
	readBuffer [65540]byte
}

func (d *darwinPacketDevice) Read(b []byte) (int, error) {
	tmp := d.readBuffer[:]
	n, e := d.file.Read(tmp)
	if n < 4 {
		return 0, e
	}
	return copy(b, tmp[4:n]), e
}
func (d *darwinPacketDevice) Write(b []byte) (int, error) {
	if len(b) < 1 {
		return 0, nil
	}
	frame := make([]byte, len(b)+4)
	family := uint32(unix.AF_INET)
	if b[0]>>4 == 6 {
		family = unix.AF_INET6
	}
	binary.BigEndian.PutUint32(frame, family)
	copy(frame[4:], b)
	n, e := d.file.Write(frame)
	if n < 4 {
		return 0, e
	}
	return n - 4, e
}
func (d *darwinPacketDevice) Close() error { return d.file.Close() }

type packetSession struct {
	conn     *net.UnixConn
	stack    *packetStack
	done     chan error
	once     sync.Once
	restored chan struct{}
}

func (s *packetSession) bytes() (int64, int64) { return s.stack.up.Load(), s.stack.down.Load() }
func (s *packetSession) deviceError() error    { return s.stack.deviceError() }

func rootAgentConnection(ctx context.Context) (*net.UnixConn, error) {
	c, e := (&net.Dialer{}).DialContext(ctx, "unix", tunnelAgentSocket)
	if e != nil {
		return nil, errors.New("系统隧道服务未运行，请先安装本版本的网络组件")
	}
	u := c.(*net.UnixConn)
	raw, e := u.SyscallConn()
	if e != nil {
		u.Close()
		return nil, e
	}
	valid := false
	raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		valid = e == nil && cred.Uid == 0
	})
	if !valid {
		u.Close()
		return nil, errors.New("系统隧道服务身份无效")
	}
	return u, nil
}
func startPacketSession(ctx context.Context, t Tunnel, endpoint string) (*packetSession, error) {
	return openPacketSession(ctx, t, endpoint, "enable")
}
func startPacketProbe(ctx context.Context, t Tunnel) (*packetSession, error) {
	return openPacketSession(ctx, t, "", "probe")
}
func openPacketSession(ctx context.Context, t Tunnel, endpoint, action string) (*packetSession, error) {
	c, e := rootAgentConnection(ctx)
	if e != nil {
		return nil, e
	}
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()
	c.SetDeadline(time.Now().Add(25 * time.Second))
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if e = json.NewEncoder(c).Encode(map[string]any{"action": action, "endpoint": endpoint}); e != nil {
		return nil, e
	}
	data, control := make([]byte, 16384), make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, e := c.ReadMsgUnix(data, control)
	if e != nil {
		return nil, e
	}
	for bytes.IndexByte(data[:n], '\n') < 0 {
		if n >= 1024 {
			return nil, errors.New("系统隧道响应过大")
		}
		more, err := c.Read(data[n:1024])
		if err != nil {
			return nil, err
		}
		n += more
	}
	var response struct {
		Event   string `json:"event"`
		Message string `json:"message"`
		Stage   string `json:"stage"`
	}
	if json.Unmarshal(data[:n], &response) != nil || response.Event != "device" {
		if response.Event == "error" && response.Message != "" {
			return nil, errors.New("系统隧道：" + response.Message + "（" + response.Stage + "）")
		}
		return nil, errors.New("无法创建系统隧道，请检查网络组件版本或其他 VPN")
	}
	msgs, e := unix.ParseSocketControlMessage(control[:oobn])
	if e != nil || len(msgs) != 1 {
		return nil, errors.New("系统隧道描述符无效")
	}
	fds, e := unix.ParseUnixRights(&msgs[0])
	if e != nil || len(fds) != 1 {
		for _, fd := range fds {
			unix.Close(fd)
		}
		return nil, errors.New("系统隧道描述符无效")
	}
	unix.CloseOnExec(fds[0])
	file := os.NewFile(uintptr(fds[0]), "clearline-utun")
	p, e := newPacketStack(ctx, &darwinPacketDevice{file: file}, t)
	if e != nil {
		file.Close()
		return nil, e
	}
	s := &packetSession{conn: c, stack: p, done: make(chan error, 1), restored: make(chan struct{})}
	if e = json.NewEncoder(c).Encode(map[string]string{"action": "ready"}); e != nil {
		p.Close()
		return nil, e
	}
	decoder := json.NewDecoder(c)
	if e = decoder.Decode(&response); e != nil || response.Event != "applied" {
		p.Close()
		if response.Event == "error" && response.Message != "" {
			return nil, errors.New("系统隧道：" + response.Message + "（" + response.Stage + "）")
		}
		return nil, errors.New("系统隧道或 DNS 接管失败，已请求恢复原网络")
	}
	c.SetDeadline(time.Time{})
	reportFailure := func(err error) {
		select {
		case s.done <- err:
		default:
		}
	}
	go func() {
		<-p.ctx.Done()
		if err := p.deviceError(); err != nil {
			reportFailure(err)
		}
	}()
	go func() {
		defer close(s.restored)
		for {
			if e := decoder.Decode(&response); e != nil {
				reportFailure(errors.New("系统隧道服务已停止，正在恢复网络"))
				return
			}
			if response.Event == "restored" {
				return
			}
			if response.Event == "error" {
				reportFailure(errors.New("系统隧道网络配置异常，请重新连接"))
				return
			}
		}
	}()
	ok = true
	return s, nil
}
func (s *packetSession) Close() {
	s.once.Do(func() {
		json.NewEncoder(s.conn).Encode(map[string]string{"action": "disable"})
		select {
		case <-s.restored:
		case <-time.After(12 * time.Second):
		}
		s.conn.Close()
		s.stack.Close()
	})
}
func recoverPacketTunnel(ctx context.Context) error {
	c, e := rootAgentConnection(ctx)
	if e != nil {
		return e
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	if e = json.NewEncoder(c).Encode(map[string]string{"action": "recover"}); e != nil {
		return e
	}
	var response struct{ Event string }
	if e = json.NewDecoder(c).Decode(&response); e != nil {
		return e
	}
	if response.Event != "restored" {
		return errors.New("系统隧道恢复未完成")
	}
	return nil
}
