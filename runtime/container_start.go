package runtime

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"os"

	"sync"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
	"github.com/rancher/agent/host_info"
	"github.com/rancher/agent/progress"
	"github.com/rancher/agent/utils"
	v3 "github.com/rancher/go-rancher/v3"
)

const (
	PullImageLabels = "io.rancher.container.pull_image"
	nameInuseError  = "You have to remove (or rename) that container to be able to reuse that name"
)

var (
	dockerRootOnce = sync.Once{}
	dockerRoot     = ""
	HTTPProxyList  = []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "no_proxy", "NO_PROXY"}
)

func ContainerStart(containerSpec v3.Container, volumes []v3.Volume, networkKind string, credentials []v3.Credential, progress *progress.Progress, runtimeClient *client.Client, idsMap map[string]string) (string, error) {
	started := false

	// setup name
	parts := strings.Split(containerSpec.Uuid, "-")
	if len(parts) == 0 {
		return "", errors.New("Failed to parse UUID")
	}
	name := fmt.Sprintf("r-%s", containerSpec.Uuid)
	if str := utils.NameRegexCompiler.FindString(containerSpec.Name); str != "" {
		name = fmt.Sprintf("r-%s-%s", containerSpec.Name, parts[0])
	}

	// creating managed volumes
	rancherBindMounts, err := setupRancherFlexVolume(volumes, containerSpec.DataVolumes, progress)
	if err != nil {
		return "", errors.Wrap(err, "failed to set up rancher flex volumes")
	}

	// make sure managed volumes are unmounted if container is not started
	defer func() {
		if !started {
			unmountRancherFlexVolume(volumes)
		}
	}()

	// setup container spec(config and hostConfig)
	spec, err := setupContainerSpec(containerSpec, volumes, networkKind, rancherBindMounts, runtimeClient, progress, idsMap)
	if err != nil {
		return "", errors.Wrap(err, "failed to generate container spec")
	}

	containerID, err := utils.FindContainer(runtimeClient, containerSpec, false)
	if err != nil && !utils.IsContainerNotFoundError(err) {
		return "", errors.Wrap(err, "failed to get container")
	}
	created := false
	if containerID == "" {
		credential := v3.Credential{}
		for _, cred := range credentials {
			if cred.Id == containerSpec.RegistryCredentialId {
				credential = cred
				break
			}
		}
		newID, err := createContainer(runtimeClient, &spec.config, &spec.hostConfig, containerSpec, credential, name, progress)
		if err != nil && !strings.Contains(err.Error(), nameInuseError) {
			return "", errors.Wrap(err, "failed to create container")
		}
		if newID == "" {
			contID, err := utils.FindContainer(runtimeClient, containerSpec, true)
			if err != nil && !utils.IsContainerNotFoundError(err) {
				return "", errors.Wrap(err, "failed to get container")
			}
			containerID = contID
		} else {
			containerID = newID
		}

		created = true
	}

	startErr := utils.Serialize(func() error {
		return runtimeClient.ContainerStart(context.Background(), containerID, types.ContainerStartOptions{})
	})
	if startErr != nil {
		if created {
			if err := utils.RemoveContainer(runtimeClient, containerID); err != nil {
				return "", errors.Wrapf(err, "failed to remove container: failed to start: %v", startErr)
			}
		}
		return "", errors.Wrap(startErr, "failed to start container")
	}

	logrus.Infof("rancher id [%v]: Container [%v] with docker id [%v] has been started", containerSpec.Id, containerSpec.Name, containerID)
	started = true
	return containerID, nil
}

func IsContainerStarted(containerSpec v3.Container, client *client.Client) (bool, bool, error) {
	cont, err := utils.FindContainer(client, containerSpec, false)
	if err != nil {
		if utils.IsContainerNotFoundError(err) {
			return false, false, nil
		}
		return false, false, errors.Wrap(err, "failed to get container")
	}
	return isRunning(client, cont)
}

type dockerContainerSpec struct {
	config     container.Config
	hostConfig container.HostConfig
}

