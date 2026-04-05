package drivers

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// This file contains various sanity checks for containerBackend - these
// shouldn't be run as part of a regular test run, but are designed to ensure
// that our backends work when hacking on them.

var backends = []struct {
	name    string
	backend containerBackend
}{
	{
		name:    "docker",
		backend: docker{cmd: "docker"},
	},
	{
		name:    "podman",
		backend: docker{cmd: "podman"},
	},
	{
		name:    "nerdctl",
		backend: docker{cmd: "nerdctl"},
	},
	{
		name:    "apple",
		backend: apple{},
	},
	{
		name:    "incus",
		backend: incus{},
	},
}

var _, shouldRun = os.LookupEnv("DRIVER_TEST")

func requireBackendAvailable(t *testing.T, backend containerBackend) {
	t.Helper()

	available, err := backend.Available(t.Context())
	require.NoError(t, err)
	if !available {
		t.Skip("backend not available")
	}
}

func testImageForBackend(name string) string {
	if name == "incus" {
		return "local:test-incus-native:latest"
	}
	return "alpine:3.18"
}

func seedIncusTestImage(t *testing.T, backend containerBackend, imageRef string) {
	t.Helper()

	ctx := t.Context()
	exists, err := backend.ImageExists(ctx, imageRef)
	require.NoError(t, err)
	if exists {
		return
	}

	loadBackend := backend.ImageLoader(ctx)
	require.NotNil(t, loadBackend)
	loader, err := loadBackend.Loader(ctx)
	require.NoError(t, err)

	tarball, err := buildTestDockerArchive(imageRef)
	require.NoError(t, err)

	require.NoError(t, loader.TarballWriter(ctx, imageRef, bytes.NewReader(tarball)))
}

func buildTestDockerArchive(repoTag string) ([]byte, error) {
	var layerBuf bytes.Buffer
	layerTw := tar.NewWriter(&layerBuf)
	payload := []byte("hello from dagger")
	if err := layerTw.WriteHeader(&tar.Header{
		Name: "hello.txt",
		Mode: 0o644,
		Size: int64(len(payload)),
	}); err != nil {
		return nil, err
	}
	if _, err := layerTw.Write(payload); err != nil {
		return nil, err
	}
	if err := layerTw.Close(); err != nil {
		return nil, err
	}

	configBytes, err := json.Marshal(map[string]any{
		"architecture": runtime.GOARCH,
		"os":           runtime.GOOS,
	})
	if err != nil {
		return nil, err
	}

	var tarball bytes.Buffer
	tw := tar.NewWriter(&tarball)
	if err := tw.WriteHeader(&tar.Header{
		Name: "config.json",
		Mode: 0o644,
		Size: int64(len(configBytes)),
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(configBytes); err != nil {
		return nil, err
	}

	if err := tw.WriteHeader(&tar.Header{
		Name: "layer.tar",
		Mode: 0o644,
		Size: int64(layerBuf.Len()),
	}); err != nil {
		return nil, err
	}
	if _, err := io.Copy(tw, bytes.NewReader(layerBuf.Bytes())); err != nil {
		return nil, err
	}

	manifestBytes, err := json.Marshal([]map[string]any{
		{
			"Config":   "config.json",
			"Layers":   []string{"layer.tar"},
			"RepoTags": []string{repoTag},
		},
	})
	if err != nil {
		return nil, err
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "manifest.json",
		Mode: 0o644,
		Size: int64(len(manifestBytes)),
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(manifestBytes); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return tarball.Bytes(), nil
}

func TestBackendImagePullAndExists(t *testing.T) {
	if !shouldRun {
		t.Skip()
	}

	for _, tc := range backends {
		t.Run(tc.name, func(t *testing.T) {
			requireBackendAvailable(t, tc.backend)
			ctx := t.Context()

			testImage := testImageForBackend(tc.name)
			if tc.name == "incus" {
				seedIncusTestImage(t, tc.backend, testImage)
			} else {
				_ = tc.backend.ImageRemove(ctx, testImage)
			}

			existsBefore, err := tc.backend.ImageExists(ctx, testImage)
			require.NoError(t, err)
			if tc.name == "incus" {
				require.True(t, existsBefore)
			} else {
				require.False(t, existsBefore)
			}

			err = tc.backend.ImagePull(ctx, testImage)
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx := context.WithoutCancel(ctx)
				require.NoError(t, tc.backend.ImageRemove(ctx, testImage))
			})

			existsAfter, err := tc.backend.ImageExists(ctx, testImage)
			require.NoError(t, err)
			require.True(t, existsAfter)
		})
	}
}

func TestBackendImageLoadAndExists(t *testing.T) {
	if !shouldRun {
		t.Skip()
	}

	for _, tc := range backends {
		if tc.name == "incus" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			requireBackendAvailable(t, tc.backend)
			ctx := t.Context()

			sourceImage := testImageForBackend(tc.name)
			if tc.name == "incus" {
				seedIncusTestImage(t, tc.backend, sourceImage)
			} else {
				_ = tc.backend.ImageRemove(ctx, sourceImage)
			}
			loadedImageName := "test-loaded-alpine:custom"
			_ = tc.backend.ImageRemove(ctx, loadedImageName)

			pullCmd := exec.CommandContext(ctx, "docker", "pull", sourceImage)
			err := pullCmd.Run()
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx := context.WithoutCancel(ctx)
				rmCmd := exec.CommandContext(ctx, "docker", "image", "rm", sourceImage)
				err := rmCmd.Run()
				require.NoError(t, err)
			})

			saveCmd := exec.CommandContext(ctx, "docker", "save", sourceImage)
			var tarballBuffer bytes.Buffer
			saveCmd.Stdout = &tarballBuffer
			err = saveCmd.Run()
			require.NoError(t, err)

			existsBefore, err := tc.backend.ImageExists(ctx, loadedImageName)
			require.NoError(t, err)
			require.False(t, existsBefore)

			loadBackend := tc.backend.ImageLoader(ctx)
			require.NotNil(t, loadBackend)
			loader, err := loadBackend.Loader(ctx)
			require.NoError(t, err)

			err = loader.TarballWriter(ctx, loadedImageName, bytes.NewReader(tarballBuffer.Bytes()))
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx := context.WithoutCancel(ctx)
				require.NoError(t, tc.backend.ImageRemove(ctx, loadedImageName))
			})

			existsAfter, err := tc.backend.ImageExists(ctx, loadedImageName)
			require.NoError(t, err)
			require.True(t, existsAfter)
		})
	}
}

