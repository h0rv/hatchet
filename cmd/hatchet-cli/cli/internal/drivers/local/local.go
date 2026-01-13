package local

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hatchet-dev/hatchet/cmd/hatchet-admin/cli/seed"
	"github.com/hatchet-dev/hatchet/cmd/hatchet-api/api"
	"github.com/hatchet-dev/hatchet/cmd/hatchet-engine/engine"
	"github.com/hatchet-dev/hatchet/cmd/hatchet-migrate/migrate"
	cliconfig "github.com/hatchet-dev/hatchet/pkg/config/cli"
	"github.com/hatchet-dev/hatchet/pkg/config/loader"
	"github.com/hatchet-dev/hatchet/pkg/config/server"
)

const (
	DefaultAPIPort         = 8080
	DefaultGRPCPort        = 7077
	DefaultHealthcheckPort = 8733
	StateFileName          = "state.json"
	KeysFileName           = "keys.json"
)

// LocalDriver manages a local Hatchet server running in-process
type LocalDriver struct {
	configDir       string
	databaseURL     string
	apiPort         int
	grpcPort        int
	healthcheckPort int
	profileName     string

	// Encryption keys
	masterKey     string
	privateJWT    string
	publicJWT     string
	cookieSecrets string
}

// LocalServerState persists the state of a running local server
type LocalServerState struct {
	ConfigDir   string    `json:"config_dir"`
	DatabaseURL string    `json:"database_url"`
	PID         int       `json:"pid"` // PID of the CLI process running the server
	ApiPort     int       `json:"api_port"`
	GrpcPort    int       `json:"grpc_port"`
	ProfileName string    `json:"profile_name"`
	StartedAt   time.Time `json:"started_at"`
}

// LocalOpts configures the local driver
type LocalOpts struct {
	DatabaseURL     string
	APIPort         int
	GRPCPort        int
	HealthcheckPort int
	ProfileName     string
}

// LocalOpt is a functional option for LocalDriver
type LocalOpt func(*LocalOpts)

// WithDatabaseURL sets the database URL
func WithDatabaseURL(url string) LocalOpt {
	return func(o *LocalOpts) {
		o.DatabaseURL = url
	}
}

// WithAPIPort sets the API port
func WithAPIPort(port int) LocalOpt {
	return func(o *LocalOpts) {
		o.APIPort = port
	}
}

// WithGRPCPort sets the gRPC port
func WithGRPCPort(port int) LocalOpt {
	return func(o *LocalOpts) {
		o.GRPCPort = port
	}
}

// WithHealthcheckPort sets the healthcheck port
func WithHealthcheckPort(port int) LocalOpt {
	return func(o *LocalOpts) {
		o.HealthcheckPort = port
	}
}

// WithProfileName sets the profile name
func WithProfileName(name string) LocalOpt {
	return func(o *LocalOpts) {
		o.ProfileName = name
	}
}

// NewLocalDriver creates a new local driver
func NewLocalDriver() *LocalDriver {
	homeDir, _ := os.UserHomeDir()
	configDir := filepath.Join(homeDir, ".hatchet", "local")

	return &LocalDriver{
		configDir: configDir,
		apiPort:   DefaultAPIPort,
		grpcPort:  DefaultGRPCPort,
	}
}

// Run starts the local Hatchet server in-process (foreground mode)
// This method blocks until the server is stopped via interrupt signal
func (d *LocalDriver) Run(ctx context.Context, opts ...LocalOpt) (*RunResult, error) {
	// Apply options
	// Default DB URL works with Mac Homebrew Postgres (current user, no password)
	options := &LocalOpts{
		DatabaseURL:     "postgresql://localhost:5432/hatchet?sslmode=disable",
		APIPort:         DefaultAPIPort,
		GRPCPort:        DefaultGRPCPort,
		HealthcheckPort: DefaultHealthcheckPort,
		ProfileName:     "local",
	}
	for _, opt := range opts {
		opt(options)
	}

	d.databaseURL = options.DatabaseURL
	d.apiPort = options.APIPort
	d.grpcPort = options.GRPCPort
	d.healthcheckPort = options.HealthcheckPort
	d.profileName = options.ProfileName

	// 1. Ensure database exists and is configured correctly
	if err := d.ensureDatabase(ctx); err != nil {
		return nil, fmt.Errorf("database setup failed: %w", err)
	}

	// 2. Initialize config directory
	if err := d.initConfigDir(); err != nil {
		return nil, fmt.Errorf("failed to initialize config directory: %w", err)
	}

	// 3. Generate or load encryption keys
	if err := d.ensureEncryptionKeys(); err != nil {
		return nil, fmt.Errorf("failed to setup encryption keys: %w", err)
	}

	// 4. Write config files
	if err := d.writeConfigFiles(); err != nil {
		return nil, fmt.Errorf("failed to write config files: %w", err)
	}

	// 5. Run migrations
	if err := d.runMigrations(ctx); err != nil {
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	// 6. Seed database (idempotent)
	if err := d.seedDatabase(); err != nil {
		return nil, fmt.Errorf("failed to seed database: %w", err)
	}

	// 7. Generate API token before starting server
	token, err := d.generateToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to generate API token: %w", err)
	}

	// 8. Save state for stop command (PID of this process)
	if err := d.saveState(); err != nil {
		return nil, fmt.Errorf("failed to save state: %w", err)
	}

	return &RunResult{
		ProfileName: d.profileName,
		Token:       token,
		APIPort:     d.apiPort,
		GRPCPort:    d.grpcPort,
	}, nil
}

