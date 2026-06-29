// internal/pluginmanager/artifact.go 校验、保存、哈希并描述上传到网关的插件制品包。

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
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

var (
	pluginIDPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	secretNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	schemaKeyPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

const distributionPackageName = "artifact.mcgp"

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

func (s ArtifactStore) DistributionPackagePath(artifact ArtifactRecord) string {
	if s.Root == "" || artifact.PluginID == "" || artifact.ID == "" {
		return ""
	}
	return filepath.Join(s.Root, artifact.PluginID, artifact.ID, distributionPackageName)
}

func (s ArtifactStore) HasDistributionPackage(artifact ArtifactRecord) bool {
	path := s.DistributionPackagePath(artifact)
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
}

func (s ArtifactStore) ValidateAndStore(upload ArtifactUpload) (ArtifactRecord, error) {
	return s.validateAndStore(upload, "")
}

func (s ArtifactStore) ValidateAndStoreBinary(upload ArtifactUpload) (ArtifactRecord, error) {
	return s.validateAndStore(upload, ArtifactTypeBinary)
}

func (s ArtifactStore) ValidateAndStoreSource(upload ArtifactUpload) (ArtifactRecord, error) {
	return s.validateAndStore(upload, ArtifactTypeSource)
}

func (s ArtifactStore) StoreBuiltBinary(upload ArtifactUpload, manifest Manifest, pluginBytes []byte, packageSHA string, metadata map[string]any) (ArtifactRecord, error) {
	if s.Root == "" {
		return ArtifactRecord{}, errors.New("plugin artifact root is empty")
	}
	if s.now == nil {
		s.now = time.Now
	}
	manifest.ArtifactType = ArtifactTypeBinary
	manifest.Runtime.Entry = RuntimeEntry
	if err := validateManifest(manifest); err != nil {
		return ArtifactRecord{}, err
	}
	if len(pluginBytes) == 0 {
		return ArtifactRecord{}, errors.New("built runtime entry is empty")
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
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return ArtifactRecord{}, err
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "manifest.json"), manifestBytes, 0644); err != nil {
		return ArtifactRecord{}, err
	}
	if err := writeBinaryDistributionPackage(filepath.Join(artifactDir, distributionPackageName), manifestBytes, pluginBytes, manifest.Runtime.Entry, metadata); err != nil {
		return ArtifactRecord{}, err
	}
	if len(metadata) > 0 {
		provenance, err := json.MarshalIndent(metadata, "", "  ")
		if err != nil {
			return ArtifactRecord{}, err
		}
		if err := os.WriteFile(filepath.Join(artifactDir, "provenance.json"), provenance, 0644); err != nil {
			return ArtifactRecord{}, err
		}
	}
	extensionPoints, err := json.Marshal(extensionPointKeys(manifest))
	if err != nil {
		return ArtifactRecord{}, err
	}
	capabilities, err := manifestCapabilitiesSummaryJSON(manifest)
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
		ArtifactType:            ArtifactTypeBinary,
		RuntimeType:             manifest.Runtime.Type,
		RuntimeEntry:            manifest.Runtime.Entry,
		Status:                  ArtifactStatusLoadable,
		MetadataJSON:            string(manifestBytes),
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

func (s ArtifactStore) validateAndStore(upload ArtifactUpload, expectedArtifactType string) (ArtifactRecord, error) {
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
		if file.FileInfo().IsDir() {
			if _, err := cleanZipDirName(file.Name); err != nil {
				return ArtifactRecord{}, err
			}
			continue
		}
		clean, err := cleanZipName(file.Name)
		if err != nil {
			return ArtifactRecord{}, err
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
	if expectedArtifactType != "" && manifest.ArtifactType != expectedArtifactType {
		return ArtifactRecord{}, fmt.Errorf("artifact_type %q does not match expected %q", manifest.ArtifactType, expectedArtifactType)
	}
	conformance, hasConformance, err := conformanceSummaryFromEntries(entries, s.MaxManifestBytes)
	if err != nil {
		return ArtifactRecord{}, err
	}
	if manifest.ArtifactType == ArtifactTypeSource {
		return s.storeSourcePackage(upload, manifest, manifestBytes, entries, packageSHA, conformance, hasConformance)
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
	if err := copyFile(upload.SourcePath, filepath.Join(artifactDir, distributionPackageName)); err != nil {
		return ArtifactRecord{}, err
	}

	provenance, err := artifactProvenanceFromEntries(entries, s.MaxManifestBytes)
	if err != nil {
		return ArtifactRecord{}, err
	}
	sbomSummary, err := sbomSummaryFromEntries(entries, s.MaxManifestBytes)
	if err != nil {
		return ArtifactRecord{}, err
	}
	manifest = mergeSBOMDependenciesIntoManifest(manifest, sbomSummary.Dependencies)
	metadataJSON, err := artifactMetadataJSON(manifest, conformance, hasConformance, provenance)
	if err != nil {
		return ArtifactRecord{}, err
	}
	extensionPoints, err := json.Marshal(extensionPointKeys(manifest))
	if err != nil {
		return ArtifactRecord{}, err
	}
	capabilities, err := manifestCapabilitiesSummaryJSON(manifest)
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

func (s ArtifactStore) storeSourcePackage(upload ArtifactUpload, manifest Manifest, manifestBytes []byte, entries map[string]*zip.File, packageSHA string, conformance ConformanceSummary, hasConformance bool) (ArtifactRecord, error) {
	if err := validateSourceEntries(manifest, entries); err != nil {
		return ArtifactRecord{}, err
	}
	artifactID := packageSHA
	artifactDir := filepath.Join(s.Root, manifest.ID, artifactID)
	sourceDir := filepath.Join(artifactDir, "source")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		return ArtifactRecord{}, err
	}
	if err := copyFile(upload.SourcePath, filepath.Join(artifactDir, "source.mcgp")); err != nil {
		return ArtifactRecord{}, err
	}
	var sizeBytes int64
	for name, file := range entries {
		if name == "manifest.json" {
			continue
		}
		if file.UncompressedSize64 > uint64(s.MaxNonRuntimeBytes) && !strings.HasPrefix(name, "vendor/") {
			return ArtifactRecord{}, fmt.Errorf("zip entry %q size %d exceeds limit %d", name, file.UncompressedSize64, s.MaxNonRuntimeBytes)
		}
		target := filepath.Join(sourceDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return ArtifactRecord{}, err
		}
		data, err := readZipFile(file, s.MaxExtractedBytes)
		if err != nil {
			return ArtifactRecord{}, err
		}
		sizeBytes += int64(len(data))
		if err := os.WriteFile(target, data, 0644); err != nil {
			return ArtifactRecord{}, err
		}
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "manifest.json"), manifestBytes, 0644); err != nil {
		return ArtifactRecord{}, err
	}
	provenance, err := artifactProvenanceFromEntries(entries, s.MaxManifestBytes)
	if err != nil {
		return ArtifactRecord{}, err
	}
	sbomSummary, err := sbomSummaryFromEntries(entries, s.MaxManifestBytes)
	if err != nil {
		return ArtifactRecord{}, err
	}
	manifest = mergeSBOMDependenciesIntoManifest(manifest, sbomSummary.Dependencies)
	metadataJSON, err := artifactMetadataJSON(manifest, conformance, hasConformance, provenance)
	if err != nil {
		return ArtifactRecord{}, err
	}
	extensionPoints, err := json.Marshal(extensionPointKeys(manifest))
	if err != nil {
		return ArtifactRecord{}, err
	}
	capabilities, err := manifestCapabilitiesSummaryJSON(manifest)
	if err != nil {
		return ArtifactRecord{}, err
	}
	now := s.now().Unix()
	return ArtifactRecord{
		ID:                      artifactID,
		PluginID:                manifest.ID,
		Version:                 manifest.Version,
		FileName:                upload.FileName,
		FilePath:                sourceDir,
		SHA256:                  artifactID,
		PackageSHA256:           packageSHA,
		SizeBytes:               sizeBytes,
		ArtifactType:            ArtifactTypeSource,
		RuntimeType:             manifest.Runtime.Type,
		RuntimeEntry:            sourceBuildEntry(manifest),
		Status:                  ArtifactStatusValidated,
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

func artifactMetadataJSON(manifest Manifest, conformance ConformanceSummary, hasConformance bool, provenance map[string]any) ([]byte, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if !hasConformance {
		if len(provenance) == 0 {
			return data, nil
		}
	}
	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}
	for key, value := range provenance {
		metadata[key] = value
	}
	if hasConformance {
		metadata["conformance"] = conformance
	}
	return json.Marshal(metadata)
}

func artifactProvenanceFromEntries(entries map[string]*zip.File, maxBytes int64) (map[string]any, error) {
	file, ok := entries["provenance.json"]
	if !ok {
		return nil, nil
	}
	if maxBytes <= 0 {
		maxBytes = DefaultManifestMaxBytes
	}
	if file.UncompressedSize64 > uint64(maxBytes) {
		return nil, fmt.Errorf("provenance.json size %d exceeds limit %d", file.UncompressedSize64, maxBytes)
	}
	data, err := readZipFile(file, maxBytes)
	if err != nil {
		return nil, err
	}
	var provenance map[string]any
	if err := json.Unmarshal(data, &provenance); err != nil {
		return nil, fmt.Errorf("invalid provenance.json: %w", err)
	}
	return provenance, nil
}

type sbomDependencySummary struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	License string `json:"license,omitempty"`
}

type sbomParseSummary struct {
	Parsed       bool                    `json:"parsed"`
	Sources      []string                `json:"sources,omitempty"`
	Dependencies []sbomDependencySummary `json:"dependencies,omitempty"`
}

func sbomSummaryFromEntries(entries map[string]*zip.File, maxBytes int64) (sbomParseSummary, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultManifestMaxBytes
	}
	var summary sbomParseSummary
	for name, file := range entries {
		base := strings.ToLower(path.Base(name))
		if !strings.Contains(base, "sbom") || !strings.HasSuffix(base, ".json") {
			continue
		}
		if file.UncompressedSize64 > uint64(maxBytes) {
			return summary, fmt.Errorf("%s size %d exceeds limit %d", name, file.UncompressedSize64, maxBytes)
		}
		data, err := readZipFile(file, maxBytes)
		if err != nil {
			return summary, err
		}
		deps, err := parseSBOMDependencies(data)
		if err != nil {
			return summary, fmt.Errorf("invalid SBOM %q: %w", name, err)
		}
		summary.Parsed = true
		summary.Sources = append(summary.Sources, name)
		summary.Dependencies = append(summary.Dependencies, deps...)
	}
	summary.Dependencies = uniqueSBOMDependencySummaries(summary.Dependencies)
	sort.Strings(summary.Sources)
	return summary, nil
}

func parseSBOMDependencies(data []byte) ([]sbomDependencySummary, error) {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	var deps []sbomDependencySummary
	for _, key := range []string{"dependencies", "sbom_dependencies", "modules"} {
		for _, item := range anySlice(root[key]) {
			if dep := sbomDependencyFromMap(jsonMapFromAny(item)); dep.Name != "" {
				deps = append(deps, dep)
			}
		}
	}
	for _, item := range anySlice(root["packages"]) {
		obj := jsonMapFromAny(item)
		dep := sbomDependencySummary{
			Name:    firstMetadataString(obj, "name", "SPDXID"),
			Version: firstMetadataString(obj, "version", "versionInfo"),
			License: firstMetadataString(obj, "license", "licenseDeclared", "licenseConcluded"),
		}
		if dep.Name != "" && !strings.HasPrefix(dep.Name, "SPDXRef-DOCUMENT") {
			deps = append(deps, dep)
		}
	}
	for _, item := range anySlice(root["components"]) {
		obj := jsonMapFromAny(item)
		dep := sbomDependencySummary{
			Name:    firstMetadataString(obj, "name", "purl"),
			Version: firstMetadataString(obj, "version"),
			License: sbomCycloneDXLicense(obj["licenses"]),
		}
		if dep.Name != "" {
			deps = append(deps, dep)
		}
	}
	return uniqueSBOMDependencySummaries(deps), nil
}

func sbomDependencyFromMap(obj map[string]any) sbomDependencySummary {
	if obj == nil {
		return sbomDependencySummary{}
	}
	return sbomDependencySummary{
		Name:    firstMetadataString(obj, "name", "path", "module", "package"),
		Version: firstMetadataString(obj, "version"),
		License: firstMetadataString(obj, "license", "license_expression", "spdx"),
	}
}

func firstMetadataString(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := metadataString(obj[key]); value != "" {
			return value
		}
	}
	return ""
}