func TestBackendIncusImageLoadAndExists(t *testing.T) {
	if !shouldRun {
		t.Skip()
	}

	tc := struct {
		name    string
		backend containerBackend
	}{
		name:    "incus",
		backend: incus{},
	}

	t.Run(tc.name, func(t *testing.T) {
		requireBackendAvailable(t, tc.backend)
		ctx := t.Context()

		testImage := testImageForBackend(tc.name)
		seedIncusTestImage(t, tc.backend, testImage)
		loadedImageName := "test-loaded-alpine:custom"
		_ = tc.backend.ImageRemove(ctx, loadedImageName)

		existsBefore, err := tc.backend.ImageExists(ctx, testImage)
		require.NoError(t, err)
		require.True(t, existsBefore)

		err = tc.backend.ImagePull(ctx, testImage)
		require.NoError(t, err)
		t.Cleanup(func() {
			ctx := context.WithoutCancel(ctx)
			require.NoError(t, tc.backend.ImageRemove(ctx, testImage))
			require.NoError(t, tc.backend.ImageRemove(ctx, loadedImageName))
		})

		existsAfter, err := tc.backend.ImageExists(ctx, testImage)
		require.NoError(t, err)
		require.True(t, existsAfter)

		loadBackend := tc.backend.ImageLoader(ctx)
		require.NotNil(t, loadBackend)
		loader, err := loadBackend.Loader(ctx)
		require.NoError(t, err)

		var tarballBuffer bytes.Buffer
		err = loader.TarballReader(ctx, testImage, &tarballBuffer)
		require.NoError(t, err)

		err = loader.TarballWriter(ctx, loadedImageName, bytes.NewReader(tarballBuffer.Bytes()))
		require.NoError(t, err)

		existsLoaded, err := tc.backend.ImageExists(ctx, loadedImageName)
		require.NoError(t, err)
		require.True(t, existsLoaded)
	})
}

