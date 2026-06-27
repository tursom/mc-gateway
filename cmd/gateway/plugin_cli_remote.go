package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tursom/mc-gateway/internal/pluginmanager"
)

type pluginRemoteOptions struct {
	Gateway             string
	Token               string
	Target              string
	Extra               []string
	ArtifactID          string
	ConfigPath          string
	ConfigJSON          string
	Priority            int
	Source              bool
	SnapshotID          int64
	FullDesired         bool
	Profile             string
	Action              string
	Decision            string
	Notes               string
	Reason              string
	TTLSeconds          int64
	ConfirmToken        string
	DryRun              bool
	RepositoryType      string
	IndexPath           string
	Version             string
	TrustPolicy         string
	MetadataPath        string
	MetadataJSON        string
	BenchmarkProfile    string
	P95MS               float64
	P99MS               float64
	ErrorRate           float64
	ActiveProxyCapacity int64
	BaselineDiff        float64
	Mode                string
}

type pluginRemoteClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func runPluginRemoteStatusCLI(args []string) error {
	opts, err := parsePluginRemoteOptions(args)
	if err != nil {
		return err
	}
	client, err := newPluginRemoteClient(opts)
	if err != nil {
		return err
	}
	endpoint := "/plugins"
	if opts.Target != "" {
		endpoint = "/plugins/" + url.PathEscape(opts.Target)
	}
	return client.doToStdout(http.MethodGet, endpoint, nil)
}

func runPluginRemoteUploadCLI(args []string) error {
	opts, err := parsePluginRemoteOptions(args)
	if err != nil {
		return err
	}
	if opts.Target == "" {
		return errors.New("upload requires an artifact path")
	}
	client, err := newPluginRemoteClient(opts)
	if err != nil {
		return err
	}
	endpoint := "/plugin-artifacts"
	if opts.Source {
		endpoint = "/plugin-sources"
	}
	return client.uploadArtifact(endpoint, opts.Target)
}

func runPluginRemoteDesiredCLI(args []string, desiredState string) error {
	opts, err := parsePluginRemoteOptions(args)
	if err != nil {
		return err
	}
	if opts.Target == "" {
		return errors.New("enable requires a plugin id")
	}
	if opts.ArtifactID == "" {
		return errors.New("enable requires --artifact")
	}
	client, err := newPluginRemoteClient(opts)
	if err != nil {
		return err
	}
	configJSON, err := remoteConfigJSON(opts)
	if err != nil {
		return err
	}
	body := map[string]any{
		"artifact_id":    opts.ArtifactID,
		"desired_state":  desiredState,
		"config_json":    configJSON,
		"priority":       opts.Priority,
		"source":         "cli",
		"requested_mode": "desired",
	}
	return client.doToStdout(http.MethodPut, "/plugins/"+url.PathEscape(opts.Target), body)
}

func runPluginRemoteActionCLI(args []string, action string) error {
	opts, err := parsePluginRemoteOptions(args)
	if err != nil {
		return err
	}
	if opts.Target == "" {
		return fmt.Errorf("%s requires a plugin id", action)
	}
	client, err := newPluginRemoteClient(opts)
	if err != nil {
		return err
	}
	return client.doToStdout(http.MethodPost, "/plugins/"+url.PathEscape(opts.Target)+"/"+action, nil)
}

func runPluginRemoteRollbackCLI(args []string) error {
	opts, err := parsePluginRemoteOptions(args)
	if err != nil {
		return err
	}
	if opts.Target == "" {
		return errors.New("rollback requires a plugin id")
	}
	client, err := newPluginRemoteClient(opts)
	if err != nil {
		return err
	}
	if opts.SnapshotID > 0 {
		body := map[string]any{"snapshot_id": opts.SnapshotID, "full_desired": opts.FullDesired}
		return client.doToStdout(http.MethodPost, "/plugins/"+url.PathEscape(opts.Target)+"/rollback/config", body)
	}
	if opts.ArtifactID == "" {
		return errors.New("rollback requires --artifact or --snapshot")
	}
	return client.doToStdout(http.MethodPost, "/plugins/"+url.PathEscape(opts.Target)+"/rollback/artifact", map[string]any{"artifact_id": opts.ArtifactID})
}