func sbomCycloneDXLicense(value any) string {
	for _, item := range anySlice(value) {
		obj := jsonMapFromAny(item)
		if obj == nil {
			continue
		}
		if license := jsonMapFromAny(obj["license"]); license != nil {
			if id := firstMetadataString(license, "id", "name"); id != "" {
				return id
			}
		}
		if expression := metadataString(obj["expression"]); expression != "" {
			return expression
		}
	}
	return ""
}

func mergeSBOMDependenciesIntoManifest(manifest Manifest, deps []sbomDependencySummary) Manifest {
	if len(deps) == 0 {
		return manifest
	}
	var supply map[string]any
	if len(manifest.SupplyChain) > 0 {
		_ = json.Unmarshal(manifest.SupplyChain, &supply)
	}
	if supply == nil {
		supply = map[string]any{}
	}
	existing := anySlice(supply["sbom_dependencies"])
	for _, dep := range deps {
		item := map[string]any{"name": dep.Name}
		if dep.Version != "" {
			item["version"] = dep.Version
		}
		if dep.License != "" {
			item["license"] = dep.License
		}
		existing = append(existing, item)
	}
	supply["sbom_dependencies"] = existing
	data, err := json.Marshal(supply)
	if err == nil {
		manifest.SupplyChain = data
	}
	return manifest
}

