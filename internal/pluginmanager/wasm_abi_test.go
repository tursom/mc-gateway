package pluginmanager

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestWASMABISchemaEncodeDecodeCanonical(t *testing.T) {
	reqBytes, err := EncodeWASMABIRequest(WASMABIRequest{
		ExtensionPoint: ExtensionConfigValidate,
		PluginID:       "plugin-a",
		ArtifactID:     "artifact-a",
		Config:         []byte(`{"z":2,"a":1}`),
	})
	if err != nil {
		t.Fatalf("EncodeWASMABIRequest() error = %v", err)
	}
	req, canonical, err := DecodeWASMABIRequest(ExtensionConfigValidate, reqBytes)
	if err != nil {
		t.Fatalf("DecodeWASMABIRequest() error = %v", err)
	}
	if req.ABI != wasmHostABIV1 || req.FailPolicy != ExternalFailPolicyClosed || strings.Contains(string(canonical), " ") {
		t.Fatalf("request = %+v canonical=%s, want v1 fail-closed canonical JSON", req, canonical)
	}
	if !strings.Contains(string(canonical), `"config":{"a":1,"z":2}`) {
		t.Fatalf("canonical request = %s, want nested RawMessage object keys sorted", canonical)
	}

	valid := true
	respBytes, err := EncodeWASMABIResponse(WASMABIResponse{
		ExtensionPoint: ExtensionConfigValidate,
		OK:             true,
		Valid:          &valid,
	})
	if err != nil {
		t.Fatalf("EncodeWASMABIResponse() error = %v", err)
	}
	resp, canonicalResp, err := DecodeWASMABIResponse(ExtensionConfigValidate, respBytes)
	if err != nil {
		t.Fatalf("DecodeWASMABIResponse() error = %v", err)
	}
	if !resp.OK || resp.Valid == nil || !*resp.Valid || strings.Contains(string(canonicalResp), " ") {
		t.Fatalf("response = %+v canonical=%s, want valid canonical response", resp, canonicalResp)
	}
}

func TestWASMABISchemaRejectsUnknownFieldBadJSONAndOversized(t *testing.T) {
	_, _, err := DecodeWASMABIRequest(ExtensionConfigValidate, []byte(`{"abi":"mc-gateway.wasm.host/v1","extension_point":"config.validate/v1","fail_policy":"fail_closed","config":{},"extra":true}`))
	if got := wasmABIErrorCode(err); got != wasmABIErrorUnknownField {
		t.Fatalf("DecodeWASMABIRequest(unknown) error = %v code=%q, want %s", err, got, wasmABIErrorUnknownField)
	}
	_, _, err = DecodeWASMABIRequest(ExtensionConfigValidate, []byte(`{"abi":`))
	if got := wasmABIErrorCode(err); got != wasmABIErrorBadJSON {
		t.Fatalf("DecodeWASMABIRequest(bad json) error = %v code=%q, want %s", err, got, wasmABIErrorBadJSON)
	}
	_, _, err = DecodeWASMABIRequest(ExtensionConfigValidate, []byte(strings.Repeat("x", wasmABIMaxInputBytes+1)))
	if got := wasmABIErrorCode(err); got != wasmABIErrorOversizedInput {
		t.Fatalf("DecodeWASMABIRequest(oversized) error = %v code=%q, want %s", err, got, wasmABIErrorOversizedInput)
	}
	_, _, err = DecodeWASMABIResponse(ExtensionRuleEvaluate, []byte(strings.Repeat("x", wasmABIMaxOutputBytes+1)))
	if got := wasmABIErrorCode(err); got != wasmABIErrorOversizedOutput {
		t.Fatalf("DecodeWASMABIResponse(oversized) error = %v code=%q, want %s", err, got, wasmABIErrorOversizedOutput)
	}
}

func TestWASMABIExportMappingAndModuleValidation(t *testing.T) {
	manifest := wasmTestManifest([]ExtensionPoint{
		{Type: "validator", Key: ExtensionConfigValidate},
		{Type: "rule", Key: ExtensionRuleEvaluate},
		{Type: "provider", Key: ExtensionRouteResolve},
	})
	if got, ok := WASMABIExportForExtension(ExtensionConfigValidate); !ok || got != wasmExportConfigValidateV1 {
		t.Fatalf("WASMABIExportForExtension(config) = %q %v, want %q", got, ok, wasmExportConfigValidateV1)
	}
	if err := validateWASMModuleABI(context.Background(), wasmOKModule, manifest, 64*1024); err != nil {
		t.Fatalf("validateWASMModuleABI(ok) error = %v", err)
	}
	if err := validateWASMModuleABI(context.Background(), wasmLegacyValidateModule(), manifest, 64*1024); wasmABIErrorCode(err) != wasmABIErrorExportMissing {
		t.Fatalf("validateWASMModuleABI(legacy validate) error = %v code=%q, want export_missing", err, wasmABIErrorCode(err))
	}
}