func runPluginRemoteConfigCLI(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gateway plugin config validate <plugin-id> ...")
	}
	switch args[0] {
	case "validate":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		if opts.Target == "" {
			return errors.New("config validate requires a plugin id")
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		configJSON, err := remoteConfigJSON(opts)
		if err != nil {
			return err
		}
		body := map[string]any{"artifact_id": opts.ArtifactID, "config_json": configJSON}
		if opts.Priority != 0 {
			body["priority"] = opts.Priority
		}
		return client.doToStdout(http.MethodPost, "/plugins/"+url.PathEscape(opts.Target)+"/config/dry-run", body)
	default:
		return fmt.Errorf("unknown config command %q", args[0])
	}
}

func runPluginRemoteSecretCLI(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gateway plugin secret check <plugin-id> ...")
	}
	switch args[0] {
	case "check":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		if opts.Target == "" {
			return errors.New("secret check requires a plugin id")
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		return client.doToStdout(http.MethodGet, "/plugins/"+url.PathEscape(opts.Target)+"/secrets", nil)
	default:
		return fmt.Errorf("unknown secret command %q", args[0])
	}
}

func runPluginRemoteOperationsSectionCLI(command string, args []string) error {
	opts, err := parsePluginRemoteOptions(args)
	if err != nil {
		return err
	}
	if opts.Target == "" {
		return fmt.Errorf("%s requires a plugin id", command)
	}
	client, err := newPluginRemoteClient(opts)
	if err != nil {
		return err
	}
	body, err := client.doJSON(http.MethodGet, "/plugins/"+url.PathEscape(opts.Target)+"/operations", nil)
	if err != nil {
		return err
	}
	operations, _ := body["operations"].(map[string]any)
	result := map[string]any{"plugin_id": opts.Target}
	switch command {
	case "logs":
		result["logs"] = operations["logs"]
		result["traces"] = operations["traces"]
	case "events":
		result["events"] = operations["events"]
		result["event_queue"] = operations["event_queue"]
	case "metrics":
		result["handlers"] = operations["handlers"]
		result["custom_metrics"] = operations["custom_metrics"]
	default:
		return fmt.Errorf("unknown operations section %q", command)
	}
	return encodePluginCLIJSON(result)
}

func runPluginRemoteDiagnoseCLI(args []string) error {
	opts, err := parsePluginRemoteOptions(args)
	if err != nil {
		return err
	}
	if opts.Target == "" {
		return errors.New("diagnose requires a plugin id")
	}
	client, err := newPluginRemoteClient(opts)
	if err != nil {
		return err
	}
	return client.doToStdout(http.MethodGet, "/plugins/"+url.PathEscape(opts.Target)+"/diagnostics", nil)
}

