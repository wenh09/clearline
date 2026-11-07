package main

import (
	"context"
	"errors"
	"os/exec"
)

func checkEffectiveDNS(ctx context.Context) error {
	output, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--dns").Output()
	if err != nil || !effectiveDNSOwned(string(output)) {
		return errors.New("全局验证：其他系统 DNS 仍在生效")
	}
	return nil
}
