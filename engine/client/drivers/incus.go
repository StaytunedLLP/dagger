package drivers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/adrg/xdg"
	"github.com/dagger/dagger/engine/client/imageload"
	"github.com/dagger/dagger/engine/distconsts"
	"github.com/dagger/dagger/util/traceexec"
	telemetry "github.com/dagger/otel-go"
	"github.com/docker/cli/cli/connhelper/commandconn"
)

type incus struct{}

var _ containerBackend = incus{}

const incusDockerRemote = "docker"
const incusDockerRemoteURL = "https://docker.io"
const incusDockerRemoteProtocol = "oci"

var incusHostStateDir = filepath.Join(xdg.DataHome, "dagger", "incus")

type incusRemote struct {
	Protocol string `json:"protocol"`
	Addr     string `json:"Addr"`
}

func (incus) Available(ctx context.Context) (bool, error) {
	if _, err := exec.LookPath("incus"); err != nil {
		return false, nil //nolint:nilerr
	}
	if err := traceexec.Exec(ctx, exec.CommandContext(ctx, "incus", "info"), telemetry.Encapsulated()); err == nil {
		return true, nil
	}

	stdout, _, err := traceexec.ExecOutput(ctx, exec.CommandContext(ctx, "incus", "remote", "get-default"), telemetry.Encapsulated())
	if err != nil {
		return false, nil // treat a non-running or unreachable daemon as unavailable
	}

	remote := strings.TrimSpace(stdout)
	if remote == "" {
		return false, nil
	}

	cmd := exec.CommandContext(ctx, "incus", "info", remote+":")
	if err := traceexec.Exec(ctx, cmd, telemetry.Encapsulated()); err != nil {
		return false, nil // treat a non-running or unreachable daemon as unavailable
	}
	return true, nil
}

func (incus) ImagePull(ctx context.Context, image string) error {
	source, needsDockerRemote := incusRemoteImageRef(image)
	if needsDockerRemote {
		if err := pullIncusDockerImage(ctx, source); err != nil {
			return err
		}
		return nil
	}

	alias := incusImageAlias(image)
	exists, err := incusImageExists(ctx, incusLocalImageRef(ctx, alias))
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	args := []string{"image", "copy", source, incusImageCopyTarget(ctx), "--alias", alias}
	return traceexec.Exec(ctx, exec.CommandContext(ctx, "incus", args...), telemetry.Encapsulated())
}

func (incus) ImageExists(ctx context.Context, image string) (bool, error) {
	return incusImageExists(ctx, incusLocalImageRef(ctx, incusImageAlias(image)))
}

func (incus) ImageRemove(ctx context.Context, image string) error {
	return traceexec.Exec(ctx, exec.CommandContext(ctx, "incus", "image", "delete", incusLocalImageRef(ctx, incusImageAlias(image))))
}

func (incus) ImageLoader(ctx context.Context) imageload.Backend {
	return imageload.Incus{}
}

