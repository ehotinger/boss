package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/containerd/cgroups"
	"github.com/containerd/containerd"
	tasks "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/containerd/rootfs"
	"github.com/containerd/containerd/runtime/v2/runc/options"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/typeurl"
	"github.com/crosbymichael/boss/api"
	"github.com/crosbymichael/boss/api/v1"
	"github.com/crosbymichael/boss/config"
	"github.com/crosbymichael/boss/flux"
	"github.com/crosbymichael/boss/opts"
	"github.com/crosbymichael/boss/systemd"
	"github.com/ehazlett/element"
	"github.com/gogo/protobuf/types"
	"github.com/gomodule/redigo/redis"
	ver "github.com/opencontainers/image-spec/specs-go"
	is "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	lconfig "github.com/siddontang/ledisdb/config"
	"github.com/siddontang/ledisdb/server"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

var (
	ErrNoID  = errors.New("no id provided")
	ErrNoRef = errors.New("no ref provided")

	empty = &types.Empty{}
)

const (
	MediaTypeContainerInfo = "application/vnd.boss.container.info.v1+json"
	Master                 = "boss.io/master"
	StorePort              = "boss.io/store.port"
)

func New(c *config.Config, client *containerd.Client, store config.ConfigStore, node *element.Agent, storePort int) (*Agent, error) {
	register, err := c.GetRegister()
	if err != nil {
		return nil, err
	}
	server, err := newLocalStore(c, storePort)
	if err != nil {
		return nil, err
	}
	logrus.Debug("connecting to other nodes")
	var (
		local  = fmt.Sprintf("127.0.0.1:%d", storePort)
		master = local
	)
	if !c.Agent.Master {
		master = ""
		peers, err := node.Peers()
		if err != nil {
			return nil, err
		}
		for _, p := range peers {
			if _, ok := p.Labels[Master]; ok {
				host, _, err := net.SplitHostPort(p.Addr)
				if err != nil {
					return nil, err
				}
				// move port to lables
				master = net.JoinHostPort(host, p.Labels[StorePort])
				break
			}
		}
		if master == "" {
			return nil, errors.New("unable to find master in cluster")
		}
	}
	mp := newPool(master)
	var lp *redis.Pool
	if c.Agent.Master {
		lp = mp
	} else {
		lp = newPool(local)
		conn := lp.Get()
		defer conn.Close()
		host, port, err := net.SplitHostPort(master)
		if err != nil {
			return nil, err
		}
		if _, err := conn.Do("SLAVEOF", host, port); err != nil {
			return nil, err
		}
	}
	agent := &Agent{
		c:        c,
		client:   client,
		store:    store,
		register: register,
		node:     node,
		server:   server,
		master:   mp,
		local:    lp,
	}
	if err := agent.handleResolvConf(); err != nil {
		return nil, err
	}
	return agent, nil
}

func newPool(address string) *redis.Pool {
	return redis.NewPool(func() (redis.Conn, error) {
		return redis.Dial("tcp", address)
	}, 5)
}

func newLocalStore(c *config.Config, storePort int) (*server.App, error) {
	cfg := lconfig.NewConfigDefault()
	cfg.Addr = fmt.Sprintf("0.0.0.0:%d", storePort)
	logrus.WithField("store", cfg.Addr).Debug("store address")
	cfg.UseReplication = true
	cfg.DataDir = filepath.Join(v1.Root, c.ID)
	if !c.Agent.Master {
		cfg.Readonly = true
	}
	logrus.Debug("serving ledis store")
	server, err := server.NewApp(cfg)
	if err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		wg.Done()
		server.Run()
	}()
	wg.Wait()
	return server, nil
}

type Agent struct {
	c        *config.Config
	client   *containerd.Client
	store    config.ConfigStore
	register v1.Register
	node     *element.Agent
	server   *server.App
	master   *redis.Pool
	local    *redis.Pool
}

func (a *Agent) Close() error {
	a.server.Close()
	a.master.Close()
	a.local.Close()
	return nil
}

