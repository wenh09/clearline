package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type recoveryTunnel struct{ closed atomic.Int32 }

func (t *recoveryTunnel) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("fixture has no destination")
}
func (t *recoveryTunnel) Close() { t.closed.Add(1) }

func TestFallbackOnlyUsesSameServerAndDifferentProtocol(t *testing.T) {
	raw, _ := awgFixture(t, true)
	primary := Profile{ID: "hy", Kind: "hysteria2", Config: "hy2://pass@127.0.0.1:443"}
	backup := Profile{ID: "wg", Kind: "awg", Config: raw}
	other := backup
	other.Config = strings.Replace(raw, "Endpoint = 127.0.0.1:", "Endpoint = 127.0.0.2:", 1)
	sameKind := primary
	sameKind.ID = "hy-duplicate"
	profiles, err := connectionCandidates(primary, []Profile{other, sameKind, backup}, "wg")
	if err != nil || len(profiles) != 2 || profiles[0].ID != "wg" || profiles[1].ID != "hy" {
		t.Fatal("same-server preference lost", profiles, err)
	}
	profiles, err = connectionCandidates(primary, []Profile{other, sameKind}, "")
	if err != nil || len(profiles) != 1 {
		t.Fatal("unrelated server or duplicate protocol was used", err)
	}
}

func TestHangingPrimaryFallsBackToRealAmneziaWG(t *testing.T) {
	raw, target := awgFixture(t, true)
	// A bound UDP socket deliberately discards every primary handshake packet.
	blackhole, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	profiles := []Profile{{ID: "primary", Kind: "hysteria2", Config: "hy2://unused@" + blackhole.LocalAddr().String()}, {ID: "backup", Kind: "awg", Config: raw}}
	check := func(ctx context.Context, tunnel Tunnel) (int64, error) {
		tr := &http.Transport{DialContext: tunnel.DialContext}
		defer tr.CloseIdleConnections()
		req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
		resp, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "encrypted-through-awg" {
			return 0, errors.New("wrong transport response")
		}
		return 1, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := (connector{open: OpenTunnel, check: check, budget: 3 * time.Second, stagger: 20 * time.Millisecond}).connect(ctx, profiles, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer result.tunnel.Close()
	if result.profile.ID != "backup" {
		t.Fatal("did not use the checked backup")
	}
	// Cancelling losing attempts must leave the winner usable.
	if _, err := check(context.Background(), result.tunnel); err != nil {
		t.Fatal("winner was closed", err)
	}
}

func TestFallbackClosesLateLosingTunnel(t *testing.T) {
	release := make(chan struct{})
	loser, winner := &recoveryTunnel{}, &recoveryTunnel{}
	open := func(ctx context.Context, p Profile) (Tunnel, error) {
		if p.ID == "slow" {
			<-release
			return loser, nil
		}
		return winner, nil
	}
	check := func(context.Context, Tunnel) (int64, error) { return 1, nil }
	result, err := (connector{open: open, check: check, budget: time.Second, stagger: time.Millisecond}).connect(context.Background(), []Profile{{ID: "slow"}, {ID: "fast"}}, nil, nil)
	close(release)
	if err != nil || result.tunnel != winner {
		t.Fatal("backup failed", err)
	}
	defer winner.Close()
	deadline := time.Now().Add(time.Second)
	for loser.closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if loser.closed.Load() != 1 || winner.closed.Load() != 0 {
		t.Fatal("incorrect loser/winner cleanup")
	}
}

func TestCancelledRecoveryStopsBothAttempts(t *testing.T) {
	started := make(chan struct{}, 2)
	ended := make(chan struct{}, 2)
	open := func(ctx context.Context, p Profile) (Tunnel, error) {
		started <- struct{}{}
		<-ctx.Done()
		ended <- struct{}{}
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (connector{open: open, check: probe, budget: time.Second, stagger: time.Millisecond}).connect(ctx, []Profile{{ID: "one"}, {ID: "two"}}, nil, nil)
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("attempt did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel stalled")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-ended:
		case <-time.After(time.Second):
			t.Fatal("attempt leaked after cancel")
		}
	}
}

func TestConfigurationFailuresStopRetryButNetworkFailuresRecover(t *testing.T) {
	for _, transient := range []bool{false, true} {
		open := func(context.Context, Profile) (Tunnel, error) {
			return nil, connectionError{message: "safe failure", retryable: transient, cause: errors.New("private upstream detail")}
		}
		_, err := (connector{open: open, check: probe, budget: time.Second, stagger: time.Second}).connect(context.Background(), []Profile{{ID: "one"}}, nil, nil)
		if err == nil || retryableConnectionError(err) != transient || strings.Contains(err.Error(), "private upstream detail") {
			t.Fatal("unsafe recovery classification", err)
		}
	}
	_, err := connectionCandidates(Profile{Kind: "hysteria2", Config: "invalid"}, nil, "")
	if err == nil || retryableConnectionError(err) {
		t.Fatal("malformed profile would retry forever")
	}
}
