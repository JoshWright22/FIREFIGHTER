//go:build !windows

package setup

const installerManagesAutostart = false

func removeStartupShortcut() error { return nil }