func runPluginRemoteTaskCLI(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gateway plugin task list|run|cancel ...")
	}
	switch args[0] {
	case "list":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		if opts.Target == "" {
			return errors.New("task list requires a plugin id")
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		body, err := client.doJSON(http.MethodGet, "/plugins/"+url.PathEscape(opts.Target)+"/operations", nil)
		if err != nil {
			return err
		}
		operations, _ := body["operations"].(map[string]any)
		return encodePluginCLIJSON(map[string]any{
			"plugin_id":        opts.Target,
			"background_tasks": operations["background_tasks"],
		})
	case "run":
		opts, err := parsePluginRemoteOptionsWithPositionals(args[1:], 2)
		if err != nil {
			return err
		}
		if opts.Target == "" || len(opts.Extra) == 0 {
			return errors.New("task run requires a plugin id and task id")
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		taskID := opts.Extra[0]
		body := map[string]any{"confirm_token": opts.ConfirmToken}
		return client.doToStdout(http.MethodPost, "/plugins/"+url.PathEscape(opts.Target)+"/operations/tasks/"+url.PathEscape(taskID)+"/trigger", body)
	case "cancel":
		return runPluginReservedCLI("task cancel", args[1:])
	default:
		return fmt.Errorf("unknown task command %q", args[0])
	}
}

func runPluginRemoteResourceCLI(command string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: gateway plugin %s inspect|gc ...", command)
	}
	switch args[0] {
	case "inspect":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		if opts.Target == "" {
			return fmt.Errorf("%s inspect requires a plugin id", command)
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		body, err := client.doJSON(http.MethodGet, "/plugins/"+url.PathEscape(opts.Target)+"/operations", nil)
		if err != nil {
			return err
		}
		operations, _ := body["operations"].(map[string]any)
		field := "plugin_data"
		if command == "files" {
			field = "plugin_files"
		}
		return encodePluginCLIJSON(map[string]any{
			"plugin_id": opts.Target,
			field:       operations[field],
			"gc":        operations["gc"],
		})
	case "gc":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		if opts.Target == "" {
			return fmt.Errorf("%s gc requires a plugin id", command)
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		method := http.MethodGet
		if !opts.DryRun {
			method = http.MethodPost
		}
		return client.doToStdout(method, "/plugins/"+url.PathEscape(opts.Target)+"/operations/gc", nil)
	case "export":
		return runPluginReservedCLI(command+" export", args[1:])
	default:
		return fmt.Errorf("unknown %s command %q", command, args[0])
	}
}

func runPluginRemoteGCCLI(args []string) error {
	opts, err := parsePluginRemoteOptions(args)
	if err != nil {
		return err
	}
	client, err := newPluginRemoteClient(opts)
	if err != nil {
		return err
	}
	method := http.MethodGet
	if !opts.DryRun {
		method = http.MethodPost
	}
	endpoint := "/plugin-gc"
	if opts.Target != "" {
		endpoint = "/plugin-operations-gc?plugin_id=" + url.QueryEscape(opts.Target)
	}
	return client.doToStdout(method, endpoint, nil)
}

func runPluginRemoteReviewCLI(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gateway plugin review status|approve|reject|override ...")
	}
	switch args[0] {
	case "status":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		if opts.Target == "" {
			return errors.New("review status requires a plugin id")
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		values := url.Values{}
		if opts.ArtifactID != "" {
			values.Set("artifact_id", opts.ArtifactID)
		}
		if opts.Profile != "" {
			values.Set("profile", opts.Profile)
		}
		endpoint := "/plugins/" + url.PathEscape(opts.Target) + "/governance"
		if query := values.Encode(); query != "" {
			endpoint += "?" + query
		}
		return client.doToStdout(http.MethodGet, endpoint, nil)
	case "approve", "reject":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		if opts.Target == "" {
			return fmt.Errorf("review %s requires a plugin id", args[0])
		}
		if opts.ArtifactID == "" {
			return fmt.Errorf("review %s requires --artifact", args[0])
		}
		decision := pluginmanager.ReviewDecisionApproved
		if args[0] == "reject" {
			decision = pluginmanager.ReviewDecisionRejected
		}
		if opts.Decision != "" {
			decision = opts.Decision
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		body := map[string]any{
			"artifact_id": opts.ArtifactID,
			"profile":     opts.Profile,
			"decision":    decision,
			"notes":       opts.Notes,
		}
		return client.doToStdout(http.MethodPost, "/plugins/"+url.PathEscape(opts.Target)+"/governance/review", body)
	case "override":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		if opts.Target == "" || opts.ArtifactID == "" {
			return errors.New("review override requires a plugin id and --artifact")
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		body := map[string]any{
			"artifact_id": opts.ArtifactID,
			"profile":     opts.Profile,
			"action":      opts.Action,
			"reason":      opts.Reason,
			"ttl_seconds": opts.TTLSeconds,
		}
		return client.doToStdout(http.MethodPost, "/plugins/"+url.PathEscape(opts.Target)+"/governance/override", body)
	default:
		return fmt.Errorf("unknown review command %q", args[0])
	}
}

func runPluginRemoteAdvisoryCLI(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gateway plugin advisory scan|import ...")
	}
	switch args[0] {
	case "scan":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		endpoint := "/plugin-advisories"
		if opts.Target != "" {
			endpoint += "?plugin_id=" + url.QueryEscape(opts.Target)
		}
		return client.doToStdout(http.MethodGet, endpoint, nil)
	case "import":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		body, err := remoteMetadataJSON(opts)
		if err != nil {
			return err
		}
		return client.doToStdout(http.MethodPost, "/plugin-advisories", body)
	default:
		return fmt.Errorf("unknown advisory command %q", args[0])
	}
}

func runPluginRemoteRepoCLI(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gateway plugin repo list|import|search|show ...")
	}
	switch args[0] {
	case "list":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		return client.doToStdout(http.MethodGet, "/plugin-repositories/imports", nil)
	case "import":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		body := map[string]any{
			"repository_type": opts.RepositoryType,
			"index_path":      opts.IndexPath,
			"artifact_id":     opts.ArtifactID,
			"plugin_id":       opts.Target,
			"version":         opts.Version,
			"trust_policy":    opts.TrustPolicy,
		}
		return client.doToStdout(http.MethodPost, "/plugin-repositories/imports", body)
	case "search", "show":
		return runPluginReservedCLI("repo "+args[0], args[1:])
	default:
		return fmt.Errorf("unknown repo command %q", args[0])
	}
}

func runPluginRemoteSupplyChainCLI(command string, args []string) error {
	if command == "sbom" {
		if len(args) == 0 {
			return errors.New("usage: gateway plugin sbom verify|generate ...")
		}
		switch args[0] {
		case "verify":
			return runPluginRemoteSupplyChainAssessCLI(args[1:])
		case "generate":
			return runPluginReservedCLI("sbom generate", args[1:])
		default:
			return fmt.Errorf("unknown sbom command %q", args[0])
		}
	}
	return runPluginRemoteSupplyChainAssessCLI(args)
}

func runPluginRemoteSupplyChainAssessCLI(args []string) error {
	opts, err := parsePluginRemoteOptions(args)
	if err != nil {
		return err
	}
	if opts.Target == "" || opts.ArtifactID == "" {
		return errors.New("supply-chain verification requires a plugin id and --artifact")
	}
	client, err := newPluginRemoteClient(opts)
	if err != nil {
		return err
	}
	metadata, err := remoteMetadataJSON(opts)
	if err != nil {
		return err
	}
	body := map[string]any{
		"plugin_id":   opts.Target,
		"artifact_id": opts.ArtifactID,
		"metadata":    metadata,
	}
	return client.doToStdout(http.MethodPost, "/plugin-supply-chain", body)
}

func runPluginRuntimeCLI(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gateway plugin runtime features|status|mode|apply ...")
	}
	switch args[0] {
	case "features":
		return runPluginFeaturesCLI(nil)
	case "status":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		return client.doToStdout(http.MethodGet, "/plugin-service", nil)
	case "mode":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		if opts.Mode == "" {
			return errors.New("runtime mode requires --mode")
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		return client.doToStdout(http.MethodPut, "/plugin-service", map[string]any{"desired_mode": opts.Mode})
	case "apply":
		opts, err := parsePluginRemoteOptions(args[1:])
		if err != nil {
			return err
		}
		client, err := newPluginRemoteClient(opts)
		if err != nil {
			return err
		}
		return client.doToStdout(http.MethodPost, "/plugin-service", nil)
	default:
		return fmt.Errorf("unknown runtime command %q", args[0])
	}
}

func runPluginReservedCLI(command string, args []string) error {
	_ = args
	return encodePluginCLIJSON(map[string]any{
		"command": command,
		"status":  "reserved",
		"message": "command is reserved by the plugin toolchain design but is not implemented in this gateway yet",
	})
}

func parsePluginRemoteOptions(args []string) (pluginRemoteOptions, error) {
	return parsePluginRemoteOptionsWithPositionals(args, 1)
}

func parsePluginRemoteOptionsWithPositionals(args []string, maxPositionals int) (pluginRemoteOptions, error) {
	opts := pluginRemoteOptions{
		Gateway:  os.Getenv("MC_GATEWAY_ADMIN_URL"),
		Token:    os.Getenv("MC_GATEWAY_ADMIN_TOKEN"),
		Priority: pluginmanager.DefaultPriority,
		DryRun:   true,
	}
	var positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			if len(positionals) >= maxPositionals {
				return pluginRemoteOptions{}, fmt.Errorf("unexpected argument %q", arg)
			}
			positionals = append(positionals, arg)
			continue
		}
		key, value, consumed, err := parsePluginCLIFlag(args, i)
		if err != nil {
			return pluginRemoteOptions{}, err
		}
		i += consumed
		switch key {
		case "gateway":
			opts.Gateway = value
		case "token":
			opts.Token = value
		case "artifact":
			opts.ArtifactID = value
		case "config":
			opts.ConfigPath = value
		case "config-json":
			opts.ConfigJSON = value
		case "priority":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return pluginRemoteOptions{}, fmt.Errorf("invalid --priority %q: %w", value, err)
			}
			opts.Priority = parsed
		case "source":
			opts.Source = parsePluginBoolFlag(value)
		case "snapshot":
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return pluginRemoteOptions{}, fmt.Errorf("invalid --snapshot %q: %w", value, err)
			}
			opts.SnapshotID = parsed
		case "full-desired":
			opts.FullDesired = parsePluginBoolFlag(value)
		case "profile":
			opts.Profile = value
		case "action":
			opts.Action = value
		case "decision":
			opts.Decision = value
		case "notes":
			opts.Notes = value
		case "reason":
			opts.Reason = value
		case "ttl":
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return pluginRemoteOptions{}, fmt.Errorf("invalid --ttl %q: %w", value, err)
			}
			opts.TTLSeconds = parsed
		case "confirm-token":
			opts.ConfirmToken = value
		case "dry-run":
			opts.DryRun = parsePluginBoolFlag(value)
		case "repository-type":
			opts.RepositoryType = value
		case "index":
			opts.IndexPath = value
		case "version":
			opts.Version = value
		case "trust-policy":
			opts.TrustPolicy = value
		case "metadata":
			opts.MetadataPath = value
		case "metadata-json":
			opts.MetadataJSON = value
		case "benchmark-profile":
			opts.BenchmarkProfile = value
		case "p95-ms":
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return pluginRemoteOptions{}, fmt.Errorf("invalid --p95-ms %q: %w", value, err)
			}
			opts.P95MS = parsed
		case "p99-ms":
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return pluginRemoteOptions{}, fmt.Errorf("invalid --p99-ms %q: %w", value, err)
			}
			opts.P99MS = parsed
		case "error-rate":
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return pluginRemoteOptions{}, fmt.Errorf("invalid --error-rate %q: %w", value, err)
			}
			opts.ErrorRate = parsed
		case "active-proxy-capacity":
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return pluginRemoteOptions{}, fmt.Errorf("invalid --active-proxy-capacity %q: %w", value, err)
			}
			opts.ActiveProxyCapacity = parsed
		case "baseline-diff":
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return pluginRemoteOptions{}, fmt.Errorf("invalid --baseline-diff %q: %w", value, err)
			}
			opts.BaselineDiff = parsed
		case "mode":
			opts.Mode = value
		default:
			return pluginRemoteOptions{}, fmt.Errorf("unknown remote flag --%s", key)
		}
	}
	if len(positionals) > 0 {
		opts.Target = positionals[0]
	}
	if len(positionals) > 1 {
		opts.Extra = append(opts.Extra, positionals[1:]...)
	}
	return opts, nil
}

