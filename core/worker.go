package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type workerEvent struct {
	worker *transportWorker
	value  map[string]any
	exited bool
}
type transportWorker struct {
	cmd       *exec.Cmd
	input     io.WriteCloser
	exited    chan struct{}
	stopTimer *time.Timer
	lastError error
	ready     bool
	token     string
}

func transportCommand() (*exec.Cmd, error) {
	path, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(path, "transport"), nil
}
func startTransportWorker(ctx context.Context, r Request, factory func() (*exec.Cmd, error), events chan<- workerEvent) (*transportWorker, error) {
	cmd, err := factory()
	if err != nil {
		return nil, err
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, err
	}
	cmd.Stderr = io.Discard // Upstream output can contain keys or destinations.
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		input.Close()
		output.Close()
		return nil, err
	}
	w := &transportWorker{cmd: cmd, input: input, exited: make(chan struct{}), token: hex.EncodeToString(secret)}
	if err := cmd.Start(); err != nil {
		input.Close()
		output.Close()
		return nil, err
	}
	go func() {
		defer close(w.exited)
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 8192), 2*maxInput)
		for scanner.Scan() {
			var value map[string]any
			if json.Unmarshal(scanner.Bytes(), &value) == nil {
				select {
				case events <- workerEvent{worker: w, value: value}:
				case <-ctx.Done():
				}
			}
		}
		_ = cmd.Wait() // Exit is delivered after the last stdout event.
		select {
		case events <- workerEvent{worker: w, exited: true}:
		case <-ctx.Done():
		}
	}()
	// Credentials are data on a private pipe, never argv, env, or a disk file.
	r.InternalToken = w.token
	r.RoutingMode = "all"
	r.Bypass = nil
	r.ProxyDomains = nil
	r.AuditRoutes = false
	if err := json.NewEncoder(input).Encode(r); err != nil {
		w.stop()
		return w, err
	}
	return w, nil
}
func (w *transportWorker) send(command ControlRequest) error {
	return json.NewEncoder(w.input).Encode(command)
}
func (w *transportWorker) stop() {
	if w.stopTimer != nil {
		return
	}
	_ = w.input.Close()
	_ = w.cmd.Process.Signal(syscall.SIGTERM)
	w.stopTimer = time.AfterFunc(3*time.Second, func() { _ = w.cmd.Process.Kill() })
}
func (w *transportWorker) finish() {
	if w.stopTimer != nil {
		w.stopTimer.Stop()
	}
	_ = w.input.Close()
}
func (w *transportWorker) shutdown() {
	w.stop()
	select {
	case <-w.exited:
	case <-time.After(4 * time.Second):
		_ = w.cmd.Process.Kill()
		<-w.exited
	}
	w.finish()
}
func workerFailure(event map[string]any) error {
	message, _ := event["message"].(string)
	if message == "" {
		message = "传输连接暂时中断"
	}
	retryable, ok := event["retryable"].(bool)
	return connectionError{message: message, retryable: !ok || retryable, cause: errors.New("transport worker failed")}
}
