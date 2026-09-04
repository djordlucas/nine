package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"nine/internal/config"
	"nine/internal/protocol"
)

// APIServe starts the API server process.
func (c *CLI) APIServe(cfg *config.Config, flags []string) error {
	// Build the command to start the API server
	args := []string{"api", "serve"}
	args = append(args, flags...)

	// Start the API server as a subprocess
	cmd := exec.Command(os.Args[0], args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start API server: %w", err)
	}

	// Give it a moment to start
	time.Sleep(500 * time.Millisecond)

	// Check if it's running
	if cmd.Process == nil {
		return fmt.Errorf("API server process is nil")
	}

	// Verify the API is responding
	if err := checkAPIHealth(cfg); err != nil {
		// Clean up if it failed
		cmd.Process.Kill() //nolint:errcheck
		return fmt.Errorf("API server started but is not healthy: %w", err)
	}

	fmt.Fprintf(c.Out, "API server started successfully\n")
	fmt.Fprintf(c.Out, "Connect to: http://%s:%d\n", cfg.API.Host(), cfg.API.Port())

	// Note: The process runs independently; use `nine api stop` to stop it
	return nil
}

// APIStatus returns the API server status.
func (c *CLI) APIStatus(cfg *config.Config) error {
	if err := checkAPIHealth(cfg); err != nil {
		fmt.Fprintf(c.Out, "API server is not running: %v\n", err)
		return nil
	}

	fmt.Fprintf(c.Out, "API server is running\n")
	fmt.Fprintf(c.Out, "Host: %s\n", cfg.API.Host())
	fmt.Fprintf(c.Out, "Port: %d\n", cfg.API.Port())
	fmt.Fprintf(c.Out, "Health: healthy\n")
	return nil
}

// APIStop stops the API server process.
func (c *CLI) APIStop(cfg *config.Config) error {
	// Try to connect to the API and trigger a graceful shutdown
	// For now, we'll just report that the user should kill the process manually
	// In a full implementation, we would:
	// 1. Connect to the API management endpoint
	// 2. Send a shutdown request
	// 3. Wait for it to stop

	fmt.Fprintf(c.Out, "To stop the API server, you need to:\n")
	fmt.Fprintf(c.Out, "1. Find the API server process: ps aux | grep 'nine api serve'\n")
	fmt.Fprintf(c.Out, "2. Kill it: kill <PID>\n")
	fmt.Fprintf(c.Out, "\nNote: In a future version, this will be automated.\n")
	return nil
}

// checkAPIHealth checks if the API server is running and healthy.
func checkAPIHealth(cfg *config.Config) error {
	// Try to connect to the health endpoint
	url := fmt.Sprintf("http://%s:%d/api/v1/health", cfg.API.Host(), cfg.API.Port())

	client := &http.Client{
		Timeout: 2 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("could not connect to API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API returned status: %d", resp.StatusCode)
	}

	var health struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return fmt.Errorf("could not parse health response: %w", err)
	}

	if health.Status != "healthy" && health.Status != "degraded" {
		return fmt.Errorf("API status is: %s", health.Status)
	}

	return nil
}

// APIProcessInfo holds information about a running API process.
type APIProcessInfo struct {
	PID       int
	StartedAt time.Time
	Port      int
	Host      string
}

// FindAPIProcess finds any running API server processes.
func FindAPIProcess(cfg *config.Config) (*APIProcessInfo, error) {
	// This is a placeholder for a more sophisticated implementation
	// that would find and manage API processes
	return nil, fmt.Errorf("API process management not yet implemented")
}

// StartAPIProcess starts the API server as a background process.
func StartAPIProcess(cfg *config.Config) (*os.Process, error) {
	args := []string{"api", "serve"}

	// Add flags from config
	if cfg.API.Port() != config.DefaultAPIPort {
		args = append(args, "--port", fmt.Sprintf("%d", cfg.API.Port()))
	}
	if cfg.API.Host() != config.DefaultAPIHost {
		args = append(args, "--host", cfg.API.Host())
	}
	if cfg.API.AuthToken != "" {
		args = append(args, "--auth-token", cfg.API.AuthToken)
	}

	cmd := exec.Command(os.Args[0], args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start API: %w", err)
	}

	return cmd.Process, nil
}

// StopAPIProcess stops a running API process.
func StopAPIProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}

// APIClient provides methods for interacting with the API server.
type APIClient struct {
	BaseURL    string
	AuthToken  string
	HTTPClient *http.Client
}

// NewAPIClient creates a new API client.
func NewAPIClient(cfg *config.Config) *APIClient {
	return &APIClient{
		BaseURL:   fmt.Sprintf("http://%s:%d/api/v1", cfg.API.Host(), cfg.API.Port()),
		AuthToken: cfg.API.AuthToken,
		HTTPClient: &http.Client{
			Timeout: time.Duration(cfg.API.TimeoutSeconds()) * time.Second,
		},
	}
}

// doRequest makes an HTTP request to the API.
func (a *APIClient) doRequest(method, path string, body any) ([]byte, error) {
	url := a.BaseURL + path

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = strings.NewReader(string(data))
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}

	if a.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.AuthToken)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return nil, fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
		}
		return nil, fmt.Errorf("API error: %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// Status returns the API server status.
func (a *APIClient) Status() (map[string]any, error) {
	data, err := a.doRequest("GET", "/status", nil)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// Health returns the API server health.
func (a *APIClient) Health() (map[string]any, error) {
	data, err := a.doRequest("GET", "/health", nil)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}