func newPluginRemoteClient(opts pluginRemoteOptions) (pluginRemoteClient, error) {
	if strings.TrimSpace(opts.Gateway) == "" {
		return pluginRemoteClient{}, errors.New("--gateway or MC_GATEWAY_ADMIN_URL is required")
	}
	if strings.TrimSpace(opts.Token) == "" {
		return pluginRemoteClient{}, errors.New("--token or MC_GATEWAY_ADMIN_TOKEN is required")
	}
	base, err := normalizeAdminAPIBase(opts.Gateway)
	if err != nil {
		return pluginRemoteClient{}, err
	}
	return pluginRemoteClient{baseURL: base, token: opts.Token, client: http.DefaultClient}, nil
}

func normalizeAdminAPIBase(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid gateway URL %q", raw)
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	switch {
	case parsed.Path == "":
		parsed.Path = "/admin/api"
	case strings.HasSuffix(parsed.Path, "/admin/api"):
	case strings.HasSuffix(parsed.Path, "/admin"):
		parsed.Path = parsed.Path + "/api"
	default:
		parsed.Path = path.Join(parsed.Path, "admin/api")
	}
	return parsed.String(), nil
}

func (c pluginRemoteClient) doToStdout(method, endpoint string, body any) error {
	data, err := c.doBytes(method, endpoint, body)
	if err != nil {
		return err
	}
	return writePluginRemoteData(data)
}

