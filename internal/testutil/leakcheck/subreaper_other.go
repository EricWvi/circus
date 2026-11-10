//go:build !linux

package leakcheck

import "errors"

func setChildSubreaper() error { return errors.New("child subreapers require Linux") }
