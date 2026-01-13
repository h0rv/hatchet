package local

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	"github.com/hatchet-dev/hatchet/pkg/config/database"
	"github.com/hatchet-dev/hatchet/pkg/config/server"
	"github.com/hatchet-dev/hatchet/pkg/encryption"
	"github.com/hatchet-dev/hatchet/pkg/random"
)

// StoredKeys represents the encryption keys stored on disk
type StoredKeys struct {
	MasterKey     string `json:"master_key"`
	PrivateJWT    string `json:"private_jwt"`
	PublicJWT     string `json:"public_jwt"`
	CookieSecrets string `json:"cookie_secrets"`
}

// ensureEncryptionKeys loads existing keys or generates new ones
func (d *LocalDriver) ensureEncryptionKeys() error {
	keysPath := filepath.Join(d.configDir, KeysFileName)

	// Try to load existing keys
	if fileExists(keysPath) {
		return d.loadKeys(keysPath)
	}

	// Generate new keys
	masterKey, privateJWT, publicJWT, err := encryption.GenerateLocalKeys()
	if err != nil {
		return fmt.Errorf("failed to generate encryption keys: %w", err)
	}

	d.masterKey = string(masterKey)
	d.privateJWT = string(privateJWT)
	d.publicJWT = string(publicJWT)

	// Generate cookie secrets
	cookieSecrets, err := generateCookieSecrets()
	if err != nil {
		return fmt.Errorf("failed to generate cookie secrets: %w", err)
	}
	d.cookieSecrets = cookieSecrets

	// Save keys for future use
	return d.saveKeys(keysPath)
}

// loadKeys loads encryption keys from disk
func (d *LocalDriver) loadKeys(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var keys StoredKeys
	if err := json.Unmarshal(data, &keys); err != nil {
		return err
	}

	d.masterKey = keys.MasterKey
	d.privateJWT = keys.PrivateJWT
	d.publicJWT = keys.PublicJWT
	d.cookieSecrets = keys.CookieSecrets

	return nil
}

// saveKeys saves encryption keys to disk
func (d *LocalDriver) saveKeys(path string) error {
	keys := StoredKeys{
		MasterKey:     d.masterKey,
		PrivateJWT:    d.privateJWT,
		PublicJWT:     d.publicJWT,
		CookieSecrets: d.cookieSecrets,
	}

	data, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0600)
}

// generateCookieSecrets generates cookie hash and block keys
func generateCookieSecrets() (string, error) {
	hashKey, err := random.Generate(16)
	if err != nil {
		return "", fmt.Errorf("failed to generate cookie hash key: %w", err)
	}

	blockKey, err := random.Generate(16)
	if err != nil {
		return "", fmt.Errorf("failed to generate cookie block key: %w", err)
	}

	return fmt.Sprintf("%s %s", hashKey, blockKey), nil
}

// writeConfigFiles writes the database.yaml and server.yaml config files
func (d *LocalDriver) writeConfigFiles() error {
	// Write database.yaml
	if err := d.writeDatabaseConfig(); err != nil {
		return fmt.Errorf("failed to write database config: %w", err)
	}

	// Write server.yaml
	if err := d.writeServerConfig(); err != nil {
		return fmt.Errorf("failed to write server config: %w", err)
	}

	return nil
}

// writeDatabaseConfig writes the database.yaml config file
func (d *LocalDriver) writeDatabaseConfig() error {
	config := &database.ConfigFile{
		Seed: database.SeedConfigFile{
			AdminEmail:        "admin@example.com",
			AdminPassword:     "Admin123!!",
			AdminName:         "Admin",
			DefaultTenantName: "Default",
			DefaultTenantSlug: "default",
			DefaultTenantID:   DefaultTenantID, // Use constant defined in local.go
		},
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return err
	}

	configPath := filepath.Join(d.configDir, "database.yaml")
	return os.WriteFile(configPath, data, 0600)
}

// writeServerConfig writes the server.yaml config file
func (d *LocalDriver) writeServerConfig() error {
	config := &server.ServerConfigFile{
		Auth: server.ConfigFileAuth{
			Cookie: server.ConfigFileAuthCookie{
				Domain:   "localhost",
				Insecure: true,
				Secrets:  d.cookieSecrets,
				Name:     "hatchet",
			},
			SetEmailVerified: true,
		},
		Encryption: server.EncryptionConfigFile{
			MasterKeyset: d.masterKey,
			JWT: server.EncryptionConfigFileJWT{
				PrivateJWTKeyset: d.privateJWT,
				PublicJWTKeyset:  d.publicJWT,
			},
		},
		Runtime: server.ConfigFileRuntime{
			Port:                 d.apiPort,
			ServerURL:            fmt.Sprintf("http://localhost:%d", d.apiPort),
			GRPCPort:             d.grpcPort,
			GRPCBindAddress:      "0.0.0.0",
			GRPCBroadcastAddress: fmt.Sprintf("localhost:%d", d.grpcPort),
			GRPCInsecure:         true,
			AllowSignup:          true,
			AllowInvites:         true,
			AllowCreateTenant:    true,
			AllowChangePassword:  true,
		},
		MessageQueue: server.MessageQueueConfigFile{
			Enabled: true,
			Kind:    "postgres",
		},
		SecurityCheck: server.SecurityCheckConfigFile{
			Enabled: false,
		},
		EnableDataRetention:   true,
		EnableWorkerRetention: false,
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return err
	}

	configPath := filepath.Join(d.configDir, "server.yaml")
	return os.WriteFile(configPath, data, 0600)
}

// fileExists checks if a file exists
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