func TestBackendContainerRunExec(t *testing.T) {
	if !shouldRun {
		t.Skip()
	}

	for _, tc := range backends {
		t.Run(tc.name, func(t *testing.T) {
			requireBackendAvailable(t, tc.backend)
			ctx := t.Context()
			containerName := "test-run-exec-container"
			testImage := testImageForBackend(tc.name)
			if tc.name == "incus" {
				seedIncusTestImage(t, tc.backend, testImage)
			} else {
				_ = tc.backend.ImageRemove(ctx, testImage)
			}

			_ = tc.backend.ContainerRemove(ctx, containerName)
			err := tc.backend.ImagePull(ctx, testImage)
			require.NoError(t, err)

			t.Cleanup(func() {
				ctx := context.WithoutCancel(ctx)
				require.NoError(t, tc.backend.ImageRemove(ctx, testImage))
			})

			existsBefore, err := tc.backend.ContainerExists(ctx, containerName)
			require.NoError(t, err)
			require.False(t, existsBefore)

			runOpts := runOpts{
				image: testImage,
				args:  []string{"sleep", "30"},
			}
			err = tc.backend.ContainerRun(ctx, containerName, runOpts)
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx := context.WithoutCancel(ctx)
				require.NoError(t, tc.backend.ContainerRemove(ctx, containerName))
			})

			existsAfter, err := tc.backend.ContainerExists(ctx, containerName)
			require.NoError(t, err)
			require.True(t, existsAfter)

			execArgs := []string{"echo", "hello world"}
			stdout, stderr, err := tc.backend.ContainerExec(ctx, containerName, execArgs)
			require.NoError(t, err)
			require.Equal(t, "hello world", stdout)
			require.Empty(t, stderr)

			execArgs2 := []string{"whoami"}
			stdout2, stderr2, err := tc.backend.ContainerExec(ctx, containerName, execArgs2)
			require.NoError(t, err)
			require.True(t, strings.Contains(stdout2, "root"))
			require.Empty(t, stderr2)
		})
	}
}

func TestBackendContainerLs(t *testing.T) {
	if !shouldRun {
		t.Skip()
	}

	for _, tc := range backends {
		t.Run(tc.name, func(t *testing.T) {
			requireBackendAvailable(t, tc.backend)
			ctx := t.Context()
			testImage := testImageForBackend(tc.name)
			if tc.name == "incus" {
				seedIncusTestImage(t, tc.backend, testImage)
			} else {
				_ = tc.backend.ImageRemove(ctx, testImage)
			}
			containers := []string{"test-ls-container-1", "test-ls-container-2"}

			for _, containerName := range containers {
				_ = tc.backend.ContainerRemove(ctx, containerName)
			}

			err := tc.backend.ImagePull(ctx, testImage)
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx := context.WithoutCancel(ctx)
				require.NoError(t, tc.backend.ImageRemove(ctx, testImage))
			})

			initialList, err := tc.backend.ContainerLs(ctx)
			require.NoError(t, err)
			initialCount := len(initialList)

			// Create multiple containers
			for _, containerName := range containers {
				runOpts := runOpts{
					image: testImage,
					args:  []string{"sleep", "30"},
				}
				err = tc.backend.ContainerRun(ctx, containerName, runOpts)
				require.NoError(t, err)
			}
			t.Cleanup(func() {
				ctx := context.WithoutCancel(ctx)
				for _, containerName := range containers {
					require.NoError(t, tc.backend.ContainerRemove(ctx, containerName))
				}
			})

			finalList, err := tc.backend.ContainerLs(ctx)
			require.NoError(t, err)
			require.Equal(t, initialCount+len(containers), len(finalList))

			for _, containerName := range containers {
				found := false
				for _, listedContainer := range finalList {
					if strings.Contains(listedContainer, containerName) {
						found = true
						break
					}
				}
				require.True(t, found, "Container %s should be in the list", containerName)
			}
		})
	}
}

func TestBackendContainerRunWithOptions(t *testing.T) {
	if !shouldRun {
		t.Skip()
	}

	for _, tc := range backends {
		t.Run(tc.name, func(t *testing.T) {
			requireBackendAvailable(t, tc.backend)
			ctx := t.Context()
			containerName := "test-run-options-container"
			testImage := testImageForBackend(tc.name)
			if tc.name == "incus" {
				seedIncusTestImage(t, tc.backend, testImage)
			} else {
				_ = tc.backend.ImageRemove(ctx, testImage)
			}

			_ = tc.backend.ContainerRemove(ctx, containerName)

			err := tc.backend.ImagePull(ctx, testImage)
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx := context.WithoutCancel(ctx)
				require.NoError(t, tc.backend.ImageRemove(ctx, testImage))
			})

			runOpts := runOpts{
				image: testImage,
				env:   []string{"TEST_VAR=hello", "ANOTHER_VAR=world"},
				args:  []string{"sleep", "30"},
			}
			err = tc.backend.ContainerRun(ctx, containerName, runOpts)
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx := context.WithoutCancel(ctx)
				require.NoError(t, tc.backend.ContainerRemove(ctx, containerName))
			})

			stdout, _, err := tc.backend.ContainerExec(ctx, containerName, []string{"env"})
			require.NoError(t, err)
			require.Contains(t, stdout, "TEST_VAR=hello")
			require.Contains(t, stdout, "ANOTHER_VAR=world")
		})
	}
}