func uniqueSBOMDependencySummaries(deps []sbomDependencySummary) []sbomDependencySummary {
	seen := map[string]bool{}
	var out []sbomDependencySummary
	for _, dep := range deps {
		dep.Name = strings.TrimSpace(dep.Name)
		dep.Version = strings.TrimSpace(dep.Version)
		dep.License = strings.TrimSpace(dep.License)
		if dep.Name == "" {
			continue
		}
		key := dep.Name + "\x00" + dep.Version + "\x00" + dep.License
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, dep)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].License < out[j].License
	})
	return out
}

func conformanceSummaryFromEntries(entries map[string]*zip.File, maxBytes int64) (ConformanceSummary, bool, error) {
	file, ok := entries["conformance.json"]
	if !ok {
		return ConformanceSummary{}, false, nil
	}
	if maxBytes <= 0 {
		maxBytes = DefaultManifestMaxBytes
	}
	if file.UncompressedSize64 > uint64(maxBytes) {
		return ConformanceSummary{}, false, fmt.Errorf("conformance.json size %d exceeds limit %d", file.UncompressedSize64, maxBytes)
	}
	data, err := readZipFile(file, maxBytes)
	if err != nil {
		return ConformanceSummary{}, false, err
	}
	var fixtureFile struct {
		Fixtures []map[string]any `json:"fixtures"`
	}
	if err := json.Unmarshal(data, &fixtureFile); err != nil {
		return ConformanceSummary{}, false, fmt.Errorf("invalid conformance.json: %w", err)
	}
	summary := ConformanceSummary{Source: "conformance.json", OK: true}
	for _, fixture := range fixtureFile.Fixtures {
		name := strings.TrimSpace(fmt.Sprint(fixture["name"]))
		if name == "" {
			continue
		}
		status := strings.TrimSpace(fmt.Sprint(fixture["status"]))
		if status == "" {
			status = "pass"
		}
		expected := strings.TrimSpace(fmt.Sprint(fixture["expected"]))
		extension := strings.TrimSpace(fmt.Sprint(fixture["extension"]))
		summary.Total++
		switch {
		case conformanceFixtureStatusFailed(status, expected):
			summary.Failed++
			summary.FailedFixtures = append(summary.FailedFixtures, ConformanceFixtureSummary{
				Name:      name,
				Status:    status,
				Extension: extension,
				Expected:  expected,
			})
		case status == "skip" || status == "skipped":
			summary.Skipped++
		default:
			summary.Passed++
		}
	}
	summary.OK = summary.Failed == 0
	return summary, true, nil
}

