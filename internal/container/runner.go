package container

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/controlplaneio/simulator/controlplane"
	"github.com/controlplaneio/simulator/controlplane/aws"
	"github.com/controlplaneio/simulator/internal/config"
)

var (
	NoHome       = errors.New("unable to determine your home directory")
	NoClient     = errors.New("unable to create docker client")
	CreateFailed = errors.New("unable to create simulator container")
	StartFailed  = errors.New("unable to start simulator container")
	AttachFailed = errors.New("unable to attach to simulator container")

	containerAwsDir = "/home/ubuntu/.aws"
)

type Simulator interface {
	Run(ctx context.Context, command []string) error
}

func New(config *config.Config) Simulator {
	return &simulator{
		Config: config,
	}
}

type simulator struct {
	Config *config.Config
}

func (r simulator) Run(ctx context.Context, command []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return NoHome
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return NoClient
	}

	mounts := []mount.Mount{
		{
			Type:     mount.TypeBind,
			Source:   filepath.Join(r.Config.BaseDir, controlplane.Home),
			Target:   controlplane.HomeDir,
			ReadOnly: false,
		},
		{
			Type:   mount.TypeBind,
			Source: filepath.Join(home, ".aws"),
			Target: containerAwsDir,
		},
	}

	if r.Config.Cli.Dev {
		mounts = append(mounts, []mount.Mount{
			{
				Type:   mount.TypeBind,
				Source: filepath.Join(r.Config.BaseDir, controlplane.Scenarios),
				Target: controlplane.AnsibleDir,
			},
			{
				Type:   mount.TypeBind,
				Source: filepath.Join(r.Config.BaseDir, controlplane.Packer),
				Target: controlplane.PackerTemplateDir,
			},
			{
				Type:     mount.TypeBind,
				Source:   filepath.Join(r.Config.BaseDir, controlplane.Terraform),
				Target:   controlplane.TerraformDir,
				ReadOnly: false,
			},
		}...)
	}

	cont, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:        r.Config.Container.Image,
			Env:          aws.Env,
			Cmd:          command,
			Tty:          true,
			AttachStdout: true,
			AttachStderr: true,
		},
		&container.HostConfig{
			Mounts: mounts,
		},
		&network.NetworkingConfig{},
		&v1.Platform{},
		"",
	)
	if err != nil {
		return CreateFailed
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		err = cli.ContainerStop(ctx, cont.ID, container.StopOptions{})
		if err != nil {
			slog.Warn("failed to stop container", "id", cont.ID, "err", err)
		}

		err = cli.ContainerRemove(ctx, cont.ID, types.ContainerRemoveOptions{})
		if err != nil {
			slog.Warn("failed to remove container", "id", cont.ID, "err", err)
		}
	}()

	hijack, err := cli.ContainerAttach(ctx, cont.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return AttachFailed
	}

	err = cli.ContainerStart(ctx, cont.ID, types.ContainerStartOptions{})
	if err != nil {
		return StartFailed
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		_, _ = io.Copy(os.Stdout, hijack.Reader)
		defer wg.Done()
	}()

	wg.Wait()

	return nil
}
