package component

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/go-connections/nat"

	"github.com/pkg/errors"
	"k8s.io/klog"

	"github.com/openshift/odo/pkg/devfile/adapters/common"
	"github.com/openshift/odo/pkg/devfile/adapters/docker/storage"
	"github.com/openshift/odo/pkg/devfile/adapters/docker/utils"
	versionsCommon "github.com/openshift/odo/pkg/devfile/parser/data/common"
	"github.com/openshift/odo/pkg/envinfo"
	"github.com/openshift/odo/pkg/exec"
	"github.com/openshift/odo/pkg/lclient"
	"github.com/openshift/odo/pkg/log"
)

const (
	LocalhostIP             = "127.0.0.1"
	projectSourceVolumeName = "odo-project-source"
)

func (a Adapter) createComponent() (err error) {
	componentName := a.ComponentName

	log.Infof("\nCreating Docker resources for component %s", a.ComponentName)

	// Get or create the project source volume
	var projectVolumeName string
	projectVolumeLabels := utils.GetProjectVolumeLabels(componentName)
	projectVols, err := a.Client.GetVolumesByLabel(projectVolumeLabels)
	if err != nil {
		return errors.Wrapf(err, "Unable to retrieve source volume for component "+componentName)
	}
	if len(projectVols) == 0 {
		// A source volume needs to be created
		projectVolumeName, err = storage.GenerateVolName(projectSourceVolumeName, a.ComponentName)
		if err != nil {
			return errors.Wrapf(err, "Unable to generate project source volume name for component %s", componentName)
		}
		_, err := a.Client.CreateVolume(projectVolumeName, projectVolumeLabels)
		if err != nil {
			return errors.Wrapf(err, "Unable to create project source volume for component %s", componentName)
		}
	} else if len(projectVols) == 1 {
		projectVolumeName = projectVols[0].Name
	} else if len(projectVols) > 1 {
		return errors.Wrapf(err, "Error, multiple source volumes found for component %s", componentName)
	}

	supportedComponents := common.GetSupportedComponents(a.Devfile.Data)
	if len(supportedComponents) == 0 {
		return fmt.Errorf("No valid components found in the devfile")
	}

	// Get the storage adapter and create the volumes if it does not exist
	stoAdapter := storage.New(a.AdapterContext, a.Client)
	err = stoAdapter.Create(a.uniqueStorage)
	if err != nil {
		return errors.Wrapf(err, "Unable to create Docker storage adapter for component %s", componentName)
	}

	// Loop over each component and start a container for it
	for _, comp := range supportedComponents {
		var dockerVolumeMounts []mount.Mount
		for _, vol := range a.componentAliasToVolumes[comp.Container.Name] {

			volMount := mount.Mount{
				Type:   mount.TypeVolume,
				Source: a.volumeNameToDockerVolName[vol.Name],
				Target: vol.ContainerPath,
			}
			dockerVolumeMounts = append(dockerVolumeMounts, volMount)
		}
		err = a.pullAndStartContainer(dockerVolumeMounts, projectVolumeName, comp)
		if err != nil {
			return errors.Wrapf(err, "unable to pull and start container %s for component %s", comp.Container.Name, componentName)
		}
	}
	klog.V(3).Infof("Successfully created all containers for component %s", componentName)

	return nil
}

