/*
Copyright (c) 2021 PaddlePaddle Authors. All Rights Reserve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package app

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path"
	"strings"

	libfuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	"paddleflow/cmd/fs/fuse/app/options"
	"paddleflow/pkg/client"
	"paddleflow/pkg/common/config"
	"paddleflow/pkg/common/http/api"
	"paddleflow/pkg/common/logger"
	"paddleflow/pkg/fs/client/base"
	"paddleflow/pkg/fs/client/fuse"
	"paddleflow/pkg/fs/client/meta"
	"paddleflow/pkg/fs/client/vfs"
	"paddleflow/pkg/fs/common"
	mountUtil "paddleflow/pkg/fs/utils/mount"
	"paddleflow/pkg/metric"
)

var opts *libfuse.MountOptions

func initConfig() {
	// init from yaml config
	config.InitFuseConfig()
	f := options.NewFuseOption()
	f.InitFlag(pflag.CommandLine)
	logger.Init(&config.FuseConf.Log)
	fmt.Printf("The final fuse config is:\n %s \n", config.PrettyFormat(config.FuseConf))
}

func Init() error {
	initConfig()
	opts = &libfuse.MountOptions{}
	fuseConf := config.FuseConf.Fuse
	if len(fuseConf.MountPoint) == 0 || fuseConf.MountPoint == "/" {
		log.Errorf("invalid mount point: [%s]", fuseConf.MountPoint)
		return fmt.Errorf("invalid mountpoint: %s", fuseConf.MountPoint)
	}

	isMounted, err := mountUtil.IsMountPoint(fuseConf.MountPoint)
	if err != nil {
		log.Errorf("check mount point failed: %v", err)
		return fmt.Errorf("check mountpoint failed: %v", err)
	}
	if isMounted {
		return fmt.Errorf("%s is already the mountpoint", fuseConf.MountPoint)
	}

	if len(fuseConf.MountOptions) != 0 {
		mopts := strings.Split(fuseConf.MountOptions, ",")
		opts.Options = append(opts.Options, mopts...)
	}

	if config.FuseConf.Log.Level == "DEBUG" {
		opts.Debug = true
	}

	opts.IgnoreSecurityLabels = fuseConf.IgnoreSecurityLabels
	opts.DisableXAttrs = fuseConf.DisableXAttrs
	opts.AllowOther = fuseConf.AllowOther

	// Wrap the default registry, all prometheus.MustRegister() calls should be afterwards
	// InitVFS() has many registers, should be after wrapRegister()
	wrapRegister(fuseConf.MountPoint)
	if err := InitVFS(); err != nil {
		log.Errorf("init vfs failed: %v", err)
		return err
	}
	return nil
}

func exposeMetrics(conf config.Fuse) string {
	// default set
	ip, _, err := net.SplitHostPort(conf.Server)
	if err != nil {
		log.Fatalf("metrics format error: %v", err)
	}
	port := conf.MetricsPort
	log.Debugf("metrics server - ip:%s, port:%d", ip, port)
	go metric.UpdateMetrics()
	http.Handle("/metrics", promhttp.HandlerFor(
		prometheus.DefaultGatherer,
		promhttp.HandlerOpts{
			// Opt into OpenMetrics to support exemplars.
			EnableOpenMetrics: true,
		},
	))
	prometheus.MustRegister(prometheus.NewBuildInfoCollector())
	metricsAddr := fmt.Sprintf(":%d", port)
	go func() {
		if err := http.ListenAndServe(metricsAddr, nil); err != nil {
			log.Errorf("metrics ListenAndServe error: %s", err)
		}
	}()

	log.Infof("metrics listening on %s", metricsAddr)
	return metricsAddr
}

func wrapRegister(mountPoint string) {
	registry := prometheus.NewRegistry() // replace default so only pfs-fuse metrics are exposed
	prometheus.DefaultGatherer = registry
	metricLabels := prometheus.Labels{"mp": mountPoint}
	prometheus.DefaultRegisterer = prometheus.WrapRegistererWithPrefix("pfs_",
		prometheus.WrapRegistererWith(metricLabels, registry))
	prometheus.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	prometheus.MustRegister(prometheus.NewGoCollector())
}

func Mount() (*libfuse.Server, error) {
	fuseConf := config.FuseConf.Fuse
	metricsAddr := exposeMetrics(fuseConf)
	log.Debugf("mount opts: %+v, metricsAddr: %s", opts, metricsAddr)
	return mount(fuseConf.MountPoint, opts)
}

func mount(dir string, mops *libfuse.MountOptions) (*libfuse.Server, error) {
	fuseConfig := config.FuseConf.Fuse
	mops.AllowOther = fuseConfig.AllowOther
	mops.DisableXAttrs = fuseConfig.DisableXAttrs
	server, err := fuse.Server(dir, *mops)

	return server, err
}

func InitVFS() error {
	var fsMeta common.FSMeta
	var links map[string]common.FSMeta
	fuseConf := config.FuseConf.Fuse
	if fuseConf.Local == true {
		if fuseConf.LocalRoot == "" || fuseConf.LocalRoot == "/" {
			log.Errorf("invalid localRoot: [%s]", fuseConf.LocalRoot)
			return fmt.Errorf("invalid localRoot: [%s]", fuseConf.LocalRoot)
		}
		fsMeta = common.FSMeta{
			UfsType: common.LocalType,
			SubPath: fuseConf.LocalRoot,
		}
		if fuseConf.LinkPath != "" && fuseConf.LinkRoot != "" {
			links = map[string]common.FSMeta{
				path.Clean(fuseConf.LinkPath): common.FSMeta{
					UfsType: common.LocalType,
					SubPath: fuseConf.LinkRoot,
				},
			}
		}
	} else if fuseConf.FsInfoPath != "" {
		reader, err := os.Open(fuseConf.FsInfoPath)
		if err != nil {
			return err
		}
		data, err := ioutil.ReadAll(reader)
		if err != nil {
			return err
		}
		err = json.Unmarshal(data, &fsMeta)
		if err != nil {
			log.Errorf("get fsInfo fail: %s", err.Error())
			return err
		}
		fsMeta.UfsType = fsMeta.Type
		fsMeta.Type = "fs"
		log.Infof("fuse meta is %+v", fsMeta)
		links = map[string]common.FSMeta{}
	} else {
		if fuseConf.FsID == "" {
			log.Errorf("invalid fsID: [%s]", fuseConf.FsID)
			return fmt.Errorf("invalid fsID: [%s]", fuseConf.FsID)
		}

		if fuseConf.Server == "" {
			log.Errorf("invalid server: [%s]", fuseConf.Server)
			return fmt.Errorf("invalid server: [%s]", fuseConf.Server)
		}

		if fuseConf.UserName == "" {
			log.Errorf("invalid username: [%s]", fuseConf.UserName)
			return fmt.Errorf("invalid username: [%s]", fuseConf.UserName)
		}

		if fuseConf.Password == "" {
			log.Errorf("invalid password: [%s]", fuseConf.Password)
			return fmt.Errorf("invalid password: [%s]", fuseConf.Password)
		}

		httpClient := client.NewHttpClient(fuseConf.Server, client.DefaultTimeOut)

		// 获取token
		login := api.LoginParams{
			UserName: fuseConf.UserName,
			Password: fuseConf.Password,
		}
		loginResponse, err := api.LoginRequest(login, httpClient)
		if err != nil {
			log.Errorf("fuse login failed: %v", err)
			return err
		}

		fuseClient, err := base.NewClient(fuseConf.FsID, httpClient, fuseConf.UserName, loginResponse.Authorization)
		if err != nil {
			log.Errorf("init client with fs[%s] and server[%s] failed: %v", fuseConf.FsID, fuseConf.Server, err)
			return err
		}
		fsMeta, err = fuseClient.GetFSMeta()
		if err != nil {
			log.Errorf("get fs[%s] meta from pfs server[%s] failed: %v",
				fuseConf.FsID, fuseConf.Server, err)
			return err
		}
		fuseClient.FsName = fsMeta.Name
		links, err = fuseClient.GetLinks()
		if err != nil {
			log.Errorf("get fs[%s] links from pfs server[%s] failed: %v",
				fuseConf.FsID, fuseConf.Server, err)
			return err
		}
	}
	options := []vfs.Option{
		vfs.WithMemorySize(config.FuseConf.Fuse.MemorySize),
		vfs.WithMemoryExpire(config.FuseConf.Fuse.MemoryExpire),
		vfs.WithBlockSize(config.FuseConf.Fuse.BlockSize),
		vfs.WithDiskCachePath(config.FuseConf.Fuse.DiskCachePath),
		vfs.WithDiskExpire(config.FuseConf.Fuse.DiskExpire),
	}
	if !config.FuseConf.Fuse.RawOwner {
		options = append(options, vfs.WithOwner(config.FuseConf.Fuse.Uid,
			config.FuseConf.Fuse.Gid))
	}
	options = append(options, vfs.WithMetaConfig(meta.Config{
		AttrCacheExpire:  config.FuseConf.Fuse.MetaCacheExpire,
		EntryCacheExpire: config.FuseConf.Fuse.EntryCacheExpire,
		Driver:           config.FuseConf.Fuse.MetaDriver,
		CachePath:        config.FuseConf.Fuse.MetaCachePath,
	}))
	vfsConfig := vfs.InitConfig(options...)

	if _, err := vfs.InitVFS(fsMeta, links, true, vfsConfig); err != nil {
		log.Errorf("init vfs failed: %v", err)
		return err
	}
	return nil
}