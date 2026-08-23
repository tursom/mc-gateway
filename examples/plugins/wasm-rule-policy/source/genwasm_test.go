package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tetratelabs/wazero"
)

func TestWASMModuleExportsExecutablePolicyResponses(t *testing.T) {
	exports := []exportResponse{
		{name: "mcgw_config_validate_v1", offset: 4096, response: `{"ok":true,"valid":true}`},
		{name: "mcgw_rule_evaluate_v1", offset: 8192, response: `{"ok":true,"decision":"deny"}`},
		{name: "mcgw_route_resolve_v1", offset: 12288, response: `{"ok":true,"route":"wasm-policy:25565"}`},
	}

	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	t.Cleanup(func() { _ = runtime.Close(ctx) })
	module, err := runtime.Instantiate(ctx, wasmModule(exports))
	if err != nil {
		t.Fatalf("Instantiate(generated module) error = %v", err)
	}

	for _, want := range exports {
		t.Run(want.name, func(t *testing.T) {
			function := module.ExportedFunction(want.name)
			if function == nil {
				t.Fatalf("generated module does not export %q", want.name)
			}
			results, err := function.Call(ctx, 0, 0)
			if err != nil {
				t.Fatalf("Call(%s) error = %v", want.name, err)
			}
			if len(results) != 1 {
				t.Fatalf("Call(%s) results = %v, want one packed pointer", want.name, results)
			}
			offset := uint32(results[0] >> 32)
			length := uint32(results[0])
			response, ok := module.Memory().Read(offset, length)
			if !ok {
				t.Fatalf("Read(%d, %d) failed", offset, length)
			}
			if string(response) != want.response || !json.Valid(response) {
				t.Fatalf("response = %q, want valid JSON %q", response, want.response)
			}
		})
	}
}