func TestWASMABIValidationRejectsDeniedHostImport(t *testing.T) {
	manifest := wasmTestManifest([]ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}})
	tests := []struct {
		name       string
		moduleName string
		importName string
	}{
		{name: "env secret", moduleName: "env", importName: "secret"},
		{name: "host secret", moduleName: wasmHostImportModule, importName: "secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWASMModuleABI(context.Background(), wasmModuleWithImport(tt.moduleName, tt.importName), manifest, 64*1024)
			if got := wasmABIErrorCode(err); got != wasmABIErrorImportDenied {
				t.Fatalf("validateWASMModuleABI(%s.%s) error = %v code=%q, want %s", tt.moduleName, tt.importName, err, got, wasmABIErrorImportDenied)
			}
		})
	}
}

func TestWASMABIMismatchBlocksPreflightAndPrepare(t *testing.T) {
	manager := newManagerForTest(t, &fakeAdapter{})
	manifest := wasmTestManifest([]ExtensionPoint{{Type: "rule", Key: ExtensionRuleEvaluate}})
	manifest.Runtime.ABI = "mc-gateway.wasm.host/v0"
	artifact := ArtifactRecord{
		ID:                      "artifact-a",
		PluginID:                manifest.ID,
		ArtifactType:            ArtifactTypeBinary,
		RuntimeType:             RuntimeWASM,
		RuntimeEntry:            RuntimeWASMEntry,
		MetadataJSON:            mustJSON(t, manifest),
		CapabilitiesSummaryJSON: `{}`,
	}
	preflight := manager.preflightChecks(context.Background(), PluginRecord{ID: manifest.ID, ConfigJSON: `{}`}, artifact, manifest, PolicyProfileProd, GovernanceActionEnable, `{}`)
	if preflight.OK || !preflightCheckCodes(preflight.Checks)["wasm_abi_invalid"] {
		t.Fatalf("preflight = %+v, want wasm_abi_invalid block", preflight)
	}
	adapter := WASMAdapter{Mode: PluginServiceModeInProcess}
	if _, err := adapter.Prepare(context.Background(), artifact, PluginRecord{ID: manifest.ID}); wasmABIErrorCode(err) != wasmABIErrorABIMismatch {
		t.Fatalf("Prepare(abi mismatch) error = %v code=%q, want abi_mismatch", err, wasmABIErrorCode(err))
	}
}

func TestWASMABIErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "timeout", err: context.DeadlineExceeded, code: wasmABIErrorTimeout},
		{name: "trap", err: errors.New("wasm error: unreachable"), code: wasmABIErrorTrap},
		{name: "memory", err: errors.New("memory limit exceeded"), code: wasmABIErrorMemoryExceeded},
		{name: "bad output", err: errors.New("result pointer is invalid"), code: wasmABIErrorBadOutput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mapWASMInvocationError(ExtensionRuleEvaluate, tt.err)
			if got := wasmABIErrorCode(err); got != tt.code {
				t.Fatalf("mapWASMInvocationError() = %v code=%q, want %s", err, got, tt.code)
			}
		})
	}
}

func wasmTestManifest(points []ExtensionPoint) Manifest {
	return Manifest{
		SchemaVersion: SchemaVersion,
		ID:            "wasm-test",
		Name:          "WASM Test",
		Version:       "0.1.0",
		ArtifactType:  ArtifactTypeBinary,
		Runtime: RuntimeManifest{
			Type:  RuntimeWASM,
			Entry: RuntimeWASMEntry,
			ABI:   wasmHostABIV1,
		},
		APIVersion:      APIVersion,
		ExtensionPoints: points,
		Capabilities:    []byte(`{}`),
	}
}

func wasmLegacyValidateModule() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x0c, 0x01, 0x08, 0x76, 0x61, 0x6c, 0x69, 0x64, 0x61, 0x74, 0x65, 0x00, 0x00,
		0x0a, 0x04, 0x01, 0x02, 0x00, 0x0b,
	}
}

func wasmModuleWithImport(moduleName, importName string) []byte {
	module := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	module = append(module, wasmSection(1, []byte{0x01, 0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7e})...)
	importPayload := appendU32LEB(nil, 1)
	importPayload = appendU32LEB(importPayload, uint32(len(moduleName)))
	importPayload = append(importPayload, moduleName...)
	importPayload = appendU32LEB(importPayload, uint32(len(importName)))
	importPayload = append(importPayload, importName...)
	importPayload = append(importPayload, 0x00, 0x00)
	module = append(module, wasmSection(2, importPayload)...)
	module = append(module, wasmSection(3, []byte{0x01, 0x00})...)
	module = append(module, wasmSection(7, wasmABIExportSectionForFunction(1))...)
	functionBody := []byte{0x00, 0x42, 0x00, 0x0b}
	body := appendU32LEB(nil, uint32(len(functionBody)))
	body = append(body, functionBody...)
	code := appendU32LEB(nil, 1)
	code = append(code, body...)
	module = append(module, wasmSection(10, code)...)
	return module
}

func wasmABIExportSectionForFunction(index uint32) []byte {
	exports := []string{wasmExportConfigValidateV1, wasmExportRuleEvaluateV1, wasmExportRouteResolveV1}
	payload := appendU32LEB(nil, uint32(len(exports)))
	for _, name := range exports {
		payload = appendU32LEB(payload, uint32(len(name)))
		payload = append(payload, name...)
		payload = append(payload, 0x00)
		payload = appendU32LEB(payload, index)
	}
	return payload
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("jsonMarshal() error = %v", err)
	}
	return string(data)
}
