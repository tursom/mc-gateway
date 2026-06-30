package pluginmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/tetratelabs/wazero"
	wazeroapi "github.com/tetratelabs/wazero/api"
)

const (
	wasmHostABIV1 = "mc-gateway.wasm.host/v1"

	wasmExportConfigValidateV1 = "mcgw_config_validate_v1"
	wasmExportRuleEvaluateV1   = "mcgw_rule_evaluate_v1"
	wasmExportRouteResolveV1   = "mcgw_route_resolve_v1"

	wasmHostImportModule = "mcgw_host"
	wasmHostImportLog    = "log"
	wasmHostImportMetric = "metric"

	wasmABIMaxInputBytes  = 64 * 1024
	wasmABIMaxOutputBytes = 64 * 1024

	wasmABIErrorBadJSON         = "bad_json"
	wasmABIErrorUnknownField    = "unknown_field"
	wasmABIErrorOversizedInput  = "oversized_input"
	wasmABIErrorOversizedOutput = "oversized_output"
	wasmABIErrorSchemaInvalid   = "schema_invalid"
	wasmABIErrorABIMismatch     = "abi_mismatch"
	wasmABIErrorExportMissing   = "export_missing"
	wasmABIErrorImportDenied    = "import_denied"
	wasmABIErrorBadOutput       = "bad_output"
	wasmABIErrorTimeout         = "timeout"
	wasmABIErrorTrap            = "trap"
	wasmABIErrorMemoryExceeded  = "memory_exceeded"
)

type WASMABIRequest struct {
	ABI            string          `json:"abi"`
	ExtensionPoint string          `json:"extension_point"`
	PluginID       string          `json:"plugin_id,omitempty"`
	ArtifactID     string          `json:"artifact_id,omitempty"`
	RequestID      string          `json:"request_id,omitempty"`
	FailPolicy     string          `json:"fail_policy"`
	Config         json.RawMessage `json:"config,omitempty"`
	RuleContext    json.RawMessage `json:"rule_context,omitempty"`
	RouteContext   json.RawMessage `json:"route_context,omitempty"`
	Host           string          `json:"host,omitempty"`
	Source         string          `json:"source,omitempty"`
}

type WASMABIResponse struct {
	ABI            string               `json:"abi"`
	ExtensionPoint string               `json:"extension_point"`
	OK             bool                 `json:"ok"`
	Valid          *bool                `json:"valid,omitempty"`
	Decision       string               `json:"decision,omitempty"`
	Route          string               `json:"route,omitempty"`
	Reason         string               `json:"reason,omitempty"`
	Error          *WASMABIErrorPayload `json:"error,omitempty"`
}

type WASMABIErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type WASMABIError struct {
	Code           string
	Message        string
	ExtensionPoint string
	FailPolicy     string
}

func (e *WASMABIError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return "wasm ABI " + e.Code
	}
	return "wasm ABI " + e.Code + ": " + e.Message
}

func newWASMABIError(code, point, message string) *WASMABIError {
	return &WASMABIError{
		Code:           code,
		Message:        message,
		ExtensionPoint: point,
		FailPolicy:     wasmABIFailPolicy(point),
	}
}

func WASMHostABIVersion() string {
	return wasmHostABIV1
}

func WASMABIExportForExtension(point string) (string, bool) {
	switch point {
	case ExtensionConfigValidate:
		return wasmExportConfigValidateV1, true
	case ExtensionRuleEvaluate:
		return wasmExportRuleEvaluateV1, true
	case ExtensionRouteResolve:
		return wasmExportRouteResolveV1, true
	default:
		return "", false
	}
}

func wasmABIFailPolicy(point string) string {
	switch point {
	case ExtensionConfigValidate:
		return ExternalFailPolicyClosed
	case ExtensionRuleEvaluate:
		return ExternalFailPolicyClosed
	case ExtensionRouteResolve:
		return ExternalFailPolicyOpen
	default:
		return ExternalFailPolicyClosed
	}
}

func wasmRequiredExports(manifest Manifest) ([]string, error) {
	seen := map[string]bool{}
	var exports []string
	for _, point := range manifest.ExtensionPoints {
		name, ok := WASMABIExportForExtension(point.Key)
		if !ok {
			return nil, fmt.Errorf("wasm extension point %q is not supported by host ABI %s", point.Key, wasmHostABIV1)
		}
		if !seen[name] {
			seen[name] = true
			exports = append(exports, name)
		}
	}
	sort.Strings(exports)
	return exports, nil
}

func wasmDefaultExport(manifest Manifest) string {
	for _, point := range manifest.ExtensionPoints {
		if name, ok := WASMABIExportForExtension(point.Key); ok {
			return name
		}
	}
	return wasmExportConfigValidateV1
}

