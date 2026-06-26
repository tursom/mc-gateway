package pluginmanager

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var pluginIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

type ArtifactStore struct {
	Root               string
	MaxPackageBytes    int64
	MaxManifestBytes   int64
	MaxEntries         int
	MaxExtractedBytes  int64
	MaxNonRuntimeBytes int64
	now                func() time.Time
}

type ArtifactUpload struct {
	SourcePath string
	FileName   string
	Actor      string
}

func NewArtifactStore(root string) ArtifactStore {
	return ArtifactStore{
		Root:               root,
		MaxPackageBytes:    DefaultPackageMaxBytes,
		MaxManifestBytes:   DefaultManifestMaxBytes,
		MaxEntries:         DefaultPackageMaxEntries,
		MaxExtractedBytes:  DefaultExtractedMaxBytes,
		MaxNonRuntimeBytes: DefaultNonRuntimeMaxBytes,
		now:                time.Now,
	}
}

func (s ArtifactStore) ValidateAndStore(upload ArtifactUpload) (ArtifactRecord, error) {
	if s.Root == "" {
		return ArtifactRecord{}, errors.New("plugin artifact root is empty")
	}
	if s.MaxPackageBytes <= 0 {
		s.MaxPackageBytes = DefaultPackageMaxBytes
	}
	if s.MaxManifestBytes <= 0 {
		s.MaxManifestBytes = DefaultManifestMaxBytes
	}
	if s.MaxEntries <= 0 {
		s.MaxEntries = DefaultPackageMaxEntries
	}
	if s.MaxExtractedBytes <= 0 {
		s.MaxExtractedBytes = DefaultExtractedMaxBytes
	}
	if s.MaxNonRuntimeBytes <= 0 {
		s.MaxNonRuntimeBytes = DefaultNonRuntimeMaxBytes
	}
	if s.now == nil {
		s.now = time.Now
	}

	info, err := os.Stat(upload.SourcePath)
	if err != nil {
		return ArtifactRecord{}, err
	}
	if info.Size() <= 0 {
		return ArtifactRecord{}, errors.New("plugin package is empty")
	}
	if info.Size() > s.MaxPackageBytes {
		return ArtifactRecord{}, fmt.Errorf("plugin package size %d exceeds limit %d", info.Size(), s.MaxPackageBytes)
	}

	packageSHA, err := fileSHA256(upload.SourcePath)
	if err != nil {
		return ArtifactRecord{}, err
	}

	reader, err := zip.OpenReader(upload.SourcePath)
	if err != nil {
		return ArtifactRecord{}, err
	}
	defer reader.Close()
	if len(reader.File) > s.MaxEntries {
		return ArtifactRecord{}, fmt.Errorf("plugin package has %d entries, exceeds limit %d", len(reader.File), s.MaxEntries)
	}

	var manifestFile *zip.File
	entries := make(map[string]*zip.File)
	var extractedSize uint64
	for _, file := range reader.File {
		clean, err := cleanZipName(file.Name)
		if err != nil {
			return ArtifactRecord{}, err
		}
		if file.FileInfo().IsDir() {
			continue
		}
		mode := file.FileInfo().Mode()
		if !mode.IsRegular() || mode&os.ModeType != 0 {
			return ArtifactRecord{}, fmt.Errorf("unsupported zip entry type %q", file.Name)
		}
		if _, exists := entries[clean]; exists {
			return ArtifactRecord{}, fmt.Errorf("duplicate zip entry %q", clean)
		}
		extractedSize += file.UncompressedSize64
		if extractedSize > uint64(s.MaxExtractedBytes) {
			return ArtifactRecord{}, fmt.Errorf("plugin package extracted size exceeds limit %d", s.MaxExtractedBytes)
		}
		entries[clean] = file
		if clean == "manifest.json" {
			manifestFile = file
		}
	}
	if manifestFile == nil {
		return ArtifactRecord{}, errors.New("manifest.json is required")
	}
	if manifestFile.UncompressedSize64 > uint64(s.MaxManifestBytes) {
		return ArtifactRecord{}, fmt.Errorf("manifest.json size %d exceeds limit %d", manifestFile.UncompressedSize64, s.MaxManifestBytes)
	}

	manifestBytes, err := readZipFile(manifestFile, s.MaxManifestBytes)
	if err != nil {
		return ArtifactRecord{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return ArtifactRecord{}, fmt.Errorf("invalid manifest.json: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return ArtifactRecord{}, err
	}

	entry := manifest.Runtime.Entry
	pluginFile, ok := entries[entry]
	if !ok {
		return ArtifactRecord{}, fmt.Errorf("runtime entry %q is required", entry)
	}
	if pluginFile.UncompressedSize64 == 0 {
		return ArtifactRecord{}, errors.New("runtime entry is empty")
	}
	if pluginFile.UncompressedSize64 > uint64(s.MaxPackageBytes) {
		return ArtifactRecord{}, fmt.Errorf("runtime entry size %d exceeds limit %d", pluginFile.UncompressedSize64, s.MaxPackageBytes)
	}
	for name, file := range entries {
		if name == "manifest.json" || name == entry {
			continue
		}
		if file.UncompressedSize64 > uint64(s.MaxNonRuntimeBytes) {
			return ArtifactRecord{}, fmt.Errorf("zip entry %q size %d exceeds limit %d", name, file.UncompressedSize64, s.MaxNonRuntimeBytes)
		}
	}

	pluginBytes, err := readZipFile(pluginFile, s.MaxPackageBytes)
	if err != nil {
		return ArtifactRecord{}, err
	}
	pluginSum := sha256.Sum256(pluginBytes)
	artifactID := hex.EncodeToString(pluginSum[:])
	artifactDir := filepath.Join(s.Root, manifest.ID, artifactID)
	if err := os.MkdirAll(artifactDir, 0755); err != nil {
		return ArtifactRecord{}, err
	}
	pluginPath := filepath.Join(artifactDir, RuntimeEntry)
	if err := os.WriteFile(pluginPath, pluginBytes, 0644); err != nil {
		return ArtifactRecord{}, err
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "manifest.json"), manifestBytes, 0644); err != nil {
		return ArtifactRecord{}, err
	}

	metadataJSON, err := json.Marshal(manifest)
	if err != nil {
		return ArtifactRecord{}, err
	}
	extensionPoints, err := json.Marshal(extensionPointKeys(manifest))
	if err != nil {
		return ArtifactRecord{}, err
	}
	capabilities, err := capabilitiesSummaryJSON(manifest.Capabilities)
	if err != nil {
		return ArtifactRecord{}, err
	}
	now := s.now().Unix()
	return ArtifactRecord{
		ID:                      artifactID,
		PluginID:                manifest.ID,
		Version:                 manifest.Version,
		FileName:                upload.FileName,
		FilePath:                pluginPath,
		SHA256:                  artifactID,
		PackageSHA256:           packageSHA,
		SizeBytes:               int64(len(pluginBytes)),
		ArtifactType:            manifest.ArtifactType,
		RuntimeType:             manifest.Runtime.Type,
		RuntimeEntry:            manifest.Runtime.Entry,
		Status:                  ArtifactStatusLoadable,
		MetadataJSON:            string(metadataJSON),
		CapabilitiesSummaryJSON: string(capabilities),
		ExtensionPointsJSON:     string(extensionPoints),
		APIVersion:              manifest.APIVersion,
		GoVersion:               manifest.GoVersion,
		GOOS:                    manifest.GOOS,
		GOARCH:                  manifest.GOARCH,
		UploadedBy:              upload.Actor,
		CreatedAt:               now,
		UpdatedAt:               now,
	}, nil
}

func capabilitiesSummaryJSON(raw json.RawMessage) ([]byte, error) {
	summary := CapabilitySummary{
		UpstreamConnect: UpstreamConnectCapability{Mode: UpstreamModeDialer},
	}
	if len(raw) == 0 {
		return json.Marshal(summary)
	}
	summary.Raw = append(json.RawMessage(nil), raw...)
	var caps struct {
		UpstreamConnect UpstreamConnectCapability `json:"upstream_connect"`
		Minecraft       *MinecraftCapability      `json:"minecraft"`
	}
	if err := json.Unmarshal(raw, &caps); err != nil {
		return nil, fmt.Errorf("invalid capabilities: %w", err)
	}
	if caps.UpstreamConnect.Mode != "" {
		summary.UpstreamConnect.Mode = caps.UpstreamConnect.Mode
	}
	if caps.Minecraft != nil {
		summary.Minecraft = caps.Minecraft
		if summary.Minecraft.UnsupportedPolicy == "" {
			summary.Minecraft.UnsupportedPolicy = summary.Minecraft.ProtocolVersions.UnsupportedPolicy
		}
	}
	switch summary.UpstreamConnect.Mode {
	case UpstreamModeDialer, UpstreamModeProtocolProxy:
	default:
		return nil, fmt.Errorf("unsupported upstream_connect.mode %q", summary.UpstreamConnect.Mode)
	}
	return json.Marshal(summary)
}

func validateManifest(manifest Manifest) error {
	switch {
	case manifest.SchemaVersion != SchemaVersion:
		return fmt.Errorf("unsupported schema_version %q", manifest.SchemaVersion)
	case !pluginIDPattern.MatchString(manifest.ID):
		return fmt.Errorf("invalid plugin id %q", manifest.ID)
	case strings.TrimSpace(manifest.Version) == "":
		return errors.New("version is required")
	case manifest.ArtifactType != ArtifactTypeBinary:
		return fmt.Errorf("unsupported artifact_type %q", manifest.ArtifactType)
	case manifest.Runtime.Type != RuntimeGoPlugin:
		return fmt.Errorf("unsupported runtime.type %q", manifest.Runtime.Type)
	case manifest.Runtime.Entry != RuntimeEntry:
		return fmt.Errorf("unsupported runtime.entry %q", manifest.Runtime.Entry)
	case manifest.APIVersion != APIVersion:
		return fmt.Errorf("unsupported api_version %q", manifest.APIVersion)
	case manifest.GoVersion == "":
		return errors.New("go_version is required")
	case manifest.GOOS == "":
		return errors.New("go_os is required")
	case manifest.GOARCH == "":
		return errors.New("go_arch is required")
	}
	if manifest.GOOS != runtime.GOOS {
		return fmt.Errorf("go_os %q does not match gateway %q", manifest.GOOS, runtime.GOOS)
	}
	if manifest.GOARCH != runtime.GOARCH {
		return fmt.Errorf("go_arch %q does not match gateway %q", manifest.GOARCH, runtime.GOARCH)
	}
	found := false
	for _, ep := range manifest.ExtensionPoints {
		if ep.Type == "hook" && ep.Key == ExtensionUpstreamConnect {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("extension point %q is required", ExtensionUpstreamConnect)
	}
	return nil
}

func extensionPointKeys(manifest Manifest) []string {
	keys := make([]string, 0, len(manifest.ExtensionPoints))
	for _, ep := range manifest.ExtensionPoints {
		keys = append(keys, ep.Key)
	}
	return keys
}

func cleanZipName(name string) (string, error) {
	if name == "" || strings.Contains(name, `\`) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("unsafe zip entry %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean != name || strings.HasPrefix(clean, "../") || clean == ".." || path.IsAbs(clean) {
		return "", fmt.Errorf("unsafe zip entry %q", name)
	}
	return clean, nil
}

func readZipFile(file *zip.File, maxBytes int64) ([]byte, error) {
	rc, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, rc, maxBytes+1); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(buf.Len()) > maxBytes {
		return nil, fmt.Errorf("zip entry %q exceeds limit %d", file.Name, maxBytes)
	}
	return buf.Bytes(), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