func (c pluginRemoteClient) doJSON(method, endpoint string, body any) (map[string]any, error) {
	data, err := c.doBytes(method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("admin API response is not a JSON object: %w", err)
	}
	return decoded, nil
}

func (c pluginRemoteClient) doBytes(method, endpoint string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.baseURL+endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return readPluginRemoteResponse(resp)
}

func (c pluginRemoteClient) uploadArtifact(endpoint, filePath string) error {
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	part, err := writer.CreateFormFile("artifact", filepath.Base(filePath))
	if err != nil {
		return err
	}
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, file); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+endpoint, &payload)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := readPluginRemoteResponse(resp)
	if err != nil {
		return err
	}
	return writePluginRemoteData(data)
}

func readPluginRemoteResponse(resp *http.Response) ([]byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(data))
		if message == "" {
			message = resp.Status
		}
		return nil, fmt.Errorf("admin API %s: %s", resp.Status, message)
	}
	return data, nil
}

func writePluginRemoteData(data []byte) error {
	if len(data) == 0 {
		fmt.Fprintln(os.Stdout, "{}")
		return nil
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, data, "", "  ") == nil {
		pretty.WriteByte('\n')
		_, err := pretty.WriteTo(os.Stdout)
		return err
	}
	_, err := os.Stdout.Write(data)
	if err == nil && len(data) > 0 && data[len(data)-1] != '\n' {
		fmt.Fprintln(os.Stdout)
	}
	return err
}

func remoteConfigJSON(opts pluginRemoteOptions) (string, error) {
	if opts.ConfigJSON != "" {
		if !json.Valid([]byte(opts.ConfigJSON)) {
			return "", errors.New("--config-json must be valid JSON")
		}
		return opts.ConfigJSON, nil
	}
	if opts.ConfigPath != "" {
		data, err := os.ReadFile(opts.ConfigPath)
		if err != nil {
			return "", err
		}
		if !json.Valid(data) {
			return "", fmt.Errorf("config file %q must contain valid JSON", opts.ConfigPath)
		}
		return string(data), nil
	}
	return "{}", nil
}

func remoteMetadataJSON(opts pluginRemoteOptions) (map[string]any, error) {
	if opts.MetadataJSON != "" {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(opts.MetadataJSON), &decoded); err != nil {
			return nil, fmt.Errorf("--metadata-json must be a JSON object: %w", err)
		}
		return decoded, nil
	}
	if opts.MetadataPath != "" {
		data, err := os.ReadFile(opts.MetadataPath)
		if err != nil {
			return nil, err
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			return nil, fmt.Errorf("metadata file %q must contain a JSON object: %w", opts.MetadataPath, err)
		}
		return decoded, nil
	}
	return map[string]any{}, nil
}