func conformanceFixtureStatusFailed(status, expected string) bool {
	switch strings.TrimSpace(status) {
	case "fail", "failed", "error", "blocking":
		return true
	case "blocked":
		return strings.TrimSpace(expected) != "blocked"
	default:
		return false
	}
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
		Route           RouteCapability           `json:"route"`
		Status          StatusCapability          `json:"status"`
		Middleware      MiddlewareCapability      `json:"middleware"`
		Providers       []ProviderCapability      `json:"providers"`
		EventSubscriber EventSubscriberCapability `json:"event_subscriber"`
		Ingress         *IngressCapability        `json:"ingress"`
		Minecraft       *MinecraftCapability      `json:"minecraft"`
		Runtime         RuntimeCapability         `json:"runtime"`
	}
	if err := json.Unmarshal(raw, &caps); err != nil {
		return nil, fmt.Errorf("invalid capabilities: %w", err)
	}
	if caps.UpstreamConnect.Mode != "" {
		summary.UpstreamConnect.Mode = caps.UpstreamConnect.Mode
	}
	summary.Route = caps.Route
	summary.Status = caps.Status
	summary.Middleware = caps.Middleware
	summary.Providers = append([]ProviderCapability(nil), caps.Providers...)
	summary.EventSubscriber = caps.EventSubscriber
	if caps.Ingress != nil {
		ingress := *caps.Ingress
		ingress.SecretRefs = append([]string(nil), caps.Ingress.SecretRefs...)
		if caps.Ingress.TLS != nil {
			tls := *caps.Ingress.TLS
			ingress.TLS = &tls
		}
		if caps.Ingress.Health != nil {
			health := *caps.Ingress.Health
			ingress.Health = &health
		}
		summary.Ingress = &ingress
	}
	summary.Runtime = caps.Runtime
	summary.Runtime.RequiredFeatures = append(summary.Runtime.RequiredFeatures, stringSlice(jsonObjectFromRaw(string(raw), "required_features"))...)
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