func setupContainerSpec(containerSpec v3.Container, volumes []v3.Volume, networkKind string, rancherBindMounts []string, runtimeClient *client.Client, progress *progress.Progress, idsMap map[string]string) (dockerContainerSpec, error) {
	config := container.Config{
		OpenStdin: true,
	}
	hostConfig := container.HostConfig{
		PublishAllPorts: false,
		Privileged:      containerSpec.Privileged,
		ReadonlyRootfs:  containerSpec.ReadOnly,
	}

	initializeMaps(&config, &hostConfig)

	setupLabels(containerSpec, &config)

	setupFieldsHostConfig(containerSpec, &hostConfig)

	setupFieldsConfig(containerSpec, &config)

	setupPublishPorts(&hostConfig, containerSpec)

	if err := setupDNSSearch(&hostConfig, containerSpec); err != nil {
		return dockerContainerSpec{}, errors.Wrap(err, "failed to set up DNS search")
	}

	setupHostname(&config, containerSpec)

	setupPorts(&config, containerSpec, &hostConfig)

	hostConfig.Binds = append(hostConfig.Binds, rancherBindMounts...)

	if err := setupNonRancherVolumes(&config, volumes, containerSpec, &hostConfig, runtimeClient, progress, idsMap); err != nil {
		return dockerContainerSpec{}, errors.Wrap(err, "failed to set up volumes")
	}

	if err := setupNetworking(containerSpec, &config, &hostConfig, idsMap, networkKind); err != nil {
		return dockerContainerSpec{}, errors.Wrap(err, "failed to set up networking")
	}

	setupDeviceOptions(&hostConfig, containerSpec)

	setupComputeResourceFields(&hostConfig, containerSpec)

	setupHealthConfig(containerSpec, &config)
	return dockerContainerSpec{
		config:     config,
		hostConfig: hostConfig,
	}, nil
}

type PullParams struct {
	Tag       string
	Mode      string
	Complete  bool
	ImageUUID string
}

func createContainer(dockerClient *client.Client, config *container.Config, hostConfig *container.HostConfig, containerSpec v3.Container, credential v3.Credential, name string, progress *progress.Progress) (string, error) {
	labels := config.Labels
	if labels[PullImageLabels] == "always" {
		params := PullParams{
			Tag:       "",
			Mode:      "all",
			Complete:  false,
			ImageUUID: containerSpec.Image,
		}
		_, err := DoInstancePull(params, progress, dockerClient, credential)
		if err != nil {
			return "", errors.Wrap(err, "failed to pull instance")
		}
	}
	config.Image = containerSpec.Image

	if containerSpec.ExternalId != "" {
		return "", fmt.Errorf("Container %s has been deleted from the host", containerSpec.ExternalId)
	}

	containerResponse, err := dockerContainerCreate(context.Background(), dockerClient, config, hostConfig, name)
	// if image doesn't exist
	if client.IsErrImageNotFound(err) {
		if err := ImagePull(progress, dockerClient, containerSpec.Image, credential); err != nil {
			return "", errors.Wrap(err, "failed to pull image")
		}
		containerResponse, err1 := dockerContainerCreate(context.Background(), dockerClient, config, hostConfig, name)
		if err1 != nil {
			return "", errors.Wrap(err1, "failed to create container")
		}
		return containerResponse.ID, nil
	} else if err != nil {
		return "", errors.Wrap(err, "failed to create container")
	}
	return containerResponse.ID, nil
}

func initializeMaps(config *container.Config, hostConfig *container.HostConfig) {
	config.Labels = make(map[string]string)
	config.Volumes = make(map[string]struct{})
	config.ExposedPorts = make(map[nat.Port]struct{})
	hostConfig.PortBindings = make(map[nat.Port][]nat.PortBinding)
	hostConfig.StorageOpt = make(map[string]string)
	hostConfig.Tmpfs = make(map[string]string)
	hostConfig.Sysctls = make(map[string]string)
}

func setupHostname(config *container.Config, containerSpec v3.Container) {
	config.Hostname = containerSpec.Hostname
}

func setupPorts(config *container.Config, containerSpec v3.Container, hostConfig *container.HostConfig) {
	//ports := []types.Port{}
	exposedPorts := map[nat.Port]struct{}{}
	bindings := nat.PortMap{}
	for _, endpoint := range containerSpec.PublicEndpoints {
		if endpoint.PrivatePort != 0 {
			bind := nat.Port(fmt.Sprintf("%v/%v", endpoint.PrivatePort, endpoint.Protocol))
			bindAddr := endpoint.BindIpAddress
			if _, ok := bindings[bind]; !ok {
				bindings[bind] = []nat.PortBinding{
					{
						HostIP:   bindAddr,
						HostPort: strconv.Itoa(int(endpoint.PublicPort)),
					},
				}
			} else {
				bindings[bind] = append(bindings[bind], nat.PortBinding{
					HostIP:   bindAddr,
					HostPort: strconv.Itoa(int(endpoint.PublicPort)),
				})
			}
			exposedPorts[bind] = struct{}{}
		}

	}

	config.ExposedPorts = exposedPorts

	if len(bindings) > 0 {
		hostConfig.PortBindings = bindings
	}
}

func getDockerRoot(client *client.Client) string {
	dockerRootOnce.Do(func() {
		info, err := client.Info(context.Background())
		if err != nil {
			panic(err.Error())
		}
		dockerRoot = info.DockerRootDir
	})
	return dockerRoot
}

