package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const dockerSocket = "/var/run/docker.sock"

// dockerClient is a thin wrapper around the Docker Engine API over Unix socket.
type dockerClient struct {
	http *http.Client
}

func newDockerClient() *dockerClient {
	return &dockerClient{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.DialTimeout("unix", dockerSocket, 5*time.Second)
				},
			},
			Timeout: 30 * time.Second,
		},
	}
}

func (d *dockerClient) close() {
	d.http.CloseIdleConnections()
}

// doRequest performs an HTTP request to the Docker daemon.
func (d *dockerClient) doRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	u := "http://localhost" + path
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return d.http.Do(req)
}

// Docker API response types (only the fields we need).
type dockerContainer struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

type dockerContainerInspect struct {
	ID     string `json:"Id"`
	Image  string `json:"Image"` // image digest
	Name   string `json:"Name"`
	Config struct {
		Image        string            `json:"Image"`
		Env          []string          `json:"Env"`
		Cmd          []string          `json:"Cmd"`
		Entrypoint   []string          `json:"Entrypoint"`
		WorkingDir   string            `json:"WorkingDir"`
		Labels       map[string]string `json:"Labels"`
		ExposedPorts map[string]any    `json:"ExposedPorts"`
		Volumes      map[string]any    `json:"Volumes"`
		User         string            `json:"User"`
	} `json:"Config"`
	HostConfig      json.RawMessage `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]struct {
			Aliases []string `json:"Aliases"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

type dockerInfo struct {
	ServerVersion string `json:"ServerVersion"`
	Name          string `json:"Name"`
}

type dockerImageInspect struct {
	ID string `json:"Id"`
}

type dockerCreateResponse struct {
	ID string `json:"Id"`
}

// info returns Docker daemon info.
func (d *dockerClient) info(ctx context.Context) (dockerInfo, error) {
	resp, err := d.doRequest(ctx, "GET", "/v1.45/info", nil)
	if err != nil {
		return dockerInfo{}, err
	}
	defer resp.Body.Close()

	var info dockerInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return dockerInfo{}, err
	}
	return info, nil
}

// listContainers lists running containers.
func (d *dockerClient) listContainers(ctx context.Context) ([]dockerContainer, error) {
	resp, err := d.doRequest(ctx, "GET", "/v1.45/containers/json", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var containers []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, err
	}
	return containers, nil
}

// inspectContainer inspects a container by ID.
func (d *dockerClient) inspectContainer(ctx context.Context, id string) (dockerContainerInspect, error) {
	resp, err := d.doRequest(ctx, "GET", "/v1.45/containers/"+id+"/json", nil)
	if err != nil {
		return dockerContainerInspect{}, err
	}
	defer resp.Body.Close()

	var inspect dockerContainerInspect
	if err := json.NewDecoder(resp.Body).Decode(&inspect); err != nil {
		return dockerContainerInspect{}, err
	}
	return inspect, nil
}