func manifestCapabilitiesSummaryJSON(manifest Manifest) ([]byte, error) {
	data, err := capabilitiesSummaryJSON(manifest.Capabilities)
	if err != nil {
		return nil, err
	}
	var summary CapabilitySummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return nil, err
	}
	summary.Events = append([]EventSpec(nil), manifest.Events...)
	summary.CustomMetrics = append([]MetricSpec(nil), manifest.CustomMetrics...)
	summary.ExternalDeps = append([]ExternalSpec(nil), manifest.ExternalDeps...)
	summary.DataStores = append([]DataStoreSpec(nil), manifest.DataStores...)
	summary.FileStores = append([]FileStoreSpec(nil), manifest.FileStores...)
	return json.Marshal(summary)
}

func ManifestCapabilitySummary(manifest Manifest) (CapabilitySummary, error) {
	data, err := manifestCapabilitiesSummaryJSON(manifest)
	if err != nil {
		return CapabilitySummary{}, err
	}
	var summary CapabilitySummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return CapabilitySummary{}, err
	}
	return summary, nil
}

func ValidateIngressCapability(manifest Manifest, ingress *IngressCapability) []string {
	if ingress == nil {
		return []string{"capabilities.ingress is required when ingress.service/v1 is declared"}
	}
	var problems []string
	protocol := strings.ToLower(strings.TrimSpace(ingress.Protocol))
	switch protocol {
	case "tcp", "udp", "tls", "proxy-protocol":
	case "":
		problems = append(problems, "ingress.protocol is required")
	default:
		problems = append(problems, fmt.Sprintf("ingress.protocol %q is not supported", ingress.Protocol))
	}
	bind := strings.TrimSpace(ingress.Bind)
	if bind == "" {
		problems = append(problems, "ingress.bind is required")
	} else if net.ParseIP(bind) == nil {
		problems = append(problems, fmt.Sprintf("ingress.bind %q must be an IP address", ingress.Bind))
	}
	if ingress.Port <= 0 || ingress.Port > 65535 {
		problems = append(problems, fmt.Sprintf("ingress.port %d must be between 1 and 65535", ingress.Port))
	}
	tlsRequired := protocol == "tls" || (ingress.TLS != nil && ingress.TLS.Enabled)
	if tlsRequired {
		if ingress.TLS == nil {
			problems = append(problems, "ingress.tls is required for TLS ingress")
		} else {
			if strings.TrimSpace(ingress.TLS.CertSecret) == "" {
				problems = append(problems, "ingress.tls.cert_secret is required for TLS ingress")
			}
			if strings.TrimSpace(ingress.TLS.KeySecret) == "" {
				problems = append(problems, "ingress.tls.key_secret is required for TLS ingress")
			}
		}
	}
	declaredSecrets := make(map[string]bool, len(manifest.Secrets))
	for _, secret := range manifest.Secrets {
		declaredSecrets[secret.Name] = true
	}
	secretRefs := append([]string(nil), ingress.SecretRefs...)
	if ingress.TLS != nil {
		secretRefs = append(secretRefs, ingress.TLS.CertSecret, ingress.TLS.KeySecret)
	}
	for _, ref := range secretRefs {
		name := strings.TrimSpace(ref)
		if name == "" {
			continue
		}
		if !secretNamePattern.MatchString(name) {
			problems = append(problems, fmt.Sprintf("ingress secret ref %q is invalid", ref))
			continue
		}
		if !declaredSecrets[name] {
			problems = append(problems, fmt.Sprintf("ingress secret ref %q is not declared in manifest.secrets", name))
		}
	}
	if ingress.Health != nil {
		if path := strings.TrimSpace(ingress.Health.Path); path != "" && !strings.HasPrefix(path, "/") {
			problems = append(problems, fmt.Sprintf("ingress.health.path %q must start with /", ingress.Health.Path))
		}
		if ingress.Health.Interval != "" {
			if _, err := time.ParseDuration(ingress.Health.Interval); err != nil {
				problems = append(problems, fmt.Sprintf("ingress.health.interval %q is invalid", ingress.Health.Interval))
			}
		}
		if ingress.Health.Timeout != "" {
			if _, err := time.ParseDuration(ingress.Health.Timeout); err != nil {
				problems = append(problems, fmt.Sprintf("ingress.health.timeout %q is invalid", ingress.Health.Timeout))
			}
		}
	}
	return problems
}

