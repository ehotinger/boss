package system

import (
	"context"
	"os"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/defaults"
	"github.com/containerd/containerd/namespaces"
	gocni "github.com/containerd/go-cni"
	"github.com/crosbymichael/boss/cni"
	"github.com/crosbymichael/boss/config"
	"github.com/crosbymichael/boss/util"
	"github.com/hashicorp/consul/api"
	"github.com/pkg/errors"
)

// Load the system config from disk
// fix up any missing fields with runtime data
func Load() (*config.Config, error) {
	var c config.Config
	if _, err := toml.DecodeFile(config.Path, &c); err != nil {
		return nil, err
	}
	if c.ID == "" {
		id, err := os.Hostname()
		if err != nil {
			return nil, err
		}
		c.ID = id
	}
	if c.Iface == "" {
		c.Iface = "eth0"
	}
	return &c, nil
}

// Context returns a new context to be used by boss
func Context() context.Context {
	return namespaces.WithNamespace(context.Background(), config.DefaultNamespace)
}

// NewClient returns a new containerd client
func NewClient() (*containerd.Client, error) {
	return containerd.New(
		defaults.DefaultAddress,
		containerd.WithDefaultRuntime(config.DefaultRuntime),
	)
}

var (
	consul     *api.Client
	consulErr  error
	consulOnce sync.Once
)

func getConsul() {
	consul, consulErr = api.NewClient(api.DefaultConfig())
}

func GetConfigStore(c *config.Config) (config.ConfigStore, error) {
	if c.Consul != nil {
		consulOnce.Do(getConsul)
		if consulErr != nil {
			return nil, consulErr
		}
		return &configStore{
			consul: consul,
		}, nil
	}
	return &nullStore{}, nil
}

// GetNetwork returns a network for the givin name
func GetNetwork(c *config.Config, name string) (config.Network, error) {
	ip, err := util.GetIP(c.Iface)
	if err != nil {
		return nil, err
	}
	switch name {
	case "", "none":
		return &none{}, nil
	case "host":
		return &host{ip: ip}, nil
	case "cni":
		if c.CNI == nil {
			return nil, errors.New("[cni] is not enabled in the system config")
		}
		// populate cni data from main config if fields are missing
		c.CNI.Version = "0.3.1"
		if c.CNI.Name == "" {
			c.CNI.Name = c.Domain
		}
		if c.CNI.Master == "" {
			c.CNI.Master = c.Iface
		}
		n, err := gocni.New(gocni.WithPluginDir([]string{"/opt/containerd/bin"}), gocni.WithConf(c.CNI.Bytes()), gocni.WithLoNetwork)
		if err != nil {
			return nil, err
		}
		return cni.New(c.CNI.Type, c.Iface, n)
	}
	return nil, errors.Errorf("network %s does not exist", name)
}

func GetRegister(c *config.Config) (config.Register, error) {
	if c.Consul != nil {
		consulOnce.Do(getConsul)
		if consulErr != nil {
			return nil, consulErr
		}
		return &Consul{
			client: consul,
		}, nil
	}
	return &nullRegister{}, nil
}

func GetNameservers(c *config.Config) ([]string, error) {
	if c.Consul != nil {
		consulOnce.Do(getConsul)
		if consulErr != nil {
			return nil, consulErr
		}
		nodes, _, err := consul.Catalog().Nodes(&api.QueryOptions{})
		if err != nil {
			return nil, err
		}
		var ns []string
		for _, n := range nodes {
			ns = append(ns, n.Address)
		}
		return ns, nil
	}
	if len(c.Nameservers) == 0 {
		return []string{
			"8.8.8.8",
			"8.8.4.4",
		}, nil
	}
	return c.Nameservers, nil
}