func (a *Agent) Create(ctx context.Context, req *v1.CreateRequest) (*types.Empty, error) {
	ctx = relayContext(ctx)
	image, err := a.client.Pull(ctx, req.Container.Image, containerd.WithPullUnpack, a.withPlainRemote(req.Container.Image))
	if err != nil {
		return nil, err
	}
	if _, err := a.client.LoadContainer(ctx, req.Container.ID); err == nil {
		if !req.Update {
			return nil, errors.Errorf("container %s already exists", req.Container.ID)
		}
		_, err = a.Update(ctx, &v1.UpdateRequest{
			Container: req.Container,
		})
		return empty, err
	}
	volumeRoot, err := redis.String(a.doLocal("GET", v1.VolumeRootKey))
	if err != nil && err != redis.ErrNil {
		return nil, err
	}
	container, err := a.client.NewContainer(ctx,
		req.Container.ID,
		flux.WithNewSnapshot(image),
		opts.WithBossConfig(volumeRoot, req.Container, image),
	)
	if err != nil {
		return nil, err
	}
	if err := a.store.Write(ctx, req.Container); err != nil {
		container.Delete(ctx, containerd.WithSnapshotCleanup)
		return nil, err
	}
	if err := systemd.Enable(ctx, container.ID()); err != nil {
		return nil, err
	}
	if err := systemd.Start(ctx, container.ID()); err != nil {
		return nil, err
	}
	return empty, nil
}

func (a *Agent) Delete(ctx context.Context, req *v1.DeleteRequest) (*types.Empty, error) {
	ctx = relayContext(ctx)
	id := req.ID
	if id == "" {
		return nil, ErrNoID
	}
	container, err := a.client.LoadContainer(ctx, id)
	if err != nil {
		return nil, errors.Wrap(err, "load container")
	}
	if err := systemd.Stop(ctx, id); err != nil {
		return nil, errors.Wrap(err, "stop service")
	}
	if err := systemd.Disable(ctx, id); err != nil {
		return nil, errors.Wrap(err, "disable service")
	}
	config, err := opts.GetConfig(ctx, container)
	if err != nil {
		return nil, errors.Wrap(err, "load config")
	}
	network, err := a.c.GetNetwork(config.Network)
	if err != nil {
		return nil, errors.Wrap(err, "get network")
	}
	if err := network.Remove(ctx, container); err != nil {
		return nil, err
	}
	for name := range config.Services {
		if err := a.register.Deregister(id, name); err != nil {
			logrus.WithError(err).Errorf("de-register %s-%s", id, name)
		}
	}
	return empty, container.Delete(ctx, flux.WithRevisionCleanup)
}