func IngressCapabilityDetails(ingress *IngressCapability) map[string]any {
	details := map[string]any{"extension_point": ExtensionIngressService, "data_plane": false}
	if ingress == nil {
		return details
	}
	if protocol := strings.TrimSpace(ingress.Protocol); protocol != "" {
		details["protocol"] = protocol
	}
	if bind := strings.TrimSpace(ingress.Bind); bind != "" {
		details["bind"] = bind
	}
	if ingress.Port != 0 {
		details["port"] = ingress.Port
	}
	if len(ingress.SecretRefs) > 0 {
		details["secret_refs"] = append([]string(nil), ingress.SecretRefs...)
	}
	if ingress.TLS != nil {
		tls := map[string]any{"enabled": ingress.TLS.Enabled}
		if ingress.TLS.CertSecret != "" {
			tls["cert_secret"] = ingress.TLS.CertSecret
		}
		if ingress.TLS.KeySecret != "" {
			tls["key_secret"] = ingress.TLS.KeySecret
		}
		details["tls"] = tls
	}
	if ingress.Health != nil {
		health := map[string]any{}
		if ingress.Health.Path != "" {
			health["path"] = ingress.Health.Path
		}
		if ingress.Health.Interval != "" {
			health["interval"] = ingress.Health.Interval
		}
		if ingress.Health.Timeout != "" {
			health["timeout"] = ingress.Health.Timeout
		}
		if len(health) > 0 {
			details["health"] = health
		}
	}
	return details
}

