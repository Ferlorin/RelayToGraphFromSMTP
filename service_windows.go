//go:build windows

package main

import (
	"fmt"
	"github.com/emersion/go-smtp"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
	"os"
	"time"
)

func runAppWithStop(stopCh chan struct{}) error {
	be := &Backend{}
	server := smtp.NewServer(be)
	server.Addr = fmt.Sprintf("%s:%s", config.Host, config.Port)
	server.AllowInsecureAuth = true

	errCh := make(chan error, 1)

	// Start the server in a goroutine
	go func() {
		logger.Printf("Starting the SMTP server on %s...", server.Addr)
		errCh <- server.ListenAndServe() // Blocks until error or stop
	}()

	logger.Println("SMTP server is starting...")

	// Wait for either stop signal or server error
	select {
	case <-stopCh:
		logger.Println("Stop signal received. Shutting down server...")
		return server.Close() // Gracefully stop the server
	case err := <-errCh:
		if err != nil {
			logger.Printf("SMTP server encountered an error: %v", err)
			return err
		}
	}

	return nil
}

type ServiceHandler struct {
	stopCh chan struct{}
}

func (h *ServiceHandler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	const acceptedCommands = svc.AcceptStop | svc.AcceptShutdown
	s <- svc.Status{State: svc.StartPending}
	time.Sleep(2 * time.Second) // Simulate initialization

	// Run the application in a goroutine so startup isn't blocked
	started := make(chan error)
	go func() {
		err := runAppWithStop(h.stopCh)
		started <- err
	}()

	select {
	case err := <-started:
		// If runAppWithStop failed during startup, notify SCM
		if err != nil {
			logger.Printf("runAppWithStop failed to start: %v", err)
			return false, 1 // Report error to SCM
		}
	default:
		// Application started successfully; continue service lifecycle
		logger.Println("runAppWithStop running successfully...")
	}

	// Notify the SCM that the service is now running
	s <- svc.Status{State: svc.Running, Accepts: acceptedCommands}

	// Handle SCM lifecycle commands like Stop, Shutdown, etc.
	for {
		select {
		case cmd := <-r:
			logger.Printf("ServiceHandler.Execute: Received command: %v", cmd.Cmd)
			switch cmd.Cmd {
			case svc.Interrogate:
				s <- cmd.CurrentStatus
				logger.Println("ServiceHandler.Execute: Interrogation completed")
			case svc.Stop, svc.Shutdown:
				logger.Println("ServiceHandler.Execute: Stop or shutdown requested, stopping app...")
				// Signal the app to stop
				close(h.stopCh)
				s <- svc.Status{State: svc.StopPending}
				return false, 0
			default:
				logger.Printf("Unexpected command received: %v", cmd.Cmd)
			}
		}
	}
}

func runWindowsService() error {
	if config.ServiceName == "" {
		logger.Fatalf("ServiceName is not set in the configuration file.")
	}

	logger.Printf("Running svc.Run for service: %s", config.ServiceName)
	return svc.Run(config.ServiceName, &ServiceHandler{})
}

func isWindowsService() bool {
	// Ideally this is a standard-library or trusted implementation
	isService, err := svc.IsWindowsService()

	if err != nil {
		logger.Printf("Failed to determine if running as a Windows service: %v. Falling back...", err)

		// Fallback approach: Check if SESSIONNAME environment variable is unset
		if _, exists := os.LookupEnv("SESSIONNAME"); !exists {
			logger.Println("Fallback mechanism detected service environment.")
			return true
		}

		logger.Println("Fallback mechanism did not detect Windows service environment.")
		return false
	}

	logger.Printf("isWindowsService: isService=%v", isService)
	return isService
}

func installService(serviceName, displayName, description string) {
	exePath, err := os.Executable() // Path to the current executable
	if err != nil {
		logger.Fatalf("Failed to find executable path: %v", err)
	}

	m, err := mgr.Connect() // Connect to Windows Service Manager
	if err != nil {
		logger.Fatalf("Failed to connect to service manager: %v", err)
	}
	defer func() {
		if err := m.Disconnect(); err != nil {
			// Log any errors during disconnect
			logger.Printf("Warning: Failed to disconnect from service manager: %v", err)
		}
	}()

	// Check if the service already exists
	s, err := m.OpenService(serviceName)
	if err == nil { // Service exists, clean up and exit
		if err := s.Close(); err != nil {
			logger.Printf("Warning: Failed to close existing service handle: %v", err)
		}
		logger.Fatalf("Service %s already exists", serviceName)
	}

	// Create the new service
	s, err = m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName: displayName,
		Description: description,
		StartType:   mgr.StartAutomatic, // Configure to auto-start on boot
	})
	if err != nil {
		logger.Fatalf("Failed to create service: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			// Log any errors in closing the service handler
			logger.Printf("Warning: Failed to close service handle: %v", err)
		}
	}()

	// Configure event logging for the new service
	err = eventlog.InstallAsEventCreate(serviceName, eventlog.Info|eventlog.Warning|eventlog.Error)
	if err != nil {
		// Roll back service creation if event log installation fails
		if delErr := s.Delete(); delErr != nil {
			// Log error in service deletion during rollback
			logger.Printf("Failed to delete service during rollback: %v", delErr)
		}
		logger.Fatalf("Failed to configure event log: %v", err)
	}

	logger.Printf("Service %s installed successfully", serviceName)
}

func removeService(serviceName string) {
	m, err := mgr.Connect()
	if err != nil {
		logger.Fatalf("Failed to connect to service manager: %v", err)
	}
	defer func() {
		if err := m.Disconnect(); err != nil {
			logger.Printf("Warning: Failed to disconnect from service manager: %v", err)
		}
	}()

	s, err := m.OpenService(serviceName)
	if err != nil {
		logger.Fatalf("Service does not exist: %s", serviceName)
	}
	defer func() {
		if err := s.Close(); err != nil {
			logger.Printf("Warning: Failed to close service handle: %v", err)
		}
	}()

	err = s.Delete()
	if err != nil {
		logger.Fatalf("Failed to remove service: %v", err)
	}

	err = eventlog.Remove(serviceName)
	if err != nil {
		logger.Fatalf("Failed to remove event log: %v", err)
	}

	logger.Printf("Service %s removed successfully", serviceName)
}

func runApp() error {
	be := &Backend{}
	server := smtp.NewServer(be)
	server.Addr = fmt.Sprintf("%s:%s", config.Host, config.Port)
	server.AllowInsecureAuth = true
	logger.Printf("Starting SMTP server on %s...", server.Addr)
	return server.ListenAndServe()
}
