package imageload

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dagger/dagger/util/traceexec"
	telemetry "github.com/dagger/otel-go"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"go.opentelemetry.io/otel"
	"gopkg.in/yaml.v3"
)

type Incus struct{}

func init() {
	register("incus-image", Incus{})
}

func (loader Incus) Loader(ctx context.Context) (*Loader, error) {
	return &Loader{
		TarballWriter: loader.loadTarball,
		TarballReader: loader.saveTarball,
	}, nil
}

func (loader Incus) loadTarball(ctx context.Context, name string, tarball io.Reader) (rerr error) {
	ctx, span := otel.Tracer("").Start(ctx, "load "+name)
	defer telemetry.EndWithCause(span, &rerr)

	alias := incusImageAlias(name)
	tarPath, err := os.CreateTemp("", "dagger-incus-load-*.tar")
	if err != nil {
		return err
	}
	defer func() {
		_ = tarPath.Close()
		_ = os.Remove(tarPath.Name())
	}()

	if _, err := io.Copy(tarPath, tarball); err != nil {
		return err
	}
	if err := tarPath.Close(); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "incus", "image", "import", tarPath.Name(), "--alias", alias, "--reuse")
	if err := traceexec.Exec(ctx, cmd, telemetry.Encapsulated()); err == nil {
		return nil
	}

	converted, err := imageArchiveToIncusTarball(tarPath.Name(), alias)
	if err != nil {
		return fmt.Errorf("incus image import failed and conversion failed: %w", err)
	}
	defer os.Remove(converted)

	cmd = exec.CommandContext(ctx, "incus", "image", "import", converted, "--alias", alias, "--reuse")
	if err := traceexec.Exec(ctx, cmd, telemetry.Encapsulated()); err != nil {
		return fmt.Errorf("incus image import failed: %w", err)
	}
	return nil
}