// StartServer starts the API and engine in-process and blocks until interrupted
// This should be called after Run() to actually start the server
func (d *LocalDriver) StartServer(ctx context.Context, interruptCh <-chan interface{}, onReady func()) error {
	// Set environment variables for the server
	d.setEnvVars()

	// Create config loader
	cf := loader.NewConfigLoader(d.configDir)

	// Track errors from goroutines
	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	// Start API in goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := api.Start(cf, interruptCh, "local"); err != nil {
			log.Printf("API error: %v", err)
			errCh <- fmt.Errorf("API server error: %w", err)
		}
	}()

	// Start Engine in goroutine
	engineCtx, engineCancel := context.WithCancel(ctx)
	defer engineCancel()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := engine.Run(engineCtx, cf, "local"); err != nil {
			log.Printf("Engine error: %v", err)
			errCh <- fmt.Errorf("engine error: %w", err)
		}
	}()

	// Wait for API to be ready
	if err := d.waitForHealth(ctx); err != nil {
		return fmt.Errorf("server failed to become healthy: %w", err)
	}

	// Signal that server is ready
	if onReady != nil {
		onReady()
	}

	// Wait for interrupt or error
	select {
	case <-interruptCh:
		// Clean shutdown initiated
		log.Println("cleaning up server config")
	case err := <-errCh:
		return err
	}

	// Cancel engine context to trigger shutdown
	engineCancel()

	// Wait for all goroutines to finish (with timeout)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Clean exit
	case <-time.After(10 * time.Second):
		log.Println("shutdown timeout, some goroutines may not have exited cleanly")
	}

	// Clean up state file
	d.removeState()

	return nil
}

// RunResult contains the result of starting a local server
type RunResult struct {
	ProfileName string
	Token       string
	APIPort     int
	GRPCPort    int
}

// Stop stops the local Hatchet server
func (d *LocalDriver) Stop() error {
	state, err := d.loadState()
	if err != nil {
		return fmt.Errorf("no local server running or state file not found: %w", err)
	}

	// Send SIGTERM to the process
	if state.PID > 0 {
		if err := killProcessByPID(state.PID); err != nil {
			return fmt.Errorf("failed to stop server (PID %d): %w", state.PID, err)
		}
	}

	// Remove state file
	d.removeState()

	return nil
}

// IsRunning checks if a local server is currently running
func (d *LocalDriver) IsRunning() bool {
	state, err := d.loadState()
	if err != nil {
		return false
	}

	// Check if process is actually running
	if state.PID > 0 {
		if process, err := os.FindProcess(state.PID); err == nil {
			if err := process.Signal(syscall.Signal(0)); err == nil {
				return true
			}
		}
	}

	return false
}

// ensureDatabase creates the database if needed and configures timezone
func (d *LocalDriver) ensureDatabase(ctx context.Context) error {
	// Parse the database URL to extract database name
	parsedURL, err := url.Parse(d.databaseURL)
	if err != nil {
		return fmt.Errorf("invalid database URL: %w", err)
	}

	dbName := strings.TrimPrefix(parsedURL.Path, "/")
	if dbName == "" {
		dbName = "hatchet"
	}

	// Validate database name to prevent SQL injection
	// Only allow alphanumeric characters and underscores
	if !isValidIdentifier(dbName) {
		return fmt.Errorf("invalid database name: %s (only alphanumeric and underscores allowed)", dbName)
	}

	// Build connection URL for the default 'postgres' database
	// Preserve query params (like sslmode) from original URL
	adminURL := *parsedURL
	adminURL.Path = "/postgres"
	adminConnStr := adminURL.String()

	// Try to connect to the target database first
	conn, err := pgx.Connect(ctx, d.databaseURL)
	if err == nil {
		// Database exists, just ensure timezone is set
		conn.Close(ctx)
		return d.ensureTimezone(ctx, dbName, adminConnStr)
	}

	// Database might not exist, try to create it
	log.Printf("Creating database '%s'...", dbName)

	adminConn, err := pgx.Connect(ctx, adminConnStr)
	if err != nil {
		return fmt.Errorf("could not connect to PostgreSQL: %w\n\nEnsure PostgreSQL is running and accessible", err)
	}
	defer adminConn.Close(ctx)

	// Create database (ignore error if it already exists)
	// Using quoted identifier to safely handle the database name
	_, err = adminConn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, dbName))
	if err != nil {
		// Check if error is "database already exists" - that's fine
		if !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("could not create database: %w", err)
		}
	}

	// Set timezone to UTC
	return d.ensureTimezone(ctx, dbName, adminConnStr)
}

