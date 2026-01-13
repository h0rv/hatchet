package local

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hatchet-dev/hatchet/cmd/hatchet-admin/cli/seed"
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

// LocalDriver manages a local Hatchet server without Docker
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

	// Running processes
	apiProcess    *exec.Cmd
	engineProcess *exec.Cmd
}

// LocalServerState persists the state of a running local server
type LocalServerState struct {
	ConfigDir   string    `json:"config_dir"`
	DatabaseURL string    `json:"database_url"`
	ApiPID      int       `json:"api_pid"`
	EnginePID   int       `json:"engine_pid"`
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

// Run starts the local Hatchet server
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

	// 1. Validate Postgres connection
	if err := d.validatePostgres(ctx); err != nil {
		return nil, fmt.Errorf("postgres not accessible: %w\n\nEnsure PostgreSQL is running and accessible at: %s", err, d.databaseURL)
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

	// 7. Start API server
	if err := d.startAPI(ctx); err != nil {
		return nil, fmt.Errorf("failed to start API server: %w", err)
	}

	// 8. Start Engine
	if err := d.startEngine(ctx); err != nil {
		// Clean up API if engine fails to start
		d.killProcess(d.apiProcess)
		return nil, fmt.Errorf("failed to start engine: %w", err)
	}

	// 9. Wait for health
	if err := d.waitForHealth(ctx); err != nil {
		d.killProcess(d.apiProcess)
		d.killProcess(d.engineProcess)
		return nil, fmt.Errorf("server failed to become healthy: %w", err)
	}

	// 10. Generate API token
	token, err := d.generateToken(ctx)
	if err != nil {
		d.killProcess(d.apiProcess)
		d.killProcess(d.engineProcess)
		return nil, fmt.Errorf("failed to generate API token: %w", err)
	}

	// 11. Save state for stop command
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

	var errs []error

	// Kill API process
	if state.ApiPID > 0 {
		if err := killProcessByPID(state.ApiPID); err != nil {
			errs = append(errs, fmt.Errorf("failed to kill API process (PID %d): %w", state.ApiPID, err))
		}
	}

	// Kill Engine process
	if state.EnginePID > 0 {
		if err := killProcessByPID(state.EnginePID); err != nil {
			errs = append(errs, fmt.Errorf("failed to kill engine process (PID %d): %w", state.EnginePID, err))
		}
	}

	// Remove state file
	stateFile := filepath.Join(d.configDir, StateFileName)
	if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("failed to remove state file: %w", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors during shutdown: %v", errs)
	}

	return nil
}

// IsRunning checks if a local server is currently running
func (d *LocalDriver) IsRunning() bool {
	state, err := d.loadState()
	if err != nil {
		return false
	}

	// Check if processes are actually running
	if state.ApiPID > 0 {
		if process, err := os.FindProcess(state.ApiPID); err == nil {
			if err := process.Signal(syscall.Signal(0)); err == nil {
				return true
			}
		}
	}

	return false
}

// validatePostgres checks if the database is accessible
func (d *LocalDriver) validatePostgres(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, d.databaseURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	return conn.Ping(ctx)
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

// startAPI starts the hatchet-api process
func (d *LocalDriver) startAPI(ctx context.Context) error {
	cmd := exec.Command("hatchet-api", "--config", d.configDir)
	cmd.Env = d.buildEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Set process group for clean shutdown (Unix only)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start hatchet-api: %w\n\nEnsure 'hatchet-api' binary is in your PATH", err)
	}

	d.apiProcess = cmd
	return nil
}

// startEngine starts the hatchet-engine process
func (d *LocalDriver) startEngine(ctx context.Context) error {
	cmd := exec.Command("hatchet-engine", "--config", d.configDir)
	cmd.Env = d.buildEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Set process group for clean shutdown (Unix only)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start hatchet-engine: %w\n\nEnsure 'hatchet-engine' binary is in your PATH", err)
	}

	d.engineProcess = cmd
	return nil
}

// buildEnv builds the environment variables for the server processes
func (d *LocalDriver) buildEnv() []string {
	// Start with current environment
	env := os.Environ()

	// Add Hatchet-specific variables
	hatchetEnv := []string{
		"DATABASE_URL=" + d.databaseURL,
		"SERVER_AUTH_COOKIE_DOMAIN=localhost",
		"SERVER_AUTH_COOKIE_INSECURE=t",
		"SERVER_AUTH_COOKIE_SECRETS=" + d.cookieSecrets,
		"SERVER_GRPC_BIND_ADDRESS=0.0.0.0",
		"SERVER_GRPC_INSECURE=t",
		"SERVER_GRPC_PORT=" + strconv.Itoa(d.grpcPort),
		"SERVER_GRPC_BROADCAST_ADDRESS=localhost:" + strconv.Itoa(d.grpcPort),
		"SERVER_URL=http://localhost:" + strconv.Itoa(d.apiPort),
		"SERVER_PORT=" + strconv.Itoa(d.apiPort),
		"SERVER_HEALTHCHECK_PORT=" + strconv.Itoa(d.healthcheckPort),
		"SERVER_AUTH_SET_EMAIL_VERIFIED=t",
		"SERVER_ENCRYPTION_MASTER_KEYSET=" + d.masterKey,
		"SERVER_ENCRYPTION_JWT_PRIVATE_KEYSET=" + d.privateJWT,
		"SERVER_ENCRYPTION_JWT_PUBLIC_KEYSET=" + d.publicJWT,
		"SERVER_MSGQUEUE_KIND=postgres",
		"SERVER_INTERNAL_CLIENT_INTERNAL_GRPC_BROADCAST_ADDRESS=localhost:" + strconv.Itoa(d.grpcPort),
	}

	return append(env, hatchetEnv...)
}

// waitForHealth waits for the API server to become healthy
func (d *LocalDriver) waitForHealth(ctx context.Context) error {
	healthURL := fmt.Sprintf("http://localhost:%d/api/ready", d.apiPort)

	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for server to become healthy")
		case <-ticker.C:
			resp, err := http.Get(healthURL)
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
		ApiPID:      d.apiProcess.Process.Pid,
		EnginePID:   d.engineProcess.Process.Pid,
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

// killProcess gracefully kills a process
func (d *LocalDriver) killProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	killProcessByPID(cmd.Process.Pid)
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

	// Wait up to 3 seconds for graceful shutdown
	done := make(chan error, 1)
	go func() {
		_, err := process.Wait()
		done <- err
	}()

	select {
	case <-done:
		return nil
	case <-time.After(3 * time.Second):
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
