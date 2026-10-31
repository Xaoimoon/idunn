package engine

import (
	"encoding/json"
	"slices"
	"strings"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

// spec is what a container is created from.
type spec struct {
	config  *container.Config
	host    *container.HostConfig
	network *network.NetworkingConfig            // the primary network, at creation
	extra   map[string]*network.EndpointSettings // other networks, connected before start
}

// specOptions adjust the copy of a container's configuration.
type specOptions struct {
	// imageConfig is the configuration of the image the old container runs:
	// what the container inherited from it is left out, so that the new
	// image's own defaults apply. nil keeps the configuration as is.
	imageConfig *dockerspec.DockerOCIImageConfig
	// guessed: imageConfig is the new image's, the old one being gone. A
	// label is then dropped when the image sets the same key.
	guessed    bool
	newImageID string
	// replaced maps the IDs of containers recreated earlier in the pass to
	// their replacements, for the namespaces and volumes this one shares.
	replaced map[string]string
	keep     []string          // label keys kept even when inherited from the image
	labels   map[string]string // labels added
}

// newSpec copies what was set on the container, as Watchtower does: the
// values that came from the old image are dropped, the rest is kept.
func newSpec(c *Container, o specOptions) (spec, error) {
	var s spec
	if err := deepCopy(c.Info.Config, &s.config); err != nil {
		return s, err
	}
	if err := deepCopy(c.Info.HostConfig, &s.host); err != nil {
		return s, err
	}
	if s.config == nil {
		s.config = &container.Config{}
	}
	if s.host == nil {
		s.host = &container.HostConfig{}
	}
	cfg, hc := s.config, s.host

	if img := o.imageConfig; img != nil {
		stripImageDefaults(cfg, img, o.keep, o.guessed)
	}
	for p := range hc.PortBindings {
		if cfg.ExposedPorts == nil {
			cfg.ExposedPorts = network.PortSet{}
		}
		cfg.ExposedPorts[p] = struct{}{}
	}

	// The default hostname is the short ID of the old container.
	if cfg.Hostname == c.ShortID() || hc.NetworkMode.IsContainer() || hc.UTSMode != "" {
		cfg.Hostname = ""
	}
	if hc.NetworkMode.IsContainer() {
		cfg.Domainname = ""
		cfg.ExposedPorts = nil
	}

	delete(cfg.Labels, LabelReplaces)
	if _, ok := cfg.Labels[labelComposeImage]; ok && o.newImageID != "" {
		cfg.Labels[labelComposeImage] = o.newImageID
	}
	for k, v := range o.labels {
		if cfg.Labels == nil {
			cfg.Labels = map[string]string{}
		}
		cfg.Labels[k] = v
	}

	rewriteShared(hc, o.replaced)
	reuseAnonymousVolumes(c, cfg, hc)
	s.network, s.extra = endpoints(c, hc)
	return s, nil
}

func deepCopy[T any](src *T, dst **T) error {
	if src == nil {
		*dst = nil
		return nil
	}
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// stripImageDefaults removes from cfg the values inherited from img. With
// byKey, labels go when img sets the same key, whatever the value.
func stripImageDefaults(cfg *container.Config, img *dockerspec.DockerOCIImageConfig, keep []string, byKey bool) {
	if cfg.WorkingDir == img.WorkingDir {
		cfg.WorkingDir = ""
	}
	if cfg.User == img.User {
		cfg.User = ""
	}
	if cfg.StopSignal == img.StopSignal {
		cfg.StopSignal = ""
	}
	// An entrypoint given at creation resets the image command.
	if slices.Equal(cfg.Entrypoint, img.Entrypoint) {
		cfg.Entrypoint = nil
		if slices.Equal(cfg.Cmd, img.Cmd) {
			cfg.Cmd = nil
		}
	}
	if slices.Equal(cfg.Shell, img.Shell) {
		cfg.Shell = nil
	}
	if slices.Equal(cfg.OnBuild, img.OnBuild) {
		cfg.OnBuild = nil
	}
	if hc, ihc := cfg.Healthcheck, img.Healthcheck; hc != nil && ihc != nil {
		if slices.Equal(hc.Test, ihc.Test) {
			hc.Test = nil
		}
		if hc.Interval == ihc.Interval {
			hc.Interval = 0
		}
		if hc.Timeout == ihc.Timeout {
			hc.Timeout = 0
		}
		if hc.StartPeriod == ihc.StartPeriod {
			hc.StartPeriod = 0
		}
		if hc.StartInterval == ihc.StartInterval {
			hc.StartInterval = 0
		}
		if hc.Retries == ihc.Retries {
			hc.Retries = 0
		}
		if hc.Test == nil && hc.Interval == 0 && hc.Timeout == 0 && hc.StartPeriod == 0 &&
			hc.StartInterval == 0 && hc.Retries == 0 {
			cfg.Healthcheck = nil
		}
	}

	imageEnv := map[string]bool{}
	for _, e := range img.Env {
		imageEnv[e] = true
	}
	env := cfg.Env[:0]
	for _, e := range cfg.Env {
		if !imageEnv[e] {
			env = append(env, e)
		}
	}
	cfg.Env = env

	for k, v := range img.Labels {
		if cur, ok := cfg.Labels[k]; ok && (cur == v || byKey) && !keepLabel(k, keep) {
			delete(cfg.Labels, k)
		}
	}
	for v := range img.Volumes {
		delete(cfg.Volumes, v)
	}
	for p := range cfg.ExposedPorts {
		if _, ok := img.ExposedPorts[p.String()]; ok {
			delete(cfg.ExposedPorts, p)
		}
	}
}

// keepLabel reports whether a label stays even when the image sets the same
// value: the selection labels, so that the container stays selected.
func keepLabel(key string, keep []string) bool {
	return slices.Contains(keep, key)
}

// rewriteShared points the namespaces and volumes shared with a container
// recreated earlier in the pass at its replacement.
func rewriteShared(hc *container.HostConfig, replaced map[string]string) {
	if len(replaced) == 0 {
		return
	}
	swap := func(ref string) string {
		for oldID, newID := range replaced {
			if ref == oldID || (len(ref) >= 12 && strings.HasPrefix(oldID, ref)) {
				return newID
			}
		}
		return ref
	}
	if hc.NetworkMode.IsContainer() {
		hc.NetworkMode = container.NetworkMode("container:" + swap(hc.NetworkMode.ConnectedContainer()))
	}
	if hc.IpcMode.IsContainer() {
		hc.IpcMode = container.IpcMode("container:" + swap(hc.IpcMode.Container()))
	}
	if hc.PidMode.IsContainer() {
		hc.PidMode = container.PidMode("container:" + swap(hc.PidMode.Container()))
	}
	for i, v := range hc.VolumesFrom {
		ref, mode, hasMode := strings.Cut(v, ":")
		ref = swap(ref)
		if hasMode {
			ref += ":" + mode
		}
		hc.VolumesFrom[i] = ref
	}
}

// reuseAnonymousVolumes mounts the old container's anonymous volumes again,
// at the same place: without it, the new container would get new, empty
// ones. Named volumes and bind mounts are part of the configuration already.
func reuseAnonymousVolumes(c *Container, cfg *container.Config, hc *container.HostConfig) {
	for _, mp := range c.Info.Mounts {
		if mp.Type != mount.TypeVolume || !anonymous(mp.Name) {
			continue
		}
		// "-v /path" or "--mount type=volume,target=/path" would create a new
		// volume, and a mount added by an earlier update names it already:
		// they give way to a mount of the existing one.
		delete(cfg.Volumes, mp.Destination)
		hc.Binds = slices.DeleteFunc(hc.Binds, func(b string) bool { return b == mp.Destination })
		hc.Mounts = slices.DeleteFunc(hc.Mounts, func(m mount.Mount) bool {
			return m.Type == mount.TypeVolume && m.Target == mp.Destination
		})
		hc.Mounts = append(hc.Mounts, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   mp.Name,
			Target:   mp.Destination,
			ReadOnly: !mp.RW,
		})
	}
}

// anonymous reports whether a volume name is one Docker generated.
func anonymous(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, r := range name {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// endpoints returns the network the container is created on and the other
// networks it joins before it starts. Addresses and MAC addresses the engine
// assigned are not copied: only what was configured is.
func endpoints(c *Container, hc *container.HostConfig) (*network.NetworkingConfig, map[string]*network.EndpointSettings) {
	nm := hc.NetworkMode
	if nm.IsContainer() || nm.IsHost() || nm.IsNone() || c.Info.NetworkSettings == nil {
		return nil, nil
	}
	primary := nm.NetworkName()
	if nm.IsDefault() || primary == "" {
		primary = "bridge"
	}
	var first *network.NetworkingConfig
	extra := map[string]*network.EndpointSettings{}
	for name, ep := range c.Info.NetworkSettings.Networks {
		if ep == nil {
			continue
		}
		cfg := &network.EndpointSettings{
			Links:      slices.Clone(ep.Links),
			DriverOpts: ep.DriverOpts,
			GwPriority: ep.GwPriority,
		}
		if ep.IPAMConfig != nil {
			ipam := *ep.IPAMConfig
			cfg.IPAMConfig = &ipam
		}
		for _, a := range ep.Aliases {
			if a != c.ShortID() && a != c.ID {
				cfg.Aliases = append(cfg.Aliases, a)
			}
		}
		if name == primary {
			first = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{name: cfg}}
		} else {
			extra[name] = cfg
		}
	}
	return first, extra
}