func validateManifest(manifest Manifest) error {
	switch {
	case manifest.SchemaVersion != SchemaVersion:
		return fmt.Errorf("unsupported schema_version %q", manifest.SchemaVersion)
	case !pluginIDPattern.MatchString(manifest.ID):
		return fmt.Errorf("invalid plugin id %q", manifest.ID)
	case strings.TrimSpace(manifest.Version) == "":
		return errors.New("version is required")
	case manifest.ArtifactType != ArtifactTypeBinary && manifest.ArtifactType != ArtifactTypeSource:
		return fmt.Errorf("unsupported artifact_type %q", manifest.ArtifactType)
	case manifest.Runtime.Type != RuntimeGoPlugin && manifest.Runtime.Type != RuntimeBuiltin && manifest.Runtime.Type != RuntimeSandbox && manifest.Runtime.Type != RuntimeWASM:
		return fmt.Errorf("unsupported runtime.type %q", manifest.Runtime.Type)
	case manifest.ArtifactType == ArtifactTypeBinary && manifest.Runtime.Type == RuntimeGoPlugin && manifest.Runtime.Entry != RuntimeEntry:
		return fmt.Errorf("unsupported runtime.entry %q", manifest.Runtime.Entry)
	case manifest.ArtifactType == ArtifactTypeBinary && manifest.Runtime.Type == RuntimeWASM && manifest.Runtime.Entry != RuntimeWASMEntry:
		return fmt.Errorf("unsupported runtime.entry %q", manifest.Runtime.Entry)
	case manifest.ArtifactType == ArtifactTypeSource && rawSourceBuildEntry(manifest) == "":
		return errors.New("build.entry is required for source artifacts")
	case manifest.APIVersion != APIVersion:
		return fmt.Errorf("unsupported api_version %q", manifest.APIVersion)
	case manifest.ArtifactType == ArtifactTypeBinary && manifest.Runtime.Type == RuntimeGoPlugin && manifest.GoVersion == "":
		return errors.New("go_version is required")
	case manifest.ArtifactType == ArtifactTypeBinary && manifest.Runtime.Type == RuntimeGoPlugin && manifest.GOOS == "":
		return errors.New("go_os is required")
	case manifest.ArtifactType == ArtifactTypeBinary && manifest.Runtime.Type == RuntimeGoPlugin && manifest.GOARCH == "":
		return errors.New("go_arch is required")
	}
	if manifest.ArtifactType == ArtifactTypeBinary && manifest.Runtime.Type == RuntimeGoPlugin && manifest.GOOS != "" && manifest.GOOS != runtime.GOOS {
		return fmt.Errorf("go_os %q does not match gateway %q", manifest.GOOS, runtime.GOOS)
	}
	if manifest.ArtifactType == ArtifactTypeBinary && manifest.Runtime.Type == RuntimeGoPlugin && manifest.GOARCH != "" && manifest.GOARCH != runtime.GOARCH {
		return fmt.Errorf("go_arch %q does not match gateway %q", manifest.GOARCH, runtime.GOARCH)
	}
	found := false
	for _, ep := range manifest.ExtensionPoints {
		if supportedExtensionPoint(ep.Key) {
			found = true
		}
	}
	if !found {
		return errors.New("at least one supported extension point is required")
	}
	seenSecrets := make(map[string]bool, len(manifest.Secrets))
	for _, secret := range manifest.Secrets {
		if !secretNamePattern.MatchString(secret.Name) {
			return fmt.Errorf("invalid secret name %q", secret.Name)
		}
		if seenSecrets[secret.Name] {
			return fmt.Errorf("duplicate secret name %q", secret.Name)
		}
		seenSecrets[secret.Name] = true
	}
	if err := validateNamedSpecs("event", eventSpecNames(manifest.Events)); err != nil {
		return err
	}
	if err := validateNamedSpecs("custom metric", metricSpecNames(manifest.CustomMetrics)); err != nil {
		return err
	}
	if err := validateNamedSpecs("background task", taskSpecNames(manifest.BackgroundTasks)); err != nil {
		return err
	}
	if err := validateNamedSpecs("external dependency", externalSpecNames(manifest.ExternalDeps)); err != nil {
		return err
	}
	for _, dep := range manifest.ExternalDeps {
		if strings.TrimSpace(dep.Endpoint) == "" {
			return fmt.Errorf("external dependency %q endpoint is required", dep.Name)
		}
		if dep.Retry < 0 {
			return fmt.Errorf("external dependency %q retry must be non-negative", dep.Name)
		}
		if dep.Timeout != "" {
			if _, err := time.ParseDuration(dep.Timeout); err != nil {
				return fmt.Errorf("external dependency %q timeout is invalid: %w", dep.Name, err)
			}
		}
		if policy := normalizeExternalFailPolicy(dep); !validExternalFailPolicy(policy) {
			return fmt.Errorf("external dependency %q fail_policy %q is invalid", dep.Name, dep.FailPolicy)
		}
	}
	return nil
}

func supportedExtensionPoint(key string) bool {
	switch key {
	case ExtensionUpstreamConnect, ExtensionRouteResolve, ExtensionRouteResolver, ExtensionStatusPing,
		ExtensionConnectionFilter, ExtensionHandshakeFilter, ExtensionEventSubscriber,
		ExtensionProvider, ExtensionAuthProvider, ExtensionAdminAuthProvider,
		ExtensionRuleEvaluate, ExtensionConfigValidate, ExtensionIngressService:
		return true
	default:
		return false
	}
}

func validateNamedSpecs(kind string, names []string) error {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !schemaKeyPattern.MatchString(name) {
			return fmt.Errorf("invalid %s name %q", kind, name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate %s name %q", kind, name)
		}
		seen[name] = true
	}
	return nil
}