// ensureTimezone sets the database timezone to UTC
func (d *LocalDriver) ensureTimezone(ctx context.Context, dbName, adminConnStr string) error {
	adminConn, err := pgx.Connect(ctx, adminConnStr)
	if err != nil {
		return fmt.Errorf("could not connect to PostgreSQL: %w", err)
	}
	defer adminConn.Close(ctx)

	// Set timezone to UTC (required by Hatchet)
	// Using quoted identifier to safely handle the database name
	_, err = adminConn.Exec(ctx, fmt.Sprintf(`ALTER DATABASE "%s" SET TIMEZONE='UTC'`, dbName))
	if err != nil {
		return fmt.Errorf("could not set timezone: %w", err)
	}

	// Verify we can connect to the target database
	conn, err := pgx.Connect(ctx, d.databaseURL)
	if err != nil {
		return fmt.Errorf("could not connect to database: %w", err)
	}
	defer conn.Close(ctx)

	return conn.Ping(ctx)
}

// isValidIdentifier checks if a string is a valid PostgreSQL identifier
// Only allows alphanumeric characters and underscores
func isValidIdentifier(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, c := range s {
		if i == 0 && c >= '0' && c <= '9' {
			return false // Can't start with a digit
		}
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

// initConfigDir creates the config directory if it doesn't exist
func (d *LocalDriver) initConfigDir() error {
	return os.MkdirAll(d.configDir, 0700)
}

// runMigrations runs database migrations
func (d *LocalDriver) runMigrations(ctx context.Context) error {
	// Set DATABASE_URL for the migrate package
	os.Setenv("DATABASE_URL", d.databaseURL)

	// Run migrations - this is safe to call multiple times
	migrate.RunMigrations(ctx)

	return nil
}

// seedDatabase seeds the database with initial data
func (d *LocalDriver) seedDatabase() error {
	configLoader := loader.NewConfigLoader(d.configDir)

	dbLayer, err := configLoader.InitDataLayer()
	if err != nil {
		return fmt.Errorf("failed to initialize data layer: %w", err)
	}
	defer dbLayer.Disconnect() // nolint: errcheck

	// Check if already seeded by looking for admin user
	// seed.SeedDatabase handles this internally
	return seed.SeedDatabase(dbLayer)
}

// generateToken generates an API token for the local profile
func (d *LocalDriver) generateToken(ctx context.Context) (string, error) {
	configLoader := loader.NewConfigLoader(d.configDir)

	cleanup, serverConfig, err := configLoader.CreateServerFromConfig("local",
		func(scf *server.ServerConfigFile) {
			scf.MessageQueue.Enabled = false
			scf.SecurityCheck.Enabled = false
		},
	)
	if err != nil {
		return "", fmt.Errorf("failed to create server config: %w", err)
	}
	defer cleanup() // nolint: errcheck

	// Use default tenant ID from seed
	tenantID := "707d0855-80ab-4e1f-a156-f1c4546cbf52"

	expiresAt := time.Now().UTC().Add(365 * 24 * time.Hour) // 1 year

	token, err := serverConfig.Auth.JWTManager.GenerateTenantToken(
		ctx,
		tenantID,
		"local-cli-token",
		false,
		&expiresAt,
	)
	if err != nil {
		return "", err
	}

	return token.Token, nil
}

// setEnvVars sets environment variables for the in-process server
func (d *LocalDriver) setEnvVars() {
	os.Setenv("DATABASE_URL", d.databaseURL)
	os.Setenv("SERVER_AUTH_COOKIE_DOMAIN", "localhost")
	os.Setenv("SERVER_AUTH_COOKIE_INSECURE", "t")
	os.Setenv("SERVER_AUTH_COOKIE_SECRETS", d.cookieSecrets)
	os.Setenv("SERVER_GRPC_BIND_ADDRESS", "0.0.0.0")
	os.Setenv("SERVER_GRPC_INSECURE", "t")
	os.Setenv("SERVER_GRPC_PORT", strconv.Itoa(d.grpcPort))
	os.Setenv("SERVER_GRPC_BROADCAST_ADDRESS", fmt.Sprintf("localhost:%d", d.grpcPort))
	os.Setenv("SERVER_URL", fmt.Sprintf("http://localhost:%d", d.apiPort))
	os.Setenv("SERVER_PORT", strconv.Itoa(d.apiPort))
	os.Setenv("SERVER_HEALTHCHECK_PORT", strconv.Itoa(d.healthcheckPort))
	os.Setenv("SERVER_AUTH_SET_EMAIL_VERIFIED", "t")
	os.Setenv("SERVER_ENCRYPTION_MASTER_KEYSET", d.masterKey)
	os.Setenv("SERVER_ENCRYPTION_JWT_PRIVATE_KEYSET", d.privateJWT)
	os.Setenv("SERVER_ENCRYPTION_JWT_PUBLIC_KEYSET", d.publicJWT)
	os.Setenv("SERVER_MSGQUEUE_KIND", "postgres")
	os.Setenv("SERVER_INTERNAL_CLIENT_INTERNAL_GRPC_BROADCAST_ADDRESS", fmt.Sprintf("localhost:%d", d.grpcPort))
}

// waitForHealth waits for the API server to become healthy
func (d *LocalDriver) waitForHealth(ctx context.Context) error {
	healthURL := fmt.Sprintf("http://localhost:%d/api/ready", d.apiPort)

	// Create a context with timeout for the entire health check operation
	healthCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	client := &http.Client{Timeout: 2 * time.Second}

	for {
		select {
		case <-healthCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("timeout waiting for server to become healthy")
		case <-ticker.C:
			// Use context-aware request to properly cancel on shutdown
			req, err := http.NewRequestWithContext(healthCtx, "GET", healthURL, nil)
			if err != nil {
				continue
			}
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
	}
}

// saveState saves the current server state to disk
func (d *LocalDriver) saveState() error {
	state := LocalServerState{
		ConfigDir:   d.configDir,
		DatabaseURL: d.databaseURL,
		PID:         os.Getpid(), // Current process PID (in-process server)
		ApiPort:     d.apiPort,
		GrpcPort:    d.grpcPort,
		ProfileName: d.profileName,
		StartedAt:   time.Now(),
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	stateFile := filepath.Join(d.configDir, StateFileName)
	return os.WriteFile(stateFile, data, 0600)
}

// loadState loads the server state from disk
func (d *LocalDriver) loadState() (*LocalServerState, error) {
	stateFile := filepath.Join(d.configDir, StateFileName)

	data, err := os.ReadFile(stateFile)
	if err != nil {
		return nil, err
	}

	var state LocalServerState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}

	return &state, nil
}

// removeState removes the state file
func (d *LocalDriver) removeState() {
	stateFile := filepath.Join(d.configDir, StateFileName)
	if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
		log.Printf("Warning: failed to remove state file: %v", err)
	}
}

// killProcessByPID kills a process by PID with graceful shutdown
func killProcessByPID(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	// Try SIGTERM first
	if err := process.Signal(syscall.SIGTERM); err != nil {
		// Process might already be dead
		if err == os.ErrProcessDone {
			return nil
		}
		return err
	}

	// Wait up to 5 seconds for graceful shutdown
	done := make(chan error, 1)
	go func() {
		_, err := process.Wait()
		done <- err
	}()

	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		// Force kill
		return process.Kill()
	}
}

// GetConfigDir returns the config directory path
func (d *LocalDriver) GetConfigDir() string {
	return d.configDir
}

// GetStateFilePath returns the state file path
func GetStateFilePath() string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".hatchet", "local", StateFileName)
}

// IsLocalServerRunning checks if a local server is running by checking the state file
func IsLocalServerRunning() bool {
	driver := NewLocalDriver()
	return driver.IsRunning()
}

// DefaultTenantID is the tenant ID created by the seed
const DefaultTenantID = "707d0855-80ab-4e1f-a156-f1c4546cbf52"

// CreateProfileFromResult creates a CLI profile from the run result
func CreateProfileFromResult(result *RunResult) (*cliconfig.Profile, error) {
	return &cliconfig.Profile{
		Name:         result.ProfileName,
		Token:        result.Token,
		TenantId:     DefaultTenantID,
		ApiServerURL: fmt.Sprintf("http://localhost:%d", result.APIPort),
		GrpcHostPort: fmt.Sprintf("localhost:%d", result.GRPCPort),
		TLSStrategy:  "none",
		ExpiresAt:    time.Now().Add(365 * 24 * time.Hour), // 1 year
	}, nil
}