func (loader Incus) saveTarball(ctx context.Context, name string, tarball io.Writer) (rerr error) {
	ctx, span := otel.Tracer("").Start(ctx, "save "+name)
	defer telemetry.EndWithCause(span, &rerr)

	alias := incusImageAlias(name)
	outDir, err := os.MkdirTemp("", "dagger-incus-export-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(outDir)

	cmd := exec.CommandContext(ctx, "incus", "image", "export", "local:"+alias, outDir)
	if err := traceexec.Exec(ctx, cmd, telemetry.Encapsulated()); err != nil {
		return fmt.Errorf("incus image export failed: %w", err)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		return err
	}

	var filePath string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filePath != "" {
			return fmt.Errorf("incus image export produced multiple files in %s; unsupported", outDir)
		}
		filePath = filepath.Join(outDir, entry.Name())
	}
	if filePath == "" {
		return fmt.Errorf("incus image export produced no files in %s", outDir)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(tarball, f)
	if err != nil {
		return err
	}
	return nil
}

func incusImageAlias(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return "dagger-" + hex.EncodeToString(sum[:8])
}

type dockerManifestEntry struct {
	Config   string   `json:"Config"`
	Layers   []string `json:"Layers"`
	RepoTags []string `json:"RepoTags"`
}

type dockerImageConfig struct {
	Architecture string     `json:"architecture"`
	OS           string     `json:"os"`
	Created      *time.Time `json:"created"`
}

type imageArchive struct {
	cfg    dockerImageConfig
	layers [][]byte
}

func imageArchiveToIncusTarball(sourcePath, alias string) (_ string, rerr error) {
	archive, err := parseImageArchive(sourcePath)
	if err != nil {
		return "", err
	}

	tempDir, err := os.MkdirTemp("", "dagger-incus-rootfs-*")
	if err != nil {
		return "", err
	}
	rootfsDir := filepath.Join(tempDir, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		return "", err
	}
	defer func() {
		if rerr != nil {
			_ = os.RemoveAll(tempDir)
		}
	}()

	for _, layerBytes := range archive.layers {
		if err := unpackLayer(rootfsDir, layerBytes); err != nil {
			return "", err
		}
	}

	metaPath := filepath.Join(tempDir, "metadata.yaml")
	if err := os.WriteFile(metaPath, []byte(buildMetadataYAML(alias, archive.cfg)), 0o644); err != nil {
		return "", err
	}

	outFile, err := os.CreateTemp("", "dagger-incus-image-*.tar.gz")
	if err != nil {
		return "", err
	}
	defer func() {
		if rerr != nil {
			_ = outFile.Close()
			_ = os.Remove(outFile.Name())
		}
	}()

	gw := gzip.NewWriter(outFile)
	tw := tar.NewWriter(gw)
	if err := writeFileToTar(tw, metaPath, "metadata.yaml"); err != nil {
		return "", err
	}
	if err := filepath.Walk(rootfsDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == rootfsDir {
			return nil
		}
		rel, err := filepath.Rel(rootfsDir, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Join("rootfs", rel))
		if info.IsDir() {
			name += "/"
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	}); err != nil {
		return "", err
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gw.Close(); err != nil {
		return "", err
	}
	if err := outFile.Close(); err != nil {
		return "", err
	}
	return outFile.Name(), nil
}

func parseImageArchive(sourcePath string) (*imageArchive, error) {
	if archive, err := parseOCIImageArchive(sourcePath); err == nil {
		return archive, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	return parseDockerImageArchive(sourcePath)
}

func parseDockerImageArchive(sourcePath string) (*imageArchive, error) {
	var manifestBytes []byte
	if err := readArchiveFile(sourcePath, "manifest.json", &manifestBytes); err != nil {
		return nil, err
	}

	var manifests []dockerManifestEntry
	if err := json.Unmarshal(manifestBytes, &manifests); err != nil {
		return nil, err
	}
	if len(manifests) == 0 {
		return nil, fmt.Errorf("manifest.json in %s is empty", sourcePath)
	}
	manifest := manifests[0]

	var configBytes []byte
	if err := readArchiveFile(sourcePath, manifest.Config, &configBytes); err != nil {
		return nil, err
	}

	cfg, err := parseDockerImageConfig(configBytes)
	if err != nil {
		return nil, err
	}

	layers := make([][]byte, 0, len(manifest.Layers))
	for _, layerName := range manifest.Layers {
		var layerBytes []byte
		if err := readArchiveFile(sourcePath, layerName, &layerBytes); err != nil {
			return nil, err
		}
		layers = append(layers, layerBytes)
	}

	return &imageArchive{cfg: cfg, layers: layers}, nil
}

func parseOCIImageArchive(sourcePath string) (*imageArchive, error) {
	var indexBytes []byte
	if err := readArchiveFile(sourcePath, "index.json", &indexBytes); err != nil {
		return nil, err
	}

	var index ocispecs.Index
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		return nil, err
	}
	if len(index.Manifests) == 0 {
		return nil, fmt.Errorf("index.json in %s is empty", sourcePath)
	}

	return parseOCIManifestDescriptor(sourcePath, index.Manifests[0])
}

func parseOCIManifestDescriptor(sourcePath string, desc ocispecs.Descriptor) (*imageArchive, error) {
	var manifestBytes []byte
	if err := readArchiveFile(sourcePath, filepath.Join("blobs", "sha256", desc.Digest.Encoded()), &manifestBytes); err != nil {
		return nil, err
	}

	var manifest ocispecs.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err == nil && manifest.Config.Digest.String() != "" {
		return parseOCIManifest(sourcePath, manifest)
	}

	var index ocispecs.Index
	if err := json.Unmarshal(manifestBytes, &index); err == nil {
		if len(index.Manifests) == 0 {
			return nil, fmt.Errorf("nested index in %s is empty", sourcePath)
		}
		return parseOCIManifestDescriptor(sourcePath, index.Manifests[0])
	}

	return nil, fmt.Errorf("unsupported OCI manifest blob %s in %s", desc.Digest.String(), sourcePath)
}

func parseOCIManifest(sourcePath string, manifest ocispecs.Manifest) (*imageArchive, error) {
	var configBytes []byte
	if err := readArchiveFile(sourcePath, filepath.Join("blobs", "sha256", manifest.Config.Digest.Encoded()), &configBytes); err != nil {
		return nil, err
	}

	cfg, err := parseDockerImageConfig(configBytes)
	if err != nil {
		return nil, err
	}

	layers := make([][]byte, 0, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		var layerBytes []byte
		if err := readArchiveFile(sourcePath, filepath.Join("blobs", "sha256", layer.Digest.Encoded()), &layerBytes); err != nil {
			return nil, err
		}
		layers = append(layers, layerBytes)
	}

	return &imageArchive{cfg: cfg, layers: layers}, nil
}

func parseDockerImageConfig(configBytes []byte) (dockerImageConfig, error) {
	var cfg dockerImageConfig
	if err := json.Unmarshal(configBytes, &cfg); err != nil {
		return dockerImageConfig{}, err
	}
	return cfg, nil
}

func readArchiveFile(sourcePath, target string, dst *[]byte) error {
	f, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Name == target || filepath.Base(hdr.Name) == target {
			b, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			*dst = b
			return nil
		}
	}
	return fmt.Errorf("file %q not found in %s: %w", target, sourcePath, os.ErrNotExist)
}

func unpackLayer(rootfs string, layerBytes []byte) error {
	r := bytes.NewReader(layerBytes)
	if len(layerBytes) >= 2 && layerBytes[0] == 0x1f && layerBytes[1] == 0x8b {
		gr, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		defer gr.Close()
		return untarInto(rootfs, gr)
	}
	return untarInto(rootfs, r)
}

func untarInto(rootfs string, r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		target := filepath.Join(rootfs, filepath.Clean(hdr.Name))
		base := filepath.Base(hdr.Name)
		dir := filepath.Dir(target)

		switch {
		case strings.HasPrefix(base, ".wh.") && base != ".wh..wh..opq":
			whTarget := filepath.Join(dir, strings.TrimPrefix(base, ".wh."))
			if err := os.RemoveAll(whTarget); err != nil {
				return err
			}
			continue
		case base == ".wh..wh..opq":
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
					return err
				}
			}
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil && !os.IsExist(err) {
				return err
			}
		case tar.TypeLink:
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			if err := os.Link(filepath.Join(rootfs, hdr.Linkname), target); err != nil && !os.IsExist(err) {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			continue
		}
	}
}

func buildMetadataYAML(alias string, cfg dockerImageConfig) string {
	arch := cfg.Architecture
	if arch == "" {
		arch = "amd64"
	}
	created := time.Now().Unix()
	if cfg.Created != nil {
		created = cfg.Created.Unix()
	}

	metadata := map[string]any{
		"architecture": arch,
		"creation_date": created,
		"properties": map[string]string{
			"description": alias,
			"os":          cfg.OS,
		},
	}
	b, _ := yaml.Marshal(metadata)
	return string(b)
}

func writeFileToTar(tw *tar.Writer, sourcePath, targetName string) error {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = targetName
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	f, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}