func validateWASMManifestABI(manifest Manifest) error {
	if manifest.Runtime.Type != RuntimeWASM {
		return nil
	}
	if manifest.Runtime.ABI != wasmHostABIV1 {
		return newWASMABIError(wasmABIErrorABIMismatch, "", fmt.Sprintf("runtime.abi %q does not match %q", manifest.Runtime.ABI, wasmHostABIV1))
	}
	if _, err := wasmRequiredExports(manifest); err != nil {
		return err
	}
	return nil
}

func validateWASMModuleABI(ctx context.Context, module []byte, manifest Manifest, memoryBytes int) error {
	if err := validateWASMManifestABI(manifest); err != nil {
		return err
	}
	if len(module) == 0 {
		return errors.New("wasm module is empty")
	}
	runtimeConfig := wazero.NewRuntimeConfig().WithMemoryLimitPages(wasmMemoryLimitPages(memoryBytes))
	runtime := wazero.NewRuntimeWithConfig(ctx, runtimeConfig)
	defer runtime.Close(context.Background())
	compiled, err := runtime.CompileModule(ctx, module)
	if err != nil {
		return err
	}
	defer compiled.Close(context.Background())
	return validateWASMCompiledABI(compiled, manifest)
}

func validateWASMCompiledABI(compiled wazero.CompiledModule, manifest Manifest) error {
	required, err := wasmRequiredExports(manifest)
	if err != nil {
		return err
	}
	exports := compiled.ExportedFunctions()
	for _, name := range required {
		def, ok := exports[name]
		if !ok {
			return newWASMABIError(wasmABIErrorExportMissing, "", fmt.Sprintf("required export %q not found", name))
		}
		if !sameValueTypes(def.ParamTypes(), []wazeroapi.ValueType{wazeroapi.ValueTypeI32, wazeroapi.ValueTypeI32}) || !sameValueTypes(def.ResultTypes(), []wazeroapi.ValueType{wazeroapi.ValueTypeI64}) {
			return newWASMABIError(wasmABIErrorABIMismatch, "", fmt.Sprintf("export %q must use (request_ptr_i32, request_len_i32) -> response_ptr_len_i64", name))
		}
	}
	for _, imported := range compiled.ImportedFunctions() {
		moduleName, name, ok := imported.Import()
		if !ok {
			continue
		}
		if moduleName != wasmHostImportModule {
			return newWASMABIError(wasmABIErrorImportDenied, "", fmt.Sprintf("import module %q is not allowed", moduleName))
		}
		switch name {
		case wasmHostImportLog:
			if !sameValueTypes(imported.ParamTypes(), []wazeroapi.ValueType{wazeroapi.ValueTypeI32, wazeroapi.ValueTypeI32, wazeroapi.ValueTypeI32}) || len(imported.ResultTypes()) != 1 || imported.ResultTypes()[0] != wazeroapi.ValueTypeI32 {
				return newWASMABIError(wasmABIErrorABIMismatch, "", "log import signature must be log(level_i32, message_ptr_i32, message_len_i32) -> status_i32")
			}
		case wasmHostImportMetric:
			if !sameValueTypes(imported.ParamTypes(), []wazeroapi.ValueType{wazeroapi.ValueTypeI32, wazeroapi.ValueTypeI32, wazeroapi.ValueTypeI32, wazeroapi.ValueTypeI32, wazeroapi.ValueTypeF64}) || len(imported.ResultTypes()) != 1 || imported.ResultTypes()[0] != wazeroapi.ValueTypeI32 {
				return newWASMABIError(wasmABIErrorABIMismatch, "", "metric import signature must be metric(name_ptr_i32, name_len_i32, labels_ptr_i32, labels_len_i32, value_f64) -> status_i32")
			}
		default:
			return newWASMABIError(wasmABIErrorImportDenied, "", fmt.Sprintf("host import %q is not allowed", name))
		}
	}
	return nil
}