func (a *Agent) Get(ctx context.Context, req *v1.GetRequest) (*v1.GetResponse, error) {
	ctx = relayContext(ctx)
	id := req.ID
	if id == "" {
		return nil, ErrNoID
	}
	container, err := a.client.LoadContainer(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	i, err := a.info(ctx, container)
	if err != nil {
		return nil, err
	}
	return &v1.GetResponse{
		Container: i,
	}, nil
}

func (a *Agent) info(ctx context.Context, c containerd.Container) (*v1.ContainerInfo, error) {
	info, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	d := info.Extensions[opts.CurrentConfig]
	cfg, err := opts.UnmarshalConfig(&d)
	if err != nil {
		return nil, err
	}

	service := a.client.SnapshotService(info.Snapshotter)
	usage, err := service.Usage(ctx, info.SnapshotKey)
	if err != nil {
		return nil, err
	}
	var ss []*v1.Snapshot
	if err := service.Walk(ctx, func(ctx context.Context, si snapshots.Info) error {
		if si.Labels[flux.ContainerIDLabel] != c.ID() {
			return nil
		}
		usage, err := service.Usage(ctx, si.Name)
		if err != nil {
			return err
		}
		ss = append(ss, &v1.Snapshot{
			ID:       si.Name,
			Created:  si.Created,
			Previous: si.Labels[flux.PreviousLabel],
			FsSize:   usage.Size,
		})
		return nil
	}); err != nil {
		return nil, err
	}
	bindSizes, err := getBindSizes(cfg)
	if err != nil {
		return nil, err
	}
	task, err := c.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return &v1.ContainerInfo{
				ID:        c.ID(),
				Image:     info.Image,
				Status:    string(containerd.Stopped),
				FsSize:    usage.Size + bindSizes,
				Config:    cfg,
				Snapshots: ss,
			}, nil
		}
		return nil, err
	}
	status, err := task.Status(ctx)
	if err != nil {
		return nil, err
	}
	stats, err := task.Metrics(ctx)
	if err != nil {
		return nil, err
	}
	v, err := typeurl.UnmarshalAny(stats.Data)
	if err != nil {
		return nil, err
	}
	var (
		cg     = v.(*cgroups.Metrics)
		cpu    = cg.CPU.Usage.Total
		memory = float64(cg.Memory.Usage.Usage - cg.Memory.TotalCache)
		limit  = float64(cg.Memory.Usage.Limit)
	)
	return &v1.ContainerInfo{
		ID:          c.ID(),
		Image:       info.Image,
		Status:      string(status.Status),
		IP:          info.Labels[opts.IPLabel],
		Cpu:         cpu,
		MemoryUsage: memory,
		MemoryLimit: limit,
		PidUsage:    cg.Pids.Current,
		PidLimit:    cg.Pids.Limit,
		FsSize:      usage.Size + bindSizes,
		Config:      cfg,
		Snapshots:   ss,
	}, nil
}

func (a *Agent) List(ctx context.Context, req *v1.ListRequest) (*v1.ListResponse, error) {
	var resp v1.ListResponse
	ctx = relayContext(ctx)
	containers, err := a.client.Containers(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		l, err := a.info(ctx, c)
		if err != nil {
			resp.Containers = append(resp.Containers, &v1.ContainerInfo{
				ID:     c.ID(),
				Status: "list error",
			})
			logrus.WithError(err).Error("info container")
			continue
		}
		resp.Containers = append(resp.Containers, l)
	}
	return &resp, nil
}