func (incus) ContainerRun(ctx context.Context, name string, opts runOpts) (err error) {
	if opts.gpus {
		return fmt.Errorf("incus backend does not currently support GPU passthrough")
	}
	if err := os.MkdirAll(incusHostStateDir, 0o755); err != nil {
		return err
	}

	alias := incusImageAlias(opts.image)
	exists, err := incusImageExists(ctx, incusLocalImageRef(ctx, alias))
	if err != nil {
		return err
	}
	if !exists {
		if err := (incus{}).ImagePull(ctx, opts.image); err != nil {
			return err
		}
	}

	profileName := incusProfileName(name)
	if err := ensureIncusRunProfile(ctx, profileName, name, opts); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = traceexec.Exec(context.WithoutCancel(ctx), exec.CommandContext(context.WithoutCancel(ctx), "incus", "profile", "delete", profileName))
		}
	}()

	args := []string{"launch", incusLocalImageRef(ctx, alias), incusInstanceRef(ctx, name), "--profile", profileName}
	args = append(args, "-c", "security.nesting=true")
	if opts.privileged {
		args = append(args, "-c", "security.privileged=true")
	}
	if opts.cpus != "" {
		args = append(args, "-c", "limits.cpu="+opts.cpus)
	}
	if opts.memory != "" {
		args = append(args, "-c", "limits.memory="+opts.memory)
	}

	for _, env := range opts.env {
		k, v, ok := strings.Cut(env, "=")
		if !ok {
			v = ""
		}
		args = append(args, "-c", "environment."+k+"="+v)
	}

	if len(opts.args) > 0 {
		entrypoint := opts.args
		if strings.Contains(opts.image, "registry.dagger.io/engine") {
			quotedArgs := make([]string, 0, len(opts.args))
			for _, arg := range opts.args {
				quotedArgs = append(quotedArgs, shellQuote(arg))
			}
			script := "mkdir -p /etc && touch /etc/resolv.conf && exec /usr/local/bin/dagger-entrypoint.sh"
			if len(quotedArgs) > 0 {
				script += " " + strings.Join(quotedArgs, " ")
			}
			entrypoint = []string{"/bin/sh", "-lc", shellQuote(script)}
		} else if entrypoint[0] == "dagger-entrypoint.sh" {
			entrypoint[0] = "/usr/local/bin/dagger-entrypoint.sh"
		}
		args = append(args, "-c", "raw.lxc=lxc.init.cmd="+strings.Join(entrypoint, " "))
	}

	cmd := exec.CommandContext(ctx, "incus", args...)
	_, stderr, err := traceexec.ExecOutput(ctx, cmd, telemetry.Encapsulated())
	if err != nil {
		if isIncusAlreadyExistsOutput(stderr) {
			return errContainerAlreadyExists
		}
		return err
	}
	return nil
}

func (incus) ContainerExec(ctx context.Context, name string, args []string) (string, string, error) {
	cmdArgs := append([]string{"exec", "-T", incusInstanceRef(ctx, name), "--"}, args...)
	return traceexec.ExecOutput(ctx, exec.CommandContext(ctx, "incus", cmdArgs...))
}

func (incus) ContainerDial(ctx context.Context, name string, args []string) (net.Conn, error) {
	cmdArgs := append([]string{"exec", "-T", incusInstanceRef(ctx, name), "--"}, args...)
	return commandconn.New(ctx, "incus", cmdArgs...)
}

func (incus) ContainerRemove(ctx context.Context, name string) error {
	if err := traceexec.Exec(ctx, exec.CommandContext(ctx, "incus", "delete", "-f", incusInstanceRef(ctx, name))); err != nil {
		return err
	}
	return traceexec.Exec(ctx, exec.CommandContext(ctx, "incus", "profile", "delete", incusProfileName(name)))
}

func (i incus) ContainerStart(ctx context.Context, name string) error {
	running, err := i.containerIsRunning(ctx, name)
	if err != nil {
		return err
	}
	if running {
		return nil
	}
	return traceexec.Exec(ctx, exec.CommandContext(ctx, "incus", "start", incusInstanceRef(ctx, name)), telemetry.Encapsulated())
}

func (incus) ContainerExists(ctx context.Context, name string) (bool, error) {
	_, stderr, err := traceexec.ExecOutput(ctx, exec.CommandContext(ctx, "incus", "info", incusInstanceRef(ctx, name)), telemetry.Encapsulated())
	if err == nil {
		return true, nil
	}
	if isIncusNotFoundOutput(stderr) {
		return false, nil
	}
	return false, err
}

func (incus) ContainerLs(ctx context.Context) ([]string, error) {
	args := []string{"list"}
	if remote := incusRemoteTarget(ctx); len(remote) > 0 {
		args = append(args, remote...)
	}
	args = append(args, "--format", "json")
	stdout, _, err := traceexec.ExecOutput(ctx, exec.CommandContext(ctx, "incus", args...))
	if err != nil {
		return nil, err
	}
	var result []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(result))
	for _, res := range result {
		if res.Name != "" {
			ids = append(ids, res.Name)
		}
	}
	return ids, nil
}

