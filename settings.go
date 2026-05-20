package paygate

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/farhanahmed1/paygate/internal/crypto"
)

// aad binds a ciphertext to (namespace, gateway name): a stripe blob cannot be
// decrypted as paypal, nor across hosts using different namespaces.
func (s *Service) aad(name string) []byte {
	return []byte(s.namespace + "|" + name)
}

// encryptConfig marshals and encrypts a gateway credential map with AES-256-GCM.
// An empty map encrypts to the empty string (the column default, meaning "not
// configured").
func (s *Service) encryptConfig(name string, config map[string]string) (string, error) {
	if len(config) == 0 {
		return "", nil
	}
	plaintext, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("paygate: marshal gateway config: %w", err)
	}
	return crypto.Encrypt(s.key, s.aad(name), plaintext)
}

// decryptConfig reverses encryptConfig. An empty ciphertext yields an empty map.
func (s *Service) decryptConfig(name, encrypted string) (map[string]string, error) {
	if encrypted == "" {
		return map[string]string{}, nil
	}
	plaintext, err := crypto.Decrypt(s.key, s.aad(name), encrypted)
	if err != nil {
		return nil, fmt.Errorf("paygate: decrypt gateway config: %w", err)
	}
	var config map[string]string
	if err := json.Unmarshal(plaintext, &config); err != nil {
		return nil, fmt.Errorf("paygate: unmarshal gateway config: %w", err)
	}
	return config, nil
}

// GatewaySetting loads one gateway's configuration and decrypts its
// credentials. Returns ErrGatewayNotFound if no row exists for the gateway.
func (s *Service) GatewaySetting(name string) (*GatewaySetting, error) {
	if !validGateway(name) {
		return nil, fmt.Errorf("paygate: unknown gateway %q", name)
	}

	var (
		gs        GatewaySetting
		encConfig string
	)
	err := s.db.QueryRow(
		`SELECT name, display_name, is_enabled, is_test_mode, encrypted_config
		   FROM payment_gateway_settings
		  WHERE name = $1`, name,
	).Scan(&gs.Name, &gs.DisplayName, &gs.IsEnabled, &gs.IsTestMode, &encConfig)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrGatewayNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("paygate: load gateway %q: %w", name, err)
	}

	gs.Config, err = s.decryptConfig(name, encConfig)
	if err != nil {
		return nil, err
	}
	return &gs, nil
}

// UpsertGatewaySetting creates or updates a gateway's configuration. config is
// encrypted at rest. Pass a nil/empty map to clear credentials while keeping
// the row (e.g. to disable a gateway without losing its toggles). updated_at
// is maintained by the update_payment_gateway_settings_updated_at trigger.
func (s *Service) UpsertGatewaySetting(name, displayName string, isEnabled, isTestMode bool, config map[string]string) error {
	if !validGateway(name) {
		return fmt.Errorf("paygate: unknown gateway %q", name)
	}

	enc, err := s.encryptConfig(name, config)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(
		`INSERT INTO payment_gateway_settings (name, display_name, is_enabled, is_test_mode, encrypted_config)
		      VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (name) DO UPDATE
		    SET display_name     = EXCLUDED.display_name,
		        is_enabled       = EXCLUDED.is_enabled,
		        is_test_mode     = EXCLUDED.is_test_mode,
		        encrypted_config = EXCLUDED.encrypted_config`,
		name, displayName, isEnabled, isTestMode, enc,
	)
	if err != nil {
		return fmt.Errorf("paygate: upsert gateway %q: %w", name, err)
	}
	return nil
}
