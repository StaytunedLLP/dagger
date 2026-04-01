package imageload

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestNormalizeIncusArchitecture(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"amd64":  "x86_64",
		"arm64":  "aarch64",
		"arm":    "armhf",
		"386":    "i686",
		"s390x":  "s390x",
		"ppc64le": "ppc64le",
	}

	for input, expected := range cases {
		require.Equal(t, expected, normalizeIncusArchitecture(input))
	}
}

func TestBuildMetadataYAMLUsesIncusArchitectureNames(t *testing.T) {
	t.Parallel()

	yaml := buildMetadataYAML("alias", dockerImageConfig{Architecture: "arm64", OS: "linux", Created: time.Unix(0, 0).UTC()})
	require.Contains(t, yaml, "aarch64")
	require.Contains(t, yaml, "linux")
	require.Contains(t, yaml, "alias")
}

func TestSafeExtractPathRejectsTraversal(t *testing.T) {
	t.Parallel()

	rootfs := t.TempDir()
	_, err := safeExtractPath(rootfs, "../../etc/passwd")
	require.Error(t, err)
	_, err = safeExtractPath(rootfs, "/etc/passwd")
	require.Error(t, err)
}

func TestUntarIntoRejectsTraversal(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     "../../etc/passwd",
		Mode:     0o644,
		Size:     int64(len("owned")),
		Typeflag: tar.TypeReg,
	}))
	_, err := io.WriteString(tw, "owned")
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	rootfs := t.TempDir()
	err = untarInto(rootfs, bytes.NewReader(buf.Bytes()))
	require.Error(t, err)
}

func TestUntarIntoRejectsHardlinkTraversal(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     "link",
		Linkname: "../../etc/passwd",
		Typeflag: tar.TypeLink,
	}))
	require.NoError(t, tw.Close())

	rootfs := t.TempDir()
	err := untarInto(rootfs, bytes.NewReader(buf.Bytes()))
	require.Error(t, err)
}

func TestSelectOCIManifestDescriptorChoosesHostPlatform(t *testing.T) {
	t.Parallel()

	hostArch := normalizeIncusArchitecture(runtime.GOARCH)
	otherArch := "arm64"
	if hostArch == "aarch64" {
		otherArch = "amd64"
	}

	index := ocispecs.Index{
		Manifests: []ocispecs.Descriptor{
			{Platform: &ocispecs.Platform{OS: runtime.GOOS, Architecture: otherArch}},
			{Platform: &ocispecs.Platform{OS: runtime.GOOS, Architecture: hostArch}},
		},
	}

	desc, err := selectOCIManifestDescriptor(index)
	require.NoError(t, err)
	require.Equal(t, hostArch, normalizeIncusArchitecture(desc.Platform.Architecture))
}

func TestSelectDockerManifestChoosesHostPlatform(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	hostCfgPath := filepath.Join(dir, "host-config.json")
	otherCfgPath := filepath.Join(dir, "other-config.json")

	hostCfg := dockerImageConfig{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	otherCfg := dockerImageConfig{OS: runtime.GOOS}
	if runtime.GOARCH == "amd64" {
		otherCfg.Architecture = "arm64"
	} else {
		otherCfg.Architecture = "amd64"
	}

	hostBytes, err := json.Marshal(hostCfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(hostCfgPath, hostBytes, 0o644))

	otherBytes, err := json.Marshal(otherCfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(otherCfgPath, otherBytes, 0o644))

	index := &archiveIndex{
		dir: dir,
		files: map[string]string{
			"host-config.json":  hostCfgPath,
			"other-config.json": otherCfgPath,
		},
	}

	manifest, err := selectDockerManifest(index, []dockerManifestEntry{
		{Config: "other-config.json"},
		{Config: "host-config.json"},
	})
	require.NoError(t, err)
	require.Equal(t, "host-config.json", manifest.Config)
}

func TestImageArchiveFileLookupFallsBackToNormalizedName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "manifest.json")
	require.NoError(t, os.WriteFile(filePath, []byte("{}"), 0o644))

	index := &archiveIndex{
		dir: dir,
		files: map[string]string{
			"manifest.json": filePath,
		},
	}

	got, err := index.file("./manifest.json")
	require.NoError(t, err)
	require.Equal(t, filePath, got)
}

func TestIndexImageArchiveReadsGzipTarball(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "manifest.json",
		Mode: 0o644,
		Size: int64(len("[]")),
	}))
	_, err := io.WriteString(tw, "[]")
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	path := filepath.Join(t.TempDir(), "image.tar.gz")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))

	index, err := indexImageArchive(path)
	require.NoError(t, err)
	t.Cleanup(index.close)

	var manifest []byte
	require.NoError(t, readArchiveFile(index, "manifest.json", &manifest))
	require.JSONEq(t, "[]", string(manifest))
}