func (incus) ContainerReady(ctx context.Context, name string, opts runOpts) error {
	probe := []string{"sh", "-ec", readinessProbeCommand(opts)}
	readyCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var lastErr error
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()

	for i := 0; i < 80; i++ {
		_ = i
		_, _, err := traceexec.ExecOutput(readyCtx, exec.CommandContext(readyCtx, "incus", append([]string{"exec", "-T", incusInstanceRef(readyCtx, name), "--"}, probe...)...))
		if err == nil {
			return nil
		}
		lastErr = err

		select {
		case <-readyCtx.Done():
			return fmt.Errorf("timed out waiting for engine container %q to become ready: %w", name, lastErr)
		case <-ticker.C:
		}
	}

	return fmt.Errorf("timed out waiting for engine container %q to become ready: %w", name, lastErr)
}

func (i incus) containerIsRunning(ctx context.Context, name string) (bool, error) {
	stdout, _, err := traceexec.ExecOutput(ctx, exec.CommandContext(ctx, "incus", "list", incusInstanceRef(ctx, name), "--format", "json"))
	if err != nil {
		return false, err
	}
	var result []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		return false, err
	}
	for _, res := range result {
		if res.Name == name {
			return strings.EqualFold(res.Status, "running"), nil
		}
	}
	return false, nil
}

func incusImageAlias(image string) string {
	sum := sha256.Sum256([]byte(image))
	return "dagger-" + hex.EncodeToString(sum[:8])
}

