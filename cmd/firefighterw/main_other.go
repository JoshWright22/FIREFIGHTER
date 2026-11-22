//go:build !windows

// Command firefighterw is the Windows logon launcher; elsewhere the service manager (systemd,
// launchd) starts the guard, so this does nothing.
package main

func main() {}