func sameValueTypes(a, b []wazeroapi.ValueType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func instantiateWASMHostImports(ctx context.Context, runtime wazero.Runtime) error {
	_, err := runtime.NewHostModuleBuilder(wasmHostImportModule).
		NewFunctionBuilder().
		WithFunc(func(level, messagePtr, messageLen uint32) uint32 {
			if level > 4 || messageLen > wasmABIMaxInputBytes {
				return 1
			}
			return 0
		}).
		Export(wasmHostImportLog).
		NewFunctionBuilder().
		WithFunc(func(namePtr, nameLen, labelsPtr, labelsLen uint32, value float64) uint32 {
			if nameLen == 0 || nameLen > 128 || labelsLen > 1024 {
				return 1
			}
			return 0
		}).
		Export(wasmHostImportMetric).
		Instantiate(ctx)
	return err
}

func EncodeWASMABIRequest(req WASMABIRequest) ([]byte, error) {
	if req.ABI == "" {
		req.ABI = wasmHostABIV1
	}
	if req.FailPolicy == "" {
		req.FailPolicy = wasmABIFailPolicy(req.ExtensionPoint)
	}
	if err := validateWASMABIRequest(req.ExtensionPoint, &req); err != nil {
		return nil, err
	}
	return canonicalJSON(req, wasmABIMaxInputBytes, wasmABIErrorOversizedInput, req.ExtensionPoint)
}

func DecodeWASMABIRequest(point string, data []byte) (WASMABIRequest, []byte, error) {
	if len(data) > wasmABIMaxInputBytes {
		return WASMABIRequest{}, nil, newWASMABIError(wasmABIErrorOversizedInput, point, fmt.Sprintf("request size %d exceeds limit %d", len(data), wasmABIMaxInputBytes))
	}
	var req WASMABIRequest
	if err := decodeStrictJSON(data, &req); err != nil {
		return WASMABIRequest{}, nil, classifyWASMJSONError(err, point)
	}
	if err := validateWASMABIRequest(point, &req); err != nil {
		return WASMABIRequest{}, nil, err
	}
	canonical, err := canonicalJSON(req, wasmABIMaxInputBytes, wasmABIErrorOversizedInput, point)
	return req, canonical, err
}

func EncodeWASMABIResponse(resp WASMABIResponse) ([]byte, error) {
	if resp.ABI == "" {
		resp.ABI = wasmHostABIV1
	}
	if err := validateWASMABIResponse(resp.ExtensionPoint, &resp); err != nil {
		return nil, err
	}
	return canonicalJSON(resp, wasmABIMaxOutputBytes, wasmABIErrorOversizedOutput, resp.ExtensionPoint)
}

func DecodeWASMABIResponse(point string, data []byte) (WASMABIResponse, []byte, error) {
	if len(data) > wasmABIMaxOutputBytes {
		return WASMABIResponse{}, nil, newWASMABIError(wasmABIErrorOversizedOutput, point, fmt.Sprintf("response size %d exceeds limit %d", len(data), wasmABIMaxOutputBytes))
	}
	var resp WASMABIResponse
	if err := decodeStrictJSON(data, &resp); err != nil {
		return WASMABIResponse{}, nil, classifyWASMJSONError(err, point)
	}
	if err := validateWASMABIResponse(point, &resp); err != nil {
		return WASMABIResponse{}, nil, err
	}
	canonical, err := canonicalJSON(resp, wasmABIMaxOutputBytes, wasmABIErrorOversizedOutput, point)
	return resp, canonical, err
}

func validateWASMABIRequest(point string, req *WASMABIRequest) error {
	if req.ABI != wasmHostABIV1 {
		return newWASMABIError(wasmABIErrorABIMismatch, point, fmt.Sprintf("request abi %q does not match %q", req.ABI, wasmHostABIV1))
	}
	if point != "" && req.ExtensionPoint != point {
		return newWASMABIError(wasmABIErrorSchemaInvalid, point, fmt.Sprintf("request extension_point %q does not match %q", req.ExtensionPoint, point))
	}
	if _, ok := WASMABIExportForExtension(req.ExtensionPoint); !ok {
		return newWASMABIError(wasmABIErrorSchemaInvalid, point, fmt.Sprintf("unsupported extension_point %q", req.ExtensionPoint))
	}
	if req.FailPolicy == "" {
		req.FailPolicy = wasmABIFailPolicy(req.ExtensionPoint)
	}
	if !validWASMFailPolicy(req.FailPolicy) {
		return newWASMABIError(wasmABIErrorSchemaInvalid, req.ExtensionPoint, fmt.Sprintf("invalid fail_policy %q", req.FailPolicy))
	}
	switch req.ExtensionPoint {
	case ExtensionConfigValidate:
		if err := validateRawJSONField("config", req.Config, true); err != nil {
			return newWASMABIError(wasmABIErrorSchemaInvalid, req.ExtensionPoint, err.Error())
		}
	case ExtensionRuleEvaluate:
		if err := validateRawJSONField("rule_context", req.RuleContext, true); err != nil {
			return newWASMABIError(wasmABIErrorSchemaInvalid, req.ExtensionPoint, err.Error())
		}
	case ExtensionRouteResolve:
		if strings.TrimSpace(req.Host) == "" {
			return newWASMABIError(wasmABIErrorSchemaInvalid, req.ExtensionPoint, "host is required")
		}
		if err := validateRawJSONField("route_context", req.RouteContext, true); err != nil {
			return newWASMABIError(wasmABIErrorSchemaInvalid, req.ExtensionPoint, err.Error())
		}
	}
	return nil
}

func validateWASMABIResponse(point string, resp *WASMABIResponse) error {
	if resp.ABI != wasmHostABIV1 {
		return newWASMABIError(wasmABIErrorABIMismatch, point, fmt.Sprintf("response abi %q does not match %q", resp.ABI, wasmHostABIV1))
	}
	if point != "" && resp.ExtensionPoint != point {
		return newWASMABIError(wasmABIErrorBadOutput, point, fmt.Sprintf("response extension_point %q does not match %q", resp.ExtensionPoint, point))
	}
	if _, ok := WASMABIExportForExtension(resp.ExtensionPoint); !ok {
		return newWASMABIError(wasmABIErrorBadOutput, point, fmt.Sprintf("unsupported extension_point %q", resp.ExtensionPoint))
	}
	if resp.Error != nil {
		if !knownWASMABIErrorCode(resp.Error.Code) {
			return newWASMABIError(wasmABIErrorBadOutput, resp.ExtensionPoint, fmt.Sprintf("unknown error code %q", resp.Error.Code))
		}
		return nil
	}
	switch resp.ExtensionPoint {
	case ExtensionConfigValidate:
		if resp.Valid == nil {
			return newWASMABIError(wasmABIErrorBadOutput, resp.ExtensionPoint, "valid is required")
		}
	case ExtensionRuleEvaluate:
		if resp.Decision != "allow" && resp.Decision != "deny" {
			return newWASMABIError(wasmABIErrorBadOutput, resp.ExtensionPoint, "decision must be allow or deny")
		}
	case ExtensionRouteResolve:
		switch resp.Decision {
		case "use_default", "reject":
		case "override":
			if strings.TrimSpace(resp.Route) == "" {
				return newWASMABIError(wasmABIErrorBadOutput, resp.ExtensionPoint, "route is required for override")
			}
		default:
			return newWASMABIError(wasmABIErrorBadOutput, resp.ExtensionPoint, "decision must be use_default, override, or reject")
		}
	}
	return nil
}

func validWASMFailPolicy(policy string) bool {
	switch policy {
	case ExternalFailPolicyOpen, ExternalFailPolicyClosed:
		return true
	default:
		return false
	}
}

func validateRawJSONField(name string, raw json.RawMessage, required bool) error {
	if len(raw) == 0 {
		if required {
			return fmt.Errorf("%s is required", name)
		}
		return nil
	}
	if !json.Valid(raw) {
		return fmt.Errorf("%s must be valid JSON", name)
	}
	return nil
}

func decodeStrictJSON(data []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("multiple JSON values are not allowed")
	}
	return nil
}