func readinessProbeCommand(opts runOpts) string {
	addr := distconsts.DefaultEngineSockAddr
	if opts.port != 0 {
		addr = fmt.Sprintf("tcp://127.0.0.1:%d", opts.port)
	}

	return fmt.Sprintf(`if command -v buildctl >/dev/null 2>&1; then
		buildctl --addr %s debug workers >/dev/null 2>&1
	else
		test -S /run/dagger/engine.sock
	fi`, shellQuote(addr))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func incusRemoteImageRef(image string) (string, bool) {
	if strings.Contains(image, "://") || strings.HasPrefix(image, "local:") || strings.HasPrefix(image, "docker:") || strings.HasPrefix(image, "images:") {
		return image, false
	}
	return "docker:" + image, true
}

func incusImageExists(ctx context.Context, alias string) (bool, error) {
	_, stderr, err := traceexec.ExecOutput(ctx, exec.CommandContext(ctx, "incus", "image", "info", alias), telemetry.Encapsulated())
	if err == nil {
		return true, nil
	}
	if isIncusNotFoundOutput(stderr) {
		return false, nil
	}
	return false, err
}

func ensureIncusDockerRemote(ctx context.Context) error {
	stdout, _, err := traceexec.ExecOutput(ctx, exec.CommandContext(ctx, "incus", "remote", "list", "--format", "json"))
	if err == nil {
		var remotes map[string]incusRemote
		if json.Unmarshal([]byte(stdout), &remotes) == nil {
			if remote, ok := remotes[incusDockerRemote]; ok {
				if isExpectedIncusDockerRemote(remote) {
					return nil
				}
				return fmt.Errorf("incus remote %q already exists but with different configuration: protocol=%q addr=%q", incusDockerRemote, remote.Protocol, remote.Addr)
			}
		}
	}

	cmd := exec.CommandContext(ctx, "incus", "remote", "add", incusDockerRemote, incusDockerRemoteURL, "--protocol="+incusDockerRemoteProtocol)
	_, stderr, err := traceexec.ExecOutput(ctx, cmd, telemetry.Encapsulated())
	if err != nil {
		stderrLower := strings.ToLower(stderr)
		if !strings.Contains(stderrLower, "already exists") && !strings.Contains(stderrLower, "exists as") {
			return err
		}
		stdout, _, err := traceexec.ExecOutput(ctx, exec.CommandContext(ctx, "incus", "remote", "list", "--format", "json"))
		if err != nil {
			return err
		}
		var remotes map[string]incusRemote
		if err := json.Unmarshal([]byte(stdout), &remotes); err != nil {
			return err
		}
		if remote, ok := remotes[incusDockerRemote]; ok {
			if isExpectedIncusDockerRemote(remote) {
				return nil
			}
			return fmt.Errorf("incus remote %q already exists but with different configuration: protocol=%q addr=%q", incusDockerRemote, remote.Protocol, remote.Addr)
		}
		return fmt.Errorf("incus remote %q already exists but could not be verified", incusDockerRemote)
	}
	return nil
}

func isExpectedIncusDockerRemote(remote incusRemote) bool {
	return strings.EqualFold(remote.Protocol, incusDockerRemoteProtocol) &&
		(remote.Addr == incusDockerRemoteURL || remote.Addr == "docker.io")
}

func incusConfigDir() (string, bool, error) {
	dir := filepath.Join(xdg.ConfigHome, "dagger")
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return dir, true, nil
}

func incusStateVolumeDir(name string) (string, error) {
	dir := filepath.Join(incusHostStateDir, "volumes", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func isIncusAlreadyExistsOutput(output string) bool {
	output = strings.ToLower(output)
	return strings.Contains(output, "already exists") || strings.Contains(output, "instance already exists")
}

func isIncusNotFoundOutput(output string) bool {
	output = strings.ToLower(output)
	return strings.Contains(output, "not found") || strings.Contains(output, "not found in project")
}

func incusRemoteTarget(ctx context.Context) []string {
	if remote, err := incusDefaultRemote(ctx); err == nil && remote != "" && remote != "local" {
		return []string{remote + ":"}
	}
	return nil
}

func incusDefaultRemote(ctx context.Context) (string, error) {
	stdout, _, err := traceexec.ExecOutput(ctx, exec.CommandContext(ctx, "incus", "remote", "get-default"), telemetry.Encapsulated())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

func incusRemotePrefix(ctx context.Context) string {
	if remote, err := incusDefaultRemote(ctx); err == nil && remote != "" && remote != "local" {
		return remote + ":"
	}
	return ""
}

func incusImageCopyTarget(ctx context.Context) string {
	if prefix := incusRemotePrefix(ctx); prefix != "" {
		return prefix
	}
	return "local:"
}

func incusLocalImageRef(ctx context.Context, alias string) string {
	if prefix := incusRemotePrefix(ctx); prefix != "" {
		return prefix + alias
	}
	return "local:" + alias
}

func incusInstanceRef(ctx context.Context, name string) string {
	return incusRemotePrefix(ctx) + name
}

func incusProfileName(name string) string {
	return "dagger-" + name
}

func pullIncusDockerImage(ctx context.Context, source string) error {
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "amd64"
	}

	tarball, err := os.CreateTemp("", "dagger-incus-docker-*.tar")
	if err != nil {
		return err
	}
	defer func() {
		_ = tarball.Close()
		_ = os.Remove(tarball.Name())
	}()

	if err := traceexec.Exec(ctx, exec.CommandContext(ctx, "skopeo", "copy", "--override-os", "linux", "--override-arch", arch, "docker://"+strings.TrimPrefix(source, "docker:"), "docker-archive:"+tarball.Name()), telemetry.Encapsulated()); err != nil {
		return err
	}

	if err := tarball.Close(); err != nil {
		return err
	}

	f, err := os.Open(tarball.Name())
	if err != nil {
		return err
	}
	defer f.Close()

	loader, err := (imageload.Incus{}).Loader(ctx)
	if err != nil {
		return err
	}
	return loader.TarballWriter(ctx, strings.TrimPrefix(source, "docker:"), f)
}

func ensureIncusRunProfile(ctx context.Context, profileName, name string, opts runOpts) error {
	_ = traceexec.Exec(ctx, exec.CommandContext(ctx, "incus", "profile", "delete", profileName))

	if err := traceexec.Exec(ctx, exec.CommandContext(ctx, "incus", "profile", "create", profileName)); err != nil {
		return err
	}

	addDevice := func(deviceName, deviceType string, deviceArgs ...string) error {
		args := append([]string{"profile", "device", "add", profileName, deviceName, deviceType}, deviceArgs...)
		return traceexec.Exec(ctx, exec.CommandContext(ctx, "incus", args...))
	}

	if err := addDevice("root", "disk", "pool=default", "path=/"); err != nil {
		return err
	}

	for _, port := range opts.ports {
		hostPort, containerPort, ok := strings.Cut(port, ":")
		if !ok {
			hostPort = port
			containerPort = port
		}
		if err := addDevice("dagger-port-"+hostPort, "proxy", "listen=tcp:127.0.0.1:"+hostPort, "connect=tcp:127.0.0.1:"+containerPort); err != nil {
			return err
		}
	}

	return nil
}