func (a Adapter) updateComponent() (componentExists bool, err error) {
	klog.V(3).Info("The component already exists, attempting to update it")
	componentExists = true
	componentName := a.ComponentName

	// Get the project source volume
	volumeLabels := utils.GetProjectVolumeLabels(componentName)
	projectVols, err := a.Client.GetVolumesByLabel(volumeLabels)
	if err != nil {
		return componentExists, errors.Wrapf(err, "Unable to retrieve source volume for component "+componentName)
	}
	if len(projectVols) == 0 {
		return componentExists, fmt.Errorf("Unable to find source volume for component %s", componentName)
	} else if len(projectVols) > 1 {
		return componentExists, errors.Wrapf(err, "Error, multiple source volumes found for component %s", componentName)
	}
	projectVolumeName := projectVols[0].Name

	// Get the storage adapter and create the volumes if it does not exist
	stoAdapter := storage.New(a.AdapterContext, a.Client)
	err = stoAdapter.Create(a.uniqueStorage)

	supportedComponents := common.GetSupportedComponents(a.Devfile.Data)
	if len(supportedComponents) == 0 {
		return componentExists, fmt.Errorf("No valid components found in the devfile")
	}

	for _, comp := range supportedComponents {
		// Check to see if this component is already running and if so, update it
		// If component isn't running, re-create it, as it either may be new, or crashed.
		containers, err := a.Client.GetContainersByComponentAndAlias(componentName, comp.Container.Name)
		if err != nil {
			return false, errors.Wrapf(err, "unable to list containers for component %s", componentName)
		}

		var dockerVolumeMounts []mount.Mount
		for _, vol := range a.componentAliasToVolumes[comp.Container.Name] {
			volMount := mount.Mount{
				Type:   mount.TypeVolume,
				Source: a.volumeNameToDockerVolName[vol.Name],
				Target: vol.ContainerPath,
			}
			dockerVolumeMounts = append(dockerVolumeMounts, volMount)
		}
		if len(containers) == 0 {
			log.Infof("\nCreating Docker resources for component %s", a.ComponentName)

			// Container doesn't exist, so need to pull its image (to be safe) and start a new container
			err = a.pullAndStartContainer(dockerVolumeMounts, projectVolumeName, comp)
			if err != nil {
				return false, errors.Wrapf(err, "unable to pull and start container %s for component %s", comp.Container.Name, componentName)
			}

			// Update componentExists so that we re-sync project and initialize supervisord if required
			componentExists = false
		} else if len(containers) == 1 {
			// Container already exists
			containerID := containers[0].ID

			// Get the associated container config, host config and mounts from the container
			containerConfig, hostConfig, mounts, err := a.Client.GetContainerConfigHostConfigAndMounts(containerID)
			if err != nil {
				return componentExists, errors.Wrapf(err, "unable to get the container config for component %s", componentName)
			}

			portMap, namePortMapping, err := getPortMap(a.Context, comp.Container.Endpoints, false)
			if err != nil {
				return componentExists, errors.Wrapf(err, "unable to get the port map from env.yaml file for component %s", componentName)
			}
			for port, urlName := range namePortMapping {
				containerConfig.Labels[port.Port()] = urlName
			}

			// See if the container needs to be updated
			if utils.DoesContainerNeedUpdating(comp, containerConfig, hostConfig, dockerVolumeMounts, mounts, portMap) {
				log.Infof("\nCreating Docker resources for component %s", a.ComponentName)

				s := log.SpinnerNoSpin("Updating the component " + comp.Container.Name)
				defer s.End(false)

				// Remove the container
				err := a.Client.RemoveContainer(containerID)
				if err != nil {
					return componentExists, errors.Wrapf(err, "Unable to remove container %s for component %s", containerID, comp.Container.Name)
				}

				// Start the container
				err = a.startComponent(dockerVolumeMounts, projectVolumeName, comp)
				if err != nil {
					return false, errors.Wrapf(err, "Unable to start container for devfile component %s", comp.Container.Name)
				}

				klog.V(3).Infof("Successfully created container %s for component %s", comp.Container.Image, componentName)
				s.End(true)

				// Update componentExists so that we re-sync project and initialize supervisord if required
				componentExists = false
			}
		} else {
			// Multiple containers were returned with the specified label (which should be unique)
			// Error out, as this isn't expected
			return true, fmt.Errorf("Found multiple running containers for devfile component %s and cannot push changes", comp.Container.Name)
		}
	}

	return
}

func (a Adapter) pullAndStartContainer(mounts []mount.Mount, projectVolumeName string, comp versionsCommon.DevfileComponent) error {
	// Container doesn't exist, so need to pull its image (to be safe) and start a new container
	s := log.Spinnerf("Pulling image %s", comp.Container.Image)

	err := a.Client.PullImage(comp.Container.Image)
	if err != nil {
		s.End(false)
		return errors.Wrapf(err, "Unable to pull %s image", comp.Container.Image)
	}
	s.End(true)

	// Start the component container
	err = a.startComponent(mounts, projectVolumeName, comp)
	if err != nil {
		return errors.Wrapf(err, "Unable to start container for devfile component %s", comp.Container.Name)
	}

	klog.V(3).Infof("Successfully created container %s for component %s", comp.Container.Image, a.ComponentName)
	return nil
}