func classifyWASMJSONError(err error, point string) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "unknown field") {
		return newWASMABIError(wasmABIErrorUnknownField, point, err.Error())
	}
	return newWASMABIError(wasmABIErrorBadJSON, point, err.Error())
}

func canonicalJSON(v any, limit int, oversizedCode, point string) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	data, err = json.Marshal(decoded)
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, newWASMABIError(oversizedCode, point, fmt.Sprintf("canonical JSON size %d exceeds limit %d", len(data), limit))
	}
	return data, nil
}

func knownWASMABIErrorCode(code string) bool {
	switch code {
	case wasmABIErrorBadJSON,
		wasmABIErrorUnknownField,
		wasmABIErrorOversizedInput,
		wasmABIErrorOversizedOutput,
		wasmABIErrorSchemaInvalid,
		wasmABIErrorABIMismatch,
		wasmABIErrorExportMissing,
		wasmABIErrorImportDenied,
		wasmABIErrorBadOutput,
		wasmABIErrorTimeout,
		wasmABIErrorTrap,
		wasmABIErrorMemoryExceeded:
		return true
	default:
		return false
	}
}

func mapWASMInvocationError(point string, err error) error {
	if err == nil {
		return nil
	}
	var abiErr *WASMABIError
	if errors.As(err, &abiErr) {
		return err
	}
	message := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(message, "context deadline exceeded"):
		return newWASMABIError(wasmABIErrorTimeout, point, message)
	case strings.Contains(message, "memory") && (strings.Contains(message, "limit") || strings.Contains(message, "out of memory")):
		return newWASMABIError(wasmABIErrorMemoryExceeded, point, message)
	case strings.Contains(message, "unreachable") || strings.Contains(message, "wasm error") || strings.Contains(message, "trap"):
		return newWASMABIError(wasmABIErrorTrap, point, message)
	default:
		return newWASMABIError(wasmABIErrorBadOutput, point, message)
	}
}

func wasmABIErrorCode(err error) string {
	var abiErr *WASMABIError
	if errors.As(err, &abiErr) {
		return abiErr.Code
	}
	return ""
}