// pullImage pulls an image and returns when complete.
func (d *dockerClient) pullImage(ctx context.Context, imageName string) error {
	path := "/v1.45/images/create?fromImage=" + url.QueryEscape(imageName)
	resp, err := d.doRequest(ctx, "POST", path, nil)
	if err != nil {
		return fmt.Errorf("pulling image %s: %w", imageName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pull %s failed (%d): %s", imageName, resp.StatusCode, body)
	}

	// Consume the streaming response to wait for pull completion.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// inspectImage inspects an image by name or ID.
func (d *dockerClient) inspectImage(ctx context.Context, imageName string) (dockerImageInspect, error) {
	resp, err := d.doRequest(ctx, "GET", "/v1.45/images/"+url.PathEscape(imageName)+"/json", nil)
	if err != nil {
		return dockerImageInspect{}, err
	}
	defer resp.Body.Close()

	var inspect dockerImageInspect
	if err := json.NewDecoder(resp.Body).Decode(&inspect); err != nil {
		return dockerImageInspect{}, err
	}
	return inspect, nil
}

// stopContainer stops a container with a timeout.
func (d *dockerClient) stopContainer(ctx context.Context, id string, timeout int) error {
	path := fmt.Sprintf("/v1.45/containers/%s/stop?t=%d", id, timeout)
	resp, err := d.doRequest(ctx, "POST", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("stop failed (%d): %s", resp.StatusCode, body)
	}
	return nil
}

// removeContainer removes a container.
func (d *dockerClient) removeContainer(ctx context.Context, id string) error {
	resp, err := d.doRequest(ctx, "DELETE", "/v1.45/containers/"+id, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("remove failed (%d): %s", resp.StatusCode, body)
	}
	return nil
}

// createContainer creates a new container. The body is the full create payload.
func (d *dockerClient) createContainer(ctx context.Context, name string, payload []byte) (string, error) {
	path := "/v1.45/containers/create?name=" + url.QueryEscape(name)
	resp, err := d.doRequest(ctx, "POST", path, strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create failed (%d): %s", resp.StatusCode, body)
	}

	var created dockerCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// startContainer starts a container.
func (d *dockerClient) startContainer(ctx context.Context, id string) error {
	resp, err := d.doRequest(ctx, "POST", "/v1.45/containers/"+id+"/start", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("start failed (%d): %s", resp.StatusCode, body)
	}
	return nil
}

// listMonitoredContainers returns containers that have the opt-in label set to "true".
func listMonitoredContainers(ctx context.Context, cli *dockerClient, label string) ([]ContainerInfo, error) {
	containers, err := cli.listContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	var monitored []ContainerInfo
	for _, c := range containers {
		if c.Labels[label] != "true" {
			continue
		}

		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}

		mode := UpdateModeImage
		if m := c.Labels["docker-updater.mode"]; m == "git" {
			mode = UpdateModeGit
		}

		info := ContainerInfo{
			ID:     c.ID,
			Name:   name,
			Image:  c.Image,
			Mode:   mode,
			Labels: c.Labels,
		}

		if mode == UpdateModeGit {
			info.GitRepo = c.Labels["docker-updater.git-repo"]
			info.GitRef = c.Labels["docker-updater.git-ref"]
			if info.GitRef == "" {
				info.GitRef = "refs/heads/main"
			}
		}

		// Get current image digest from inspection.
		inspect, err := cli.inspectContainer(ctx, c.ID)
		if err == nil {
			info.ImageDigest = inspect.Image
		}

		monitored = append(monitored, info)
	}

	return monitored, nil
}

// recreateContainer stops the old container, creates a new one with the same
// config but an updated image, and starts it.
func recreateContainer(ctx context.Context, cli *dockerClient, info ContainerInfo, newImage string) error {
	inspect, err := cli.inspectContainer(ctx, info.ID)
	if err != nil {
		return fmt.Errorf("inspecting container %s: %w", info.Name, err)
	}

	log.Printf("stopping container %s (%s)", info.Name, shortID(info.ID))
	if err := cli.stopContainer(ctx, info.ID, 30); err != nil {
		return fmt.Errorf("stopping container %s: %w", info.Name, err)
	}
	if err := cli.removeContainer(ctx, info.ID); err != nil {
		return fmt.Errorf("removing container %s: %w", info.Name, err)
	}

	// Build the create payload preserving original config.
	inspect.Config.Image = newImage

	// Build networking config from current state.
	endpointsConfig := map[string]any{}
	for netName, netSettings := range inspect.NetworkSettings.Networks {
		endpointsConfig[netName] = map[string]any{
			"Aliases": netSettings.Aliases,
		}
	}

	createPayload := map[string]any{
		"Image":        inspect.Config.Image,
		"Env":          inspect.Config.Env,
		"Cmd":          inspect.Config.Cmd,
		"Entrypoint":   inspect.Config.Entrypoint,
		"WorkingDir":   inspect.Config.WorkingDir,
		"Labels":       inspect.Config.Labels,
		"ExposedPorts": inspect.Config.ExposedPorts,
		"Volumes":      inspect.Config.Volumes,
		"User":         inspect.Config.User,
		"HostConfig":   json.RawMessage(inspect.HostConfig),
		"NetworkingConfig": map[string]any{
			"EndpointsConfig": endpointsConfig,
		},
	}

	payload, err := json.Marshal(createPayload)
	if err != nil {
		return fmt.Errorf("marshaling create payload for %s: %w", info.Name, err)
	}

	log.Printf("creating new container %s with image %s", info.Name, newImage)
	newID, err := cli.createContainer(ctx, info.Name, payload)
	if err != nil {
		return fmt.Errorf("creating container %s: %w", info.Name, err)
	}

	if err := cli.startContainer(ctx, newID); err != nil {
		return fmt.Errorf("starting container %s: %w", info.Name, err)
	}

	log.Printf("container %s updated and started (%s)", info.Name, shortID(newID))
	return nil
}