func (a Adapter) startComponent(mounts []mount.Mount, projectVolumeName string, comp versionsCommon.DevfileComponent) error {
	hostConfig, namePortMapping, err := a.generateAndGetHostConfig(comp.Container.Endpoints)
	hostConfig.Mounts = mounts
	if err != nil {
		return err
	}

	// Get the run command and update it's component entrypoint with supervisord if required and component's env command & workdir
	runCommand, err := common.GetRunCommand(a.Devfile.Data, a.devfileRunCmd)
	if err != nil {
		return err
	}
	utils.UpdateComponentWithSupervisord(&comp, runCommand, a.supervisordVolumeName, &hostConfig)

	// If the component set `mountSources` to true, add the source volume and env CHE_PROJECTS_ROOT to it
	if comp.Container.MountSources {
		utils.AddVolumeToContainer(projectVolumeName, lclient.OdoSourceVolumeMount, &hostConfig)

		if !common.IsEnvPresent(comp.Container.Env, common.EnvCheProjectsRoot) {
			envName := common.EnvCheProjectsRoot
			envValue := lclient.OdoSourceVolumeMount
			comp.Container.Env = append(comp.Container.Env, versionsCommon.Env{
				Name:  envName,
				Value: envValue,
			})
		}
	}

	// Generate the container config after updating the component with the necessary data
	containerConfig := a.generateAndGetContainerConfig(a.ComponentName, comp)
	for port, urlName := range namePortMapping {
		containerConfig.Labels[port.Port()] = urlName
	}

	// Create the docker container
	s := log.Spinner("Starting container for " + comp.Container.Image)
	defer s.End(false)
	err = a.Client.StartContainer(&containerConfig, &hostConfig, nil)
	if err != nil {
		return err
	}
	s.End(true)

	return nil
}

func (a Adapter) generateAndGetContainerConfig(componentName string, comp versionsCommon.DevfileComponent) container.Config {
	// Convert the env vars in the Devfile to the format expected by Docker
	envVars := utils.ConvertEnvs(comp.Container.Env)
	ports := utils.ConvertPorts(comp.Container.Endpoints)
	containerLabels := utils.GetContainerLabels(componentName, comp.Container.Name)
	containerConfig := a.Client.GenerateContainerConfig(comp.Container.Image, comp.Container.Command, comp.Container.Args, envVars, containerLabels, ports)

	return containerConfig
}

func (a Adapter) generateAndGetHostConfig(endpoints []versionsCommon.Endpoint) (container.HostConfig, map[nat.Port]string, error) {
	// Convert the port bindings from env.yaml and generate docker host config
	portMap, namePortMapping, err := getPortMap(a.Context, endpoints, true)
	if err != nil {
		return container.HostConfig{}, map[nat.Port]string{}, err
	}

	hostConfig := container.HostConfig{}
	if len(portMap) > 0 {
		hostConfig = a.Client.GenerateHostConfig(false, false, portMap)
	}

	return hostConfig, namePortMapping, nil
}