func eventSpecNames(specs []EventSpec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
		for _, field := range spec.Fields {
			if !schemaKeyPattern.MatchString(field) {
				names = append(names, "invalid field "+field)
			}
		}
	}
	return names
}

func metricSpecNames(specs []MetricSpec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
		for _, label := range spec.Labels {
			if !schemaKeyPattern.MatchString(label) {
				names = append(names, "invalid label "+label)
			}
		}
	}
	return names
}

func taskSpecNames(specs []TaskSpec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.ID)
	}
	return names
}

func externalSpecNames(specs []ExternalSpec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

func validateSourceEntries(manifest Manifest, entries map[string]*zip.File) error {
	if _, ok := entries["go.mod"]; !ok {
		return errors.New("source package requires go.mod")
	}
	buildEntry := sourceBuildEntry(manifest)
	if buildEntry == "" || buildEntry == "." {
		buildEntry = "."
	}
	cleanBuildEntry, err := cleanZipName(buildEntry)
	if buildEntry == "." {
		cleanBuildEntry = "."
		err = nil
	}
	if err != nil {
		return fmt.Errorf("invalid runtime.build_entry: %w", err)
	}
	hasBuildSource := false
	hasAnySource := false
	for name := range entries {
		switch {
		case name == "manifest.json" || name == "go.mod" || name == "go.sum":
		case strings.HasPrefix(name, "vendor/"):
		case strings.HasPrefix(name, "testdata/"):
		case strings.EqualFold(path.Base(name), "README.md"), strings.EqualFold(path.Base(name), "LICENSE"),
			strings.EqualFold(path.Base(name), "conformance.json"), strings.Contains(strings.ToLower(path.Base(name)), "sbom"):
		case strings.HasSuffix(name, ".go"):
		default:
			return fmt.Errorf("unsupported source package entry %q", name)
		}
		if strings.HasSuffix(name, ".go") {
			hasAnySource = true
			if cleanBuildEntry == "." || strings.HasPrefix(name, cleanBuildEntry+"/") || path.Dir(name) == cleanBuildEntry {
				hasBuildSource = true
			}
		}
	}
	if !hasAnySource {
		return errors.New("source package requires at least one Go source file")
	}
	if !hasBuildSource {
		return fmt.Errorf("source package build entry %q has no Go source files", manifest.Runtime.BuildEntry)
	}
	return nil
}

func sourceBuildEntry(manifest Manifest) string {
	entry := rawSourceBuildEntry(manifest)
	if entry == "" {
		return SourceBuildEntry
	}
	return entry
}

func rawSourceBuildEntry(manifest Manifest) string {
	entry := strings.Trim(strings.TrimSpace(manifest.Build.Entry), "/")
	if entry == "" {
		entry = strings.Trim(strings.TrimSpace(manifest.Runtime.BuildEntry), "/")
	}
	return entry
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

func cleanZipDirName(name string) (string, error) {
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		return "", fmt.Errorf("unsafe zip entry %q", name)
	}
	return cleanZipName(name)
}

func writeBinaryDistributionPackage(target string, manifestBytes, pluginBytes []byte, entry string, metadata map[string]any) error {
	if entry == "" {
		entry = RuntimeEntry
	}
	tmp := target + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zipWriter := zip.NewWriter(file)
	if err := writeZipEntry(zipWriter, "manifest.json", manifestBytes); err != nil {
		_ = zipWriter.Close()
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := writeZipEntry(zipWriter, entry, pluginBytes); err != nil {
		_ = zipWriter.Close()
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if len(metadata) > 0 {
		provenance, err := json.MarshalIndent(metadata, "", "  ")
		if err != nil {
			_ = zipWriter.Close()
			_ = file.Close()
			_ = os.Remove(tmp)
			return err
		}
		if err := writeZipEntry(zipWriter, "provenance.json", provenance); err != nil {
			_ = zipWriter.Close()
			_ = file.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := zipWriter.Close(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, target)
}

func writeZipEntry(zipWriter *zip.Writer, name string, data []byte) error {
	writer, err := zipWriter.Create(name)
	if err != nil {
		return err
	}
	_, err = writer.Write(data)
	return err
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

func copyFile(src, dst string) error {
	input, err := os.Open(src)
	if err != nil {
		return err
	}
	defer input.Close()

	output, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer output.Close()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	return output.Close()
}
