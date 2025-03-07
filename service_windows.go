//go:build windows

package main

import (
	"fmt"
	"github.com/emersion/go-smtp"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
	"io"
	"os"
	"time"
)

// eventLogWriter is a custom writer that sends log messages to the Windows Event Log.
type eventLogWriter struct {
	evLog *eventlog.Log
}

// Write implements the io.Writer interface.
func (w *eventLogWriter) Write(p []byte) (n int, err error) {
	if w.evLog != nil {
		// Write the log message as an Info event.
		if err = w.evLog.Info(1, string(p)); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// initEventLog opens the event log for the service.
func initEventLog() (*eventlog.Log, error) {
	// config.ServiceName should already be set.
	return eventlog.Open(config.ServiceName)
}

// overrideLoggerForService reconfigures the global logger to include Windows Event Log output.
func overrideLoggerForService() {
	// Open the event log.
	evLog, err := initEventLog()
	if err != nil {
		logger.Printf("Failed to initialize event log: %v", err)
		return
	}

	// Create a new multi-writer that writes to both the existing logger output and the event log.
	// Note: This example assumes that logger was set up in main.go to write to a file (and optionally stdout).
	// Here, we add the eventLogWriter.
	newWriter := io.MultiWriter(logger.Writer(), &eventLogWriter{evLog: evLog})
	logger.SetOutput(newWriter)
	logger.Println("Logger overridden to include Windows Event Log output.")
}

func runAppWithStop(stopCh chan struct{}) error {
	be := &Backend{}
	server := smtp.NewServer(be)
	server.Addr = fmt.Sprintf("%s:%s", config.Host, config.Port)
	server.AllowInsecureAuth = true

	errCh := make(chan error, 1)

	// Start the SMTP server in a goroutine.
	go func() {
		logger.Printf("Starting the SMTP server on %s...", server.Addr)
		errCh <- server.ListenAndServe() // Blocks until an error occurs or stop signal is received.
	}()

	logger.Println("SMTP server is starting...")

	// Wait for either the stop signal or a server error.
	select {
	case <-stopCh:
		logger.Println("Stop signal received. Shutting down server...")
		return server.Close() // Gracefully stop the server.
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

	logger.Println("ServiceHandler.Execute: Setting StartPending...")
	s <- svc.Status{State: svc.StartPending}

	// Simulating a delay during initialization
	logger.Println("ServiceHandler.Execute: Simulating initialization...")
	time.Sleep(2 * time.Second)

	// Run the application in a goroutine so that startup isn’t blocked.
	started := make(chan error)
	go func() {
		err := runAppWithStop(h.stopCh)
		started <- err
	}()

	// Wait briefly for the application to start.
	select {
	case err := <-started:
		if err != nil {
			logger.Printf("runAppWithStop failed to start: %v", err)
			return false, 1 // Report error to the Service Control Manager (SCM).
		}
	case <-time.After(500 * time.Millisecond):
		// After a short wait, assume the app started successfully.
		logger.Println("Assuming runAppWithStop is running after timeout.")
	default:
		// Application started successfully; continue service lifecycle
		logger.Println("runAppWithStop running successfully...")
	}

	// Notify the SCM that the service is now running
	logger.Println("ServiceHandler.Execute: Marking service as Running...")
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

	// Override the logger to include Windows Event Log output.
	overrideLoggerForService()

	handler := &ServiceHandler{
		stopCh: make(chan struct{}),
	}
	err := svc.Run(config.ServiceName, handler)
	if err != nil {
		logger.Printf("svc.Run failed: %v", err)
	} else {
		logger.Println("Service exited normally.")
	}
	return err
}

func isWindowsService() bool {
	isService, err := svc.IsWindowsService()
	if err != nil {
		logger.Printf("Failed to determine if running as a Windows service: %v. Falling back...", err)
		// Fallback: if SESSIONNAME isn’t set, assume service environment.
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
	exePath, err := os.Executable()
	if err != nil {
		logger.Fatalf("Failed to find executable path: %v", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		logger.Fatalf("Failed to connect to service manager: %v", err)
	}
	defer func() {
		if err := m.Disconnect(); err != nil {
			logger.Printf("Warning: Failed to disconnect from service manager: %v", err)
		}
	}()

	// Check if the service already exists.
	s, err := m.OpenService(serviceName)
	if err == nil {
		if err := s.Close(); err != nil {
			logger.Printf("Warning: Failed to close existing service handle: %v", err)
		}
		logger.Fatalf("Service %s already exists", serviceName)
	}

	// Create the new service.
	s, err = m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName: displayName,
		Description: description,
		StartType:   mgr.StartAutomatic,
	})
	if err != nil {
		logger.Fatalf("Failed to create service: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			logger.Printf("Warning: Failed to close service handle: %v", err)
		}
	}()

	// Configure event logging for the new service.
	err = eventlog.InstallAsEventCreate(serviceName, eventlog.Info|eventlog.Warning|eventlog.Error)
	if err != nil {
		if delErr := s.Delete(); delErr != nil {
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
