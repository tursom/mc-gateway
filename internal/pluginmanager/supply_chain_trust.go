package pluginmanager

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

func (m *Manager) RotateTrustRoot(ctx context.Context, actor string, req TrustRootRequest) (TrustRootRecord, error) {
	req.RootID = strings.TrimSpace(req.RootID)
	req.KeyID = strings.TrimSpace(req.KeyID)
	if req.RootID == "" {
		req.RootID = "default"
	}
	if req.KeyID == "" {
		return TrustRootRecord{}, errors.New("key_id is required")
	}
	algorithm := strings.ToLower(strings.TrimSpace(req.Algorithm))
	if algorithm == "" {
		algorithm = SignatureAlgorithmEd25519
	}
	if algorithm != SignatureAlgorithmEd25519 {
		return TrustRootRecord{}, fmt.Errorf("unsupported trust root algorithm %q", algorithm)
	}
	if existing, err := m.repo.TrustRoot(ctx, req.RootID, req.KeyID); err == nil && existing.Status == TrustRootStatusRevoked {
		return TrustRootRecord{}, fmt.Errorf("trust root key %q/%q is revoked; rotate with a new key id", req.RootID, req.KeyID)
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return TrustRootRecord{}, err
	}
	publicKey, publicKeyText, err := decodeTrustRootPublicKey(req.PublicKey)
	if err != nil {
		return TrustRootRecord{}, err
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return TrustRootRecord{}, fmt.Errorf("public key length = %d, want %d", len(publicKey), ed25519.PublicKeySize)
	}
	policyJSON, err := marshalDefaultObject(req.Policy)
	if err != nil {
		return TrustRootRecord{}, err
	}
	sum := sha256.Sum256(publicKey)
	record, err := m.repo.SaveTrustRoot(ctx, actor, TrustRootRecord{
		RootID:          req.RootID,
		KeyID:           req.KeyID,
		Algorithm:       algorithm,
		PublicKey:       publicKeyText,
		PublicKeySHA256: hex.EncodeToString(sum[:]),
		Status:          TrustRootStatusTrusted,
		PolicyJSON:      policyJSON,
	})
	if err != nil {
		return TrustRootRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, "", "", "trust_root_distribution", "succeeded", actor, "plugin trust root distributed or rotated", map[string]any{
		"root_id":             record.RootID,
		"key_id":              record.KeyID,
		"algorithm":           record.Algorithm,
		"public_key_sha256":   record.PublicKeySHA256,
		"policy_hash":         stableHashJSONRaw(defaultJSONObject(record.PolicyJSON)),
		"policy_change_audit": true,
	})
	return record, nil
}

func (m *Manager) RevokeTrustRoot(ctx context.Context, actor string, req TrustRootRevokeRequest) (TrustRootRecord, error) {
	req.RootID = strings.TrimSpace(req.RootID)
	req.KeyID = strings.TrimSpace(req.KeyID)
	if req.RootID == "" {
		req.RootID = "default"
	}
	if req.KeyID == "" {
		return TrustRootRecord{}, errors.New("key_id is required")
	}
	record, err := m.repo.RevokeTrustRoot(ctx, actor, req.RootID, req.KeyID, strings.TrimSpace(req.Reason))
	if err != nil {
		return TrustRootRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, "", "", "trust_root_revoke", "succeeded", actor, "plugin trust root key revoked", map[string]any{
		"root_id":           record.RootID,
		"key_id":            record.KeyID,
		"public_key_sha256": record.PublicKeySHA256,
		"reason":            record.RevocationReason,
	})
	return record, nil
}

func (m *Manager) ListTrustRoots(ctx context.Context, rootID string) ([]TrustRootRecord, error) {
	return m.repo.ListTrustRoots(ctx, strings.TrimSpace(rootID))
}

