package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

type exportResponse struct {
	name     string
	offset   uint32
	response string
}

func main() {
	out := flag.String("out", "plugin.wasm", "output wasm path")
	flag.Parse()
	module := wasmModule([]exportResponse{
		{
			name:     "mcgw_config_validate_v1",
			offset:   4096,
			response: `{"abi":"mc-gateway.wasm.host/v1","extension_point":"config.validate/v1","ok":true,"valid":true}`,
		},
		{
			name:     "mcgw_rule_evaluate_v1",
			offset:   8192,
			response: `{"abi":"mc-gateway.wasm.host/v1","extension_point":"rule.evaluate/v1","ok":true,"decision":"deny","reason":"blocked by wasm rule policy"}`,
		},
		{
			name:     "mcgw_route_resolve_v1",
			offset:   12288,
			response: `{"abi":"mc-gateway.wasm.host/v1","extension_point":"route.resolve/v1","ok":true,"decision":"override","route":"wasm-policy:25565","reason":"routed by wasm policy"}`,
		},
	})
	if dir := filepath.Dir(*out); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			fatal(err)
		}
	}
	if err := os.WriteFile(*out, module, 0644); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func wasmModule(exports []exportResponse) []byte {
	module := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	module = append(module, wasmSection(1, []byte{0x01, 0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7e})...)
	functions := appendU32LEB(nil, uint32(len(exports)))
	for range exports {
		functions = append(functions, 0x00)
	}
	module = append(module, wasmSection(3, functions)...)
	module = append(module, wasmSection(5, []byte{0x01, 0x00, 0x01})...)
	exportPayload := appendU32LEB(nil, uint32(len(exports)+1))
	for index, item := range exports {
		exportPayload = appendU32LEB(exportPayload, uint32(len(item.name)))
		exportPayload = append(exportPayload, item.name...)
		exportPayload = append(exportPayload, 0x00)
		exportPayload = appendU32LEB(exportPayload, uint32(index))
	}
	exportPayload = appendU32LEB(exportPayload, uint32(len("memory")))
	exportPayload = append(exportPayload, "memory"...)
	exportPayload = append(exportPayload, 0x02, 0x00)
	module = append(module, wasmSection(7, exportPayload)...)
	code := appendU32LEB(nil, uint32(len(exports)))
	for _, item := range exports {
		responseLen := uint32(len([]byte(item.response)))
		result := int64(uint64(item.offset)<<32 | uint64(responseLen))
		body := []byte{0x00, 0x42}
		body = appendI64LEB(body, result)
		body = append(body, 0x0b)
		code = appendU32LEB(code, uint32(len(body)))
		code = append(code, body...)
	}
	module = append(module, wasmSection(10, code)...)
	data := appendU32LEB(nil, uint32(len(exports)))
	for _, item := range exports {
		responseBytes := []byte(item.response)
		data = append(data, 0x00, 0x41)
		data = appendI32LEB(data, int32(item.offset))
		data = append(data, 0x0b)
		data = appendU32LEB(data, uint32(len(responseBytes)))
		data = append(data, responseBytes...)
	}
	module = append(module, wasmSection(11, data)...)
	return module
}

func wasmSection(id byte, payload []byte) []byte {
	section := []byte{id}
	section = appendU32LEB(section, uint32(len(payload)))
	section = append(section, payload...)
	return section
}

func appendU32LEB(out []byte, value uint32) []byte {
	for {
		b := byte(value & 0x7f)
		value >>= 7
		if value != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if value == 0 {
			return out
		}
	}
}

func appendI32LEB(out []byte, value int32) []byte {
	for {
		b := byte(value & 0x7f)
		value >>= 7
		done := (value == 0 && b&0x40 == 0) || (value == -1 && b&0x40 != 0)
		if !done {
			b |= 0x80
		}
		out = append(out, b)
		if done {
			return out
		}
	}
}

func appendI64LEB(out []byte, value int64) []byte {
	for {
		b := byte(value & 0x7f)
		value >>= 7
		done := (value == 0 && b&0x40 == 0) || (value == -1 && b&0x40 != 0)
		if !done {
			b |= 0x80
		}
		out = append(out, b)
		if done {
			return out
		}
	}
}
