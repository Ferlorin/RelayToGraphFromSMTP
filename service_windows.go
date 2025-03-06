//go:build windows

package main

import (
	"fmt"
	"github.com/emersion/go-smtp"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
	"log"
	"os"
)

type ServiceHandler struct {
	stopCh chan struct{}
}

func (h *ServiceHandler) Execute(args []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	const acceptedCommands = svc.AcceptStop | svc.AcceptShutdown
	s <- svc.Status{State: svc.StartPending}

	h.stopCh = make(chan struct{})
	go func() {
		if err := runApp(); err != nil {
			logger.Printf("Application error: %v", err)
		}
	}()

	s <- svc.Status{State: svc.Running, Accepts: acceptedCommands}

	for {
		select {
		case cmd := <-r:
			if cmd.Cmd == svc.Stop || cmd.Cmd == svc.Shutdown {
				close(h.stopCh)
				s <- svc.Status{State: svc.StopPending}
				return false, 0
			}
		case <-h.stopCh:
			return false, 0
		}
	}
}

func runWindowsService() error {
	return svc.Run("MyGoSMTPService", &ServiceHandler{})
}

func isWindowsService() bool {
	isService, err := svc.IsWindowsService()
	if err != nil {
		log.Printf("Failed to determine if running as a Windows service: %v", err)
		return false
	}
	return isService
}

func installService(serviceName, displayName, description string) {
	exePath, err := os.Executable() // Path to the current executable
	if err != nil {
		log.Fatalf("Failed to find executable path: %v", err)
	}

	m, err := mgr.Connect() // Connect to Windows Service Manager
	if err != nil {
		log.Fatalf("Failed to connect to service manager: %v", err)
	}
	defer func() {
		if err := m.Disconnect(); err != nil {
			// Log any errors during disconnect
			log.Printf("Warning: Failed to disconnect from service manager: %v", err)
		}
	}()

	// Check if the service already exists
	s, err := m.OpenService(serviceName)
	if err == nil { // Service exists, clean up and exit
		if err := s.Close(); err != nil {
			log.Printf("Warning: Failed to close existing service handle: %v", err)
		}
		log.Fatalf("Service %s already exists", serviceName)
	}

	// Create the new service
	s, err = m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName: displayName,
		Description: description,
		StartType:   mgr.StartAutomatic, // Configure to auto-start on boot
	})
	if err != nil {
		log.Fatalf("Failed to create service: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			// Log any errors in closing the service handler
			log.Printf("Warning: Failed to close service handle: %v", err)
		}
	}()

	// Configure event logging for the new service
	err = eventlog.InstallAsEventCreate(serviceName, eventlog.Info|eventlog.Warning|eventlog.Error)
	if err != nil {
		// Roll back service creation if event log installation fails
		if delErr := s.Delete(); delErr != nil {
			// Log error in service deletion during rollback
			log.Printf("Failed to delete service during rollback: %v", delErr)
		}
		log.Fatalf("Failed to configure event log: %v", err)
	}

	log.Printf("Service %s installed successfully", serviceName)
}

func removeService(serviceName string) {
	m, err := mgr.Connect()
	if err != nil {
		log.Fatalf("Failed to connect to service manager: %v", err)
	}
	defer func() {
		if err := m.Disconnect(); err != nil {
			log.Printf("Warning: Failed to disconnect from service manager: %v", err)
		}
	}()

	s, err := m.OpenService(serviceName)
	if err != nil {
		log.Fatalf("Service does not exist: %s", serviceName)
	}
	defer func() {
		if err := s.Close(); err != nil {
			log.Printf("Warning: Failed to close service handle: %v", err)
		}
	}()

	err = s.Delete()
	if err != nil {
		log.Fatalf("Failed to remove service: %v", err)
	}

	err = eventlog.Remove(serviceName)
	if err != nil {
		log.Fatalf("Failed to remove event log: %v", err)
	}

	log.Printf("Service %s removed successfully", serviceName)
}

func runApp() error {
	be := &Backend{}
	server := smtp.NewServer(be)
	server.Addr = fmt.Sprintf("%s:%s", config.Host, config.Port)
	server.AllowInsecureAuth = true
	logger.Printf("Starting SMTP server on %s...", server.Addr)
	return server.ListenAndServe()
}