func (m *Manager) VerifyArtifactSignature(ctx context.Context, actor string, req SignatureVerificationRequest) (SignatureVerificationResult, error) {
	req.ArtifactID = strings.TrimSpace(req.ArtifactID)
	req.PluginID = strings.TrimSpace(req.PluginID)
	req.RootID = strings.TrimSpace(req.RootID)
	req.KeyID = strings.TrimSpace(req.KeyID)
	if req.RootID == "" {
		req.RootID = "default"
	}
	if req.ArtifactID == "" {
		return SignatureVerificationResult{}, errors.New("artifact_id is required")
	}
	if strings.TrimSpace(req.Signature) == "" {
		return SignatureVerificationResult{}, errors.New("signature is required")
	}
	artifact, err := m.repo.Artifact(ctx, req.ArtifactID)
	if err != nil {
		return SignatureVerificationResult{}, err
	}
	if req.PluginID != "" && artifact.PluginID != req.PluginID {
		return SignatureVerificationResult{}, errors.New("artifact plugin_id does not match")
	}
	root, err := m.repo.TrustRoot(ctx, req.RootID, req.KeyID)
	if err != nil {
		return SignatureVerificationResult{}, err
	}
	publicKey, _, err := decodeTrustRootPublicKey(root.PublicKey)
	if err != nil {
		return SignatureVerificationResult{}, err
	}
	signature, err := decodeTrustRootBytes(req.Signature)
	if err != nil {
		return SignatureVerificationResult{}, fmt.Errorf("decode signature: %w", err)
	}
	data, err := m.signatureVerificationBytes(artifact)
	if err != nil {
		return SignatureVerificationResult{}, err
	}
	signatureValid := root.Algorithm == SignatureAlgorithmEd25519 && ed25519.Verify(ed25519.PublicKey(publicKey), data, signature)
	trusted := root.Status == TrustRootStatusTrusted
	result := SignatureVerificationResult{
		PluginID:         artifact.PluginID,
		ArtifactID:       artifact.ID,
		RootID:           root.RootID,
		KeyID:            root.KeyID,
		SignatureValid:   signatureValid,
		Trusted:          trusted,
		TrustStatus:      root.Status,
		PublicKeySHA256:  root.PublicKeySHA256,
		Revoked:          root.Status == TrustRootStatusRevoked,
		RevocationReason: root.RevocationReason,
	}
	result.Verified = result.SignatureValid && result.Trusted
	if !result.Verified {
		result.Error = "artifact signature is not trusted"
	}
	status := "succeeded"
	if !result.Verified {
		status = "failed"
	}
	_ = m.repo.RecordOperation(ctx, artifact.PluginID, artifact.ID, "signature_verify", status, actor, "plugin artifact signature verified against trust root", map[string]any{
		"root_id":           result.RootID,
		"key_id":            result.KeyID,
		"signature_valid":   result.SignatureValid,
		"trusted":           result.Trusted,
		"trust_status":      result.TrustStatus,
		"verified":          result.Verified,
		"public_key_sha256": result.PublicKeySHA256,
	})
	return result, nil
}

func (m *Manager) signatureVerificationBytes(artifact ArtifactRecord) ([]byte, error) {
	if path := m.store.DistributionPackagePath(artifact); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			return data, nil
		}
	}
	return os.ReadFile(artifact.FilePath)
}

func decodeTrustRootPublicKey(value string) ([]byte, string, error) {
	data, err := decodeTrustRootBytes(value)
	if err != nil {
		return nil, "", fmt.Errorf("decode public key: %w", err)
	}
	return data, base64.StdEncoding.EncodeToString(data), nil
}

func decodeTrustRootBytes(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("value is empty")
	}
	if data, err := base64.StdEncoding.DecodeString(value); err == nil {
		return data, nil
	}
	if data, err := base64.RawStdEncoding.DecodeString(value); err == nil {
		return data, nil
	}
	var raw []byte
	if err := json.Unmarshal([]byte(value), &raw); err == nil {
		return raw, nil
	}
	return nil, errors.New("value must be base64 encoded")
}
