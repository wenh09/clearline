//go:build !darwin

package main

import (
	"context"
	"errors"
)

func checkEffectiveDNS(context.Context) error { return errors.New("系统 DNS 验证仅支持 macOS") }
func checkScopedDNS(context.Context, []string) error {
	return errors.New("系统 DNS 验证仅支持 macOS")
}