func (a *Agent) Kill(ctx context.Context, req *v1.KillRequest) (*types.Empty, error) {
	ctx = relayContext(ctx)
	id := req.ID
	if id == "" {
		return nil, ErrNoID
	}
	container, err := a.client.LoadContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	config, err := opts.GetConfig(ctx, container)
	if err != nil {
		return nil, err
	}
	for name := range config.Services {
		if err := a.register.EnableMaintainance(id, name, "manual kill"); err != nil {
			logrus.WithError(err).Errorf("enable maintaince %s-%s", id, name)
		}
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := task.Kill(ctx, unix.SIGTERM); err != nil {
		return nil, err
	}
	return empty, nil
}

func (a *Agent) Start(ctx context.Context, req *v1.StartRequest) (*types.Empty, error) {
	ctx = relayContext(ctx)
	id := req.ID
	if id == "" {
		return nil, ErrNoID
	}
	return empty, systemd.Start(ctx, req.ID)
}

func (a *Agent) Stop(ctx context.Context, req *v1.StopRequest) (*types.Empty, error) {
	ctx = relayContext(ctx)
	id := req.ID
	if id == "" {
		return nil, ErrNoID
	}
	return empty, systemd.Stop(ctx, req.ID)
}

func (a *Agent) Update(ctx context.Context, req *v1.UpdateRequest) (*v1.UpdateResponse, error) {
	ctx = relayContext(ctx)
	ctx, done, err := a.client.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer done(ctx)
	container, err := a.client.LoadContainer(ctx, req.Container.ID)
	if err != nil {
		return nil, err
	}
	current, err := opts.GetConfig(ctx, container)
	if err != nil {
		return nil, err
	}
	// set all current services into maintaince mode
	for name := range current.Services {
		if err := a.register.EnableMaintainance(container.ID(), name, "update container configuration"); err != nil {
			logrus.WithError(err).Errorf("enable maintaince %s-%s", container.ID(), name)
		}
	}
	volumeRoot, err := redis.String(a.doLocal("GET", v1.VolumeRootKey))
	if err != nil && err != redis.ErrNil {
		return nil, err
	}
	var changes []change
	for name := range current.Services {
		if _, ok := req.Container.Services[name]; !ok {
			// if the new config does not have a service, deregister the old one
			changes = append(changes, &deregisterChange{
				register: a.register,
				name:     name,
			})
		}
	}
	changes = append(changes, &imageUpdateChange{
		ref:    req.Container.Image,
		client: a.client,
		a:      a,
	})
	changes = append(changes, &configChange{
		client:     a.client,
		c:          req.Container,
		volumeRoot: volumeRoot,
	})
	changes = append(changes, &filesChange{
		c:     req.Container,
		store: a.store,
	})

	var wait <-chan containerd.ExitStatus
	// bump the task to pickup the changes
	task, err := container.Task(ctx, nil)
	if err != nil {
		if !errdefs.IsNotFound(err) {
			return nil, err
		}
	}
	if task != nil {
		if wait, err = task.Wait(ctx); err != nil {
			return nil, err
		}
	} else {
		c := make(chan containerd.ExitStatus)
		wait = c
		close(c)
	}
	err = pauseAndRun(ctx, container, func() error {
		for _, ch := range changes {
			if err := ch.update(ctx, container); err != nil {
				return err
			}
		}
		if task == nil {
			return nil
		}
		return task.Kill(ctx, unix.SIGTERM)
	})
	if err != nil {
		return nil, err
	}
	wctx, _ := context.WithTimeout(ctx, 10*time.Second)
	for {
		select {
		case <-wctx.Done():
			if task != nil {
				return &v1.UpdateResponse{}, task.Kill(ctx, unix.SIGKILL)
			}
			return nil, wctx.Err()
		case <-wait:
			return &v1.UpdateResponse{}, nil
		}
	}
	return &v1.UpdateResponse{}, nil
}

func (a *Agent) Rollback(ctx context.Context, req *v1.RollbackRequest) (*v1.RollbackResponse, error) {
	ctx = relayContext(ctx)
	if req.ID == "" {
		return nil, ErrNoID
	}
	ctx, done, err := a.client.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer done(ctx)
	container, err := a.client.LoadContainer(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	err = pauseAndRun(ctx, container, func() error {
		if err := container.Update(ctx, flux.WithRollback, opts.WithRollback); err != nil {
			return err
		}
		task, err := container.Task(ctx, nil)
		if err != nil {
			return err
		}
		return task.Kill(ctx, unix.SIGTERM)
	})
	if err != nil {
		return nil, err
	}
	return &v1.RollbackResponse{}, nil
}

func (a *Agent) PushBuild(ctx context.Context, req *v1.PushBuildRequest) (*types.Empty, error) {
	if req.Ref == "" {
		return nil, ErrNoRef
	}
	return a.Push(ctx, &v1.PushRequest{
		Ref:   req.Ref,
		Build: true,
	})
}

func (a *Agent) Push(ctx context.Context, req *v1.PushRequest) (*types.Empty, error) {
	if req.Ref == "" {
		return nil, ErrNoRef
	}
	if req.Build {
		ctx = namespaces.WithNamespace(ctx, "buildkit")
	} else {
		ctx = relayContext(ctx)
	}
	image, err := a.client.GetImage(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	return empty, a.client.Push(ctx, req.Ref, image.Target(), a.withPlainRemote(req.Ref))
}

func (a *Agent) Checkpoint(ctx context.Context, req *v1.CheckpointRequest) (*v1.CheckpointResponse, error) {
	ctx = relayContext(ctx)
	if req.ID == "" {
		return nil, ErrNoID
	}
	ctx, done, err := a.client.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer done(ctx)
	container, err := a.client.LoadContainer(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	info, err := container.Info(ctx)
	if err != nil {
		return nil, err
	}
	index := is.Index{
		Versioned: ver.Versioned{
			SchemaVersion: 2,
		},
		Annotations: make(map[string]string),
	}
	data, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	r := bytes.NewReader(data)
	desc, err := writeContent(ctx, a.client.ContentStore(), MediaTypeContainerInfo, req.ID+"-container-info", r)
	if err != nil {
		return nil, err
	}
	desc.Platform = &is.Platform{
		OS:           runtime.GOOS,
		Architecture: runtime.GOARCH,
	}
	index.Manifests = append(index.Manifests, desc)

	opts := options.CheckpointOptions{
		Exit:                req.Exit,
		OpenTcp:             false,
		ExternalUnixSockets: false,
		Terminal:            false,
		FileLocks:           true,
		EmptyNamespaces:     nil,
	}
	any, err := typeurl.MarshalAny(&opts)
	if err != nil {
		return nil, err
	}
	err = pauseAndRun(ctx, container, func() error {
		// checkpoint rw layer
		opts := []diff.Opt{
			diff.WithReference(fmt.Sprintf("checkpoint-rw-%s", info.SnapshotKey)),
			diff.WithMediaType(is.MediaTypeImageLayer),
		}
		rw, err := rootfs.CreateDiff(ctx,
			info.SnapshotKey,
			a.client.SnapshotService(info.Snapshotter),
			a.client.DiffService(),
			opts...,
		)
		if err != nil {
			return err
		}
		rw.Platform = &is.Platform{
			OS:           runtime.GOOS,
			Architecture: runtime.GOARCH,
		}
		index.Manifests = append(index.Manifests, rw)
		if req.Live {
			task, err := a.client.TaskService().Checkpoint(ctx, &tasks.CheckpointTaskRequest{
				ContainerID: req.ID,
				Options:     any,
			})
			if err != nil {
				return err
			}
			for _, d := range task.Descriptors {
				if d.MediaType == images.MediaTypeContainerd1CheckpointConfig {
					// we will save the entire container config to the checkpoint instead
					continue
				}
				index.Manifests = append(index.Manifests, is.Descriptor{
					MediaType: d.MediaType,
					Size:      d.Size_,
					Digest:    d.Digest,
					Platform: &is.Platform{
						OS:           runtime.GOOS,
						Architecture: runtime.GOARCH,
					},
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if desc, err = a.writeIndex(ctx, &index, req.ID+"index"); err != nil {
		return nil, err
	}
	i := images.Image{
		Name:   req.Ref,
		Target: desc,
	}
	if _, err := a.client.ImageService().Create(ctx, i); err != nil {
		return nil, err
	}
	if req.Exit {
		if err := systemd.Stop(ctx, req.ID); err != nil {
			return nil, errors.Wrap(err, "stop service")
		}
	}
	return &v1.CheckpointResponse{}, nil
}

func (a *Agent) Restore(ctx context.Context, req *v1.RestoreRequest) (*v1.RestoreResponse, error) {
	ctx = relayContext(ctx)
	if req.Ref == "" {
		return nil, ErrNoRef
	}
	checkpoint, err := a.client.GetImage(ctx, req.Ref)
	if err != nil {
		if !errdefs.IsNotFound(err) {
			return nil, err
		}
		ck, err := a.client.Fetch(ctx, req.Ref, a.withPlainRemote(req.Ref))
		if err != nil {
			return nil, err
		}
		checkpoint = containerd.NewImage(a.client, ck)
	}
	store := a.client.ContentStore()
	index, err := decodeIndex(ctx, store, checkpoint.Target())
	if err != nil {
		return nil, err
	}
	configDesc, err := getByMediaType(index, MediaTypeContainerInfo)
	if err != nil {
		return nil, err
	}
	data, err := content.ReadBlob(ctx, store, *configDesc)
	if err != nil {
		return nil, err
	}
	var c containers.Container
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	config, err := opts.GetConfigFromInfo(ctx, c)
	if err != nil {
		return nil, err
	}
	image, err := a.client.Pull(ctx, config.Image, containerd.WithPullUnpack, a.withPlainRemote(config.Image))
	if err != nil {
		return nil, err
	}
	volumeRoot, err := redis.String(a.doLocal("GET", v1.VolumeRootKey))
	if err != nil && err != redis.ErrNil {
		return nil, err
	}
	o := []containerd.NewContainerOpts{
		flux.WithNewSnapshot(image),
		opts.WithBossConfig(volumeRoot, config, image),
	}
	if req.Live {
		desc, err := getByMediaType(index, images.MediaTypeContainerd1Checkpoint)
		if err != nil {
			return nil, err
		}
		o = append(o, opts.WithRestore(desc))
	}
	container, err := a.client.NewContainer(ctx,
		config.ID,
		o...,
	)
	if err != nil {
		return nil, err
	}
	// apply rw layer
	info, err := container.Info(ctx)
	if err != nil {
		return nil, err
	}
	rw, err := getByMediaType(index, is.MediaTypeImageLayerGzip)
	if err != nil {
		return nil, err
	}
	mounts, err := a.client.SnapshotService(info.Snapshotter).Mounts(ctx, info.SnapshotKey)
	if err != nil {
		return nil, err
	}
	if _, err := a.client.DiffService().Apply(ctx, *rw, mounts); err != nil {
		return nil, err
	}
	if err := a.store.Write(ctx, config); err != nil {
		container.Delete(ctx, containerd.WithSnapshotCleanup)
		return nil, err
	}
	if err := systemd.Enable(ctx, container.ID()); err != nil {
		return nil, err
	}
	if err := systemd.Start(ctx, container.ID()); err != nil {
		return nil, err
	}
	return &v1.RestoreResponse{}, nil
}

func (a *Agent) Migrate(ctx context.Context, req *v1.MigrateRequest) (*v1.MigrateResponse, error) {
	ctx = relayContext(ctx)
	if req.ID == "" {
		return nil, ErrNoID
	}
	to, err := api.Agent(req.To)
	if err != nil {
		return nil, err
	}
	defer to.Close()
	if _, err := to.Get(ctx, &v1.GetRequest{
		ID: req.ID,
	}); err == nil {
		return nil, errServiceExistsOnTarget
	}
	if _, err := a.Checkpoint(ctx, &v1.CheckpointRequest{
		ID:   req.ID,
		Live: req.Live,
		Ref:  req.Ref,
		Exit: req.Stop || req.Delete,
	}); err != nil {
		return nil, err
	}
	defer a.client.ImageService().Delete(ctx, req.Ref)
	if _, err := a.Push(ctx, &v1.PushRequest{
		Ref: req.Ref,
	}); err != nil {
		return nil, err
	}
	if _, err := to.Restore(ctx, &v1.RestoreRequest{
		Ref:  req.Ref,
		Live: req.Live,
	}); err != nil {
		return nil, err
	}
	if req.Delete {
		if _, err := a.Delete(ctx, &v1.DeleteRequest{
			ID: req.ID,
		}); err != nil {
			return nil, err
		}
	}
	return &v1.MigrateResponse{}, nil
}

func (a *Agent) Nodes(ctx context.Context, req *v1.NodesRequest) (*v1.NodesResponse, error) {
	me, err := a.node.LocalNode()
	if err != nil {
		return nil, err
	}
	peers, err := a.node.Peers()
	if err != nil {
		return nil, err
	}
	var resp v1.NodesResponse
	for _, p := range append(peers, me) {
		resp.Nodes = append(resp.Nodes, &v1.Node{
			ID:      p.Name,
			Address: p.Addr,
			Labels:  p.Labels,
		})
	}
	return &resp, nil
}

func (a *Agent) doLocal(action string, args ...interface{}) (interface{}, error) {
	conn := a.local.Get()
	defer conn.Close()
	return conn.Do(action, args...)
}

var (
	errServiceExistsOnTarget = errors.New("service exists on target")
	errMediaTypeNotFound     = errors.New("media type not found in index")
)

func getByMediaType(index *is.Index, mt string) (*is.Descriptor, error) {
	for _, d := range index.Manifests {
		if d.MediaType == mt {
			return &d, nil
		}
	}
	return nil, errMediaTypeNotFound
}

func (a *Agent) withPlainRemote(ref string) containerd.RemoteOpt {
	remote := strings.SplitN(ref, "/", 2)[0]
	return func(_ *containerd.Client, ctx *containerd.RemoteContext) error {
		ok, err := redis.Bool(a.doLocal("SISMEMBER", v1.PlainRemotesKey, remote))
		if err != nil && err != redis.ErrNil {
			return err
		}
		ctx.Resolver = docker.NewResolver(docker.ResolverOptions{
			PlainHTTP: ok,
			Client:    http.DefaultClient,
		})
		return nil
	}
}

func (a *Agent) handleResolvConf() error {
	peers, err := a.node.Peers()
	if err != nil {
		return err
	}
	me, err := a.node.LocalNode()
	if err != nil {
		return err
	}
	if err := writeResolvConf(append(peers, me)); err != nil {
		return err
	}
	c := make(chan *element.NodeEvent, 32)
	a.node.Subscribe(c)
	go func() {
		for range c {
			if err := writeResolvConf(append(peers, me)); err != nil {
				logrus.WithError(err).Error("update resolv config")
			}
		}
	}()
	return nil
}

func getBindSizes(c *v1.Container) (size int64, _ error) {
	for _, m := range c.Mounts {
		f, err := os.Open(m.Source)
		if err != nil {
			logrus.WithError(err).Warnf("unable to open bind for size %s", m.Source)
			continue
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return size, err
		}
		if info.IsDir() {
			f.Close()
			if err := filepath.Walk(m.Source, func(path string, wi os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if wi.IsDir() {
					return nil
				}
				size += wi.Size()
				return nil
			}); err != nil {
				return size, err
			}
			continue
		}
		size += info.Size()
		f.Close()
	}
	return size, nil
}

func relayContext(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, v1.DefaultNamespace)
}

func (a *Agent) writeIndex(ctx context.Context, index *is.Index, ref string) (d is.Descriptor, err error) {
	labels := map[string]string{}
	for i, m := range index.Manifests {
		labels[fmt.Sprintf("containerd.io/gc.ref.content.%d", i)] = m.Digest.String()
	}
	data, err := json.Marshal(index)
	if err != nil {
		return is.Descriptor{}, err
	}
	return writeContent(ctx, a.client.ContentStore(), is.MediaTypeImageIndex, ref, bytes.NewReader(data), content.WithLabels(labels))
}

func writeContent(ctx context.Context, store content.Ingester, mediaType, ref string, r io.Reader, opts ...content.Opt) (d is.Descriptor, err error) {
	writer, err := store.Writer(ctx, content.WithRef(ref))
	if err != nil {
		return d, err
	}
	defer writer.Close()
	size, err := io.Copy(writer, r)
	if err != nil {
		return d, err
	}
	if err := writer.Commit(ctx, size, "", opts...); err != nil {
		return d, err
	}
	return is.Descriptor{
		MediaType: mediaType,
		Digest:    writer.Digest(),
		Size:      size,
	}, nil
}

func decodeIndex(ctx context.Context, store content.Provider, desc is.Descriptor) (*is.Index, error) {
	var index is.Index
	p, err := content.ReadBlob(ctx, store, desc)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(p, &index); err != nil {
		return nil, err
	}
	return &index, nil
}

func writeResolvConf(peers []*element.PeerAgent) error {
	f, err := os.OpenFile(filepath.Join(v1.Root, "resolv.conf"), os.O_TRUNC|os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, p := range peers {
		host, _, err := net.SplitHostPort(p.Addr)
		if err != nil {
			return err
		}
		if _, err := f.WriteString(fmt.Sprintf("nameserver %s\n", host)); err != nil {
			return err
		}
	}
	return nil
}