// setupVolumes volumes except rancher specific volumes. For rancher-managed volume driver they will be setup through special steps like flexvolume
func setupNonRancherVolumes(config *container.Config, volumes []v3.Volume, containerSpec v3.Container, hostConfig *container.HostConfig, client *client.Client, progress *progress.Progress, idsMap map[string]string) error {
	volumesMap := map[string]struct{}{}
	binds := []string{}

	rancherManagedVolumeNames := map[string]struct{}{}
	for _, volume := range volumes {
		if IsRancherVolume(volume) {
			rancherManagedVolumeNames[volume.Name] = struct{}{}
		}
	}

	for _, volume := range containerSpec.DataVolumes {
		parts := strings.SplitN(volume, ":", 3)
		// don't set rancher managed volume
		if _, ok := rancherManagedVolumeNames[parts[0]]; ok {
			continue
		}
		if len(parts) == 1 {
			volumesMap[parts[0]] = struct{}{}
		} else if len(parts) > 1 {
			volumesMap[parts[1]] = struct{}{}
			mode := ""
			if len(parts) == 3 {
				mode = parts[2]
			} else {
				mode = "rw"
			}

			// Redirect /var/lib/docker:/var/lib/docker to where Docker root really is
			if parts[0] == "/var/lib/docker" && parts[1] == "/var/lib/docker" {
				root := getDockerRoot(client)
				if root != "/var/lib/docker" {
					volumesMap[root] = struct{}{}
					binds = append(binds, fmt.Sprintf("%s:%s:%s", root, parts[1], mode))
					binds = append(binds, fmt.Sprintf("%s:%s:%s", root, root, mode))
					continue
				}
			}

			bind := fmt.Sprintf("%s:%s:%s", parts[0], parts[1], mode)
			binds = append(binds, bind)
		}
	}
	config.Volumes = volumesMap
	hostConfig.Binds = append(hostConfig.Binds, binds...)

	containers := []string{}
	if containerSpec.DataVolumesFrom != nil {
		for _, volumeFrom := range containerSpec.DataVolumesFrom {
			if idsMap[volumeFrom] != "" {
				containers = append(containers, idsMap[volumeFrom])
			}
		}
		if len(containers) > 0 {
			hostConfig.VolumesFrom = containers
		}
	}

	for _, volume := range volumes {
		// volume active == exists, possibly not attached to this host
		if !IsRancherVolume(volume) {
			if ok, err := IsVolumeActive(volume, client); !ok && err == nil {
				if err := DoVolumeActivate(volume, client, progress); err != nil {
					return errors.Wrap(err, "failed to activate volume")
				}
			} else if err != nil {
				return errors.Wrap(err, "failed to check whether volume is activated")
			}
		}
	}

	return nil
}

func setupHealthConfig(spec v3.Container, config *container.Config) {
	healthConfig := &container.HealthConfig{}
	healthConfig.Test = spec.HealthCmd
	healthConfig.Interval = time.Duration(spec.HealthInterval) * time.Second
	healthConfig.Retries = int(spec.HealthRetries)
	healthConfig.Timeout = time.Duration(spec.HealthTimeout) * time.Second
	config.Healthcheck = healthConfig
}

func setupLabels(spec v3.Container, config *container.Config) {
	for k, v := range spec.Labels {
		config.Labels[k] = v
	}

	config.Labels[utils.UUIDLabel] = spec.Uuid
}

// this method convert fields data to fields in configuration
func setupFieldsConfig(spec v3.Container, config *container.Config) {

	config.Cmd = spec.Command

	envs := []string{}
	for k, v := range spec.Environment {
		envs = append(envs, fmt.Sprintf("%v=%v", k, v))
	}
	config.Env = append(config.Env, envs...)

	config.WorkingDir = spec.WorkingDir

	config.Entrypoint = spec.EntryPoint

	config.Tty = spec.Tty

	config.OpenStdin = spec.StdinOpen

	config.Domainname = spec.DomainName

	config.StopSignal = spec.StopSignal

	if !versions.LessThan(hostInfo.DockerData.Version.APIVersion, "1.25") {
		timeout := int(spec.StopTimeout)
		if timeout != 0 {
			config.StopTimeout = &timeout
		}
	}

	config.User = spec.User
}

func isRunning(dockerClient *client.Client, containerID string) (bool, bool, error) {
	if containerID == "" {
		return false, false, nil
	}
	inspect, err := dockerClient.ContainerInspect(context.Background(), containerID)
	if err == nil {
		return inspect.State.Running && !inspect.State.Restarting, inspect.State.Restarting, nil
	} else if client.IsErrContainerNotFound(err) {
		return false, false, nil
	}
	return false, false, err
}

func getHostEntries() map[string]string {
	data := map[string]string{}
	for _, env := range HTTPProxyList {
		if os.Getenv(env) != "" {
			data[env] = os.Getenv(env)
		}
	}
	return data
}
