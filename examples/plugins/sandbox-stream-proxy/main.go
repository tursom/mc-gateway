package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/tursom/mc-gateway/plugin/api"
	"github.com/tursom/mc-gateway/plugin/sandboxsdk"
)

func main() {
	client, err := sandboxsdk.NewFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	service := sandboxsdk.Service{
		Registrations: []sandboxsdk.HandlerRegistration{
			{
				ExtensionPoint: "upstream.connect/v1",
				HandlerID:      "stream-main",
				FailPolicy:     api.FailPolicyClose,
				TimeoutMS:      1000,
				SchemaVersion:  1,
			},
			{
				ExtensionPoint: "config.validate/v1",
				HandlerID:      "config-main",
				FailPolicy:     api.FailPolicyClose,
				TimeoutMS:      1000,
				SchemaVersion:  1,
			},
		},
		Capabilities: []string{"stream.proxy/v1", "runtime.cpu_memory", "runtime.process_restricted"},
		ConfigValidate: func(_ context.Context, raw json.RawMessage) (bool, string, error) {
			if len(raw) == 0 || json.Valid(raw) {
				return true, "stream config accepted", nil
			}
			return false, "config must be valid JSON", nil
		},
		StreamOpen: func(ctx context.Context, req sandboxsdk.StreamOpenRequest) (sandboxsdk.StreamOpenResponse, error) {
			endpoint := filepath.Join(os.TempDir(), req.StreamID+".sock")
			_ = os.Remove(endpoint)
			listener, err := net.Listen("unix", endpoint)
			if err != nil {
				return sandboxsdk.StreamOpenResponse{}, err
			}
			go func() {
				defer listener.Close()
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				done := make(chan struct{})
				go func() {
					_, _ = io.Copy(conn, conn)
					close(done)
				}()
				select {
				case <-ctx.Done():
				case <-done:
				}
			}()
			return sandboxsdk.StreamOpenResponse{
				Connected:    true,
				Protocol:     req.Protocol,
				StreamID:     req.StreamID,
				Endpoint:     endpoint,
				EndpointType: "unix",
			}, nil
		},
		StreamClose: func(_ context.Context, req sandboxsdk.StreamCloseRequest) error {
			if req.StreamID != "" {
				_ = os.Remove(filepath.Join(os.TempDir(), req.StreamID+".sock"))
			}
			return nil
		},
	}
	if err := client.Run(ctx, service); err != nil {
		log.Fatal(err)
	}
}