func getPortMap(context string, endpoints []versionsCommon.Endpoint, show bool) (nat.PortMap, map[nat.Port]string, error) {
	// Convert the exposed and internal port pairs saved in env.yaml file to PortMap
	// Todo: Use context to get the approraite envinfo after context is supported in experimental mode
	portmap := nat.PortMap{}
	namePortMapping := make(map[nat.Port]string)

	var dir string
	var err error
	if context == "" {
		dir, err = os.Getwd()
		if err != nil {
			return nil, nil, err
		}
	} else {
		dir = context
	}
	if err != nil {
		return nil, nil, err
	}

	envInfo, err := envinfo.NewEnvSpecificInfo(dir)
	if err != nil {
		return nil, nil, err
	}

	urlArr := envInfo.GetURL()

	for _, url := range urlArr {
		if url.ExposedPort > 0 && common.IsPortPresent(endpoints, url.Port) {
			port, err := nat.NewPort("tcp", strconv.Itoa(url.Port))
			if err != nil {
				return nil, nil, err
			}
			portmap[port] = []nat.PortBinding{
				nat.PortBinding{
					HostIP:   LocalhostIP,
					HostPort: strconv.Itoa(url.ExposedPort),
				},
			}
			namePortMapping[port] = url.Name
			if show {
				log.Successf("URL %v:%v created", LocalhostIP, url.ExposedPort)
			}
		} else if url.ExposedPort > 0 && len(endpoints) > 0 && !common.IsPortPresent(endpoints, url.Port) {
			return nil, nil, fmt.Errorf("Error creating url: odo url config's port is not present in the devfile. Please re-create odo url with the new devfile port")
		}
	}

	return portmap, namePortMapping, nil
}

// Executes all the commands from the devfile in order: init and build - which are both optional, and a compulsary run.
// Init only runs once when the component is created.
func (a Adapter) execDevfile(commandsMap common.PushCommandsMap, componentExists, show bool, containers []types.Container) (err error) {
	// If nothing has been passed, then the devfile is missing the required run command
	if len(commandsMap) == 0 {
		return errors.New(fmt.Sprint("error executing devfile commands - there should be at least 1 command"))
	}

	// Only add runinit to the expected commands if the component doesn't already exist
	// This would be the case when first running the container
	if !componentExists {
		// Get Init Command
		command, ok := commandsMap[versionsCommon.InitCommandGroupType]
		if ok {

			containerID := utils.GetContainerIDForAlias(containers, command.Exec.Component)
			compInfo := common.ComponentInfo{ContainerName: containerID}
			err = exec.ExecuteDevfileBuildAction(&a.Client, *command.Exec, command.Exec.Id, compInfo, show)
			if err != nil {
				return err
			}
		}
	}

	// Get Build Command
	command, ok := commandsMap[versionsCommon.BuildCommandGroupType]
	if ok {
		containerID := utils.GetContainerIDForAlias(containers, command.Exec.Component)
		compInfo := common.ComponentInfo{ContainerName: containerID}
		err = exec.ExecuteDevfileBuildAction(&a.Client, *command.Exec, command.Exec.Id, compInfo, show)
		if err != nil {
			return err
		}
	}

	// Get Run command
	command, ok = commandsMap[versionsCommon.RunCommandGroupType]
	if ok {
		klog.V(4).Infof("Executing devfile command %v", command.Exec.Id)

		// Check if the devfile run component containers have supervisord as the entrypoint.
		// Start the supervisord if the odo component does not exist
		if !componentExists {
			err = a.InitRunContainerSupervisord(command.Exec.Component, containers)
			if err != nil {
				return
			}
		}

		containerID := utils.GetContainerIDForAlias(containers, command.Exec.Component)
		compInfo := common.ComponentInfo{ContainerName: containerID}
		if componentExists && !common.IsRestartRequired(command) {
			klog.V(4).Info("restart:false, Not restarting DevRun Command")
			err = exec.ExecuteDevfileRunActionWithoutRestart(&a.Client, *command.Exec, command.Exec.Id, compInfo, show)
			return
		}
		err = exec.ExecuteDevfileRunAction(&a.Client, *command.Exec, command.Exec.Id, compInfo, show)
	}

	return
}

// InitRunContainerSupervisord initializes the supervisord in the container if
// the container has entrypoint that is not supervisord
func (a Adapter) InitRunContainerSupervisord(component string, containers []types.Container) (err error) {
	for _, container := range containers {
		if container.Labels["alias"] == component && !strings.Contains(container.Command, common.SupervisordBinaryPath) {
			command := []string{common.SupervisordBinaryPath, "-c", common.SupervisordConfFile, "-d"}
			compInfo := common.ComponentInfo{
				ContainerName: container.ID,
			}
			err = exec.ExecuteCommand(&a.Client, compInfo, command, true)
		}
	}

	return
}
