//go:build !windows

package main

import (
	"fmt"
	"github.com/emersion/go-smtp"
)

func isWindowsService() bool {
	// Always return false since this is a Linux build
	return false
}

func installService(serviceName, displayName, description string) {
	panic("installService is not supported on this platform. This feature is available only on Windows.")
}

func removeService(serviceName string) {
	panic("removeService is not supported on this platform. This feature is available only on Windows.")
}

func runApp() error {
	be := &Backend{}
	server := smtp.NewServer(be)
	server.Addr = fmt.Sprintf("%s:%s", config.Host, config.Port)
	server.AllowInsecureAuth = true
	logger.Printf("Starting SMTP server on %s...", server.Addr)
	return server.ListenAndServe()
}

func runWindowsService() error {
	logger.Println("Windows service functionality is not available on this platform.")
	return fmt.Errorf("unsupported on Linux or non-Windows platforms")
}
