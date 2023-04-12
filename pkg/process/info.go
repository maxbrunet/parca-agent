// Copyright 2023 The Parca Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package process

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	burrow "github.com/goburrow/cache"
	"github.com/hashicorp/go-multierror"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/parca-dev/parca-agent/pkg/cache"
	"github.com/parca-dev/parca-agent/pkg/debuginfo"
	"github.com/parca-dev/parca-agent/pkg/objectfile"
)

type metrics struct{}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{}
	return m
}

type InfoManager struct {
	metrics *metrics

	logger log.Logger
	cache  burrow.Cache
	sfg    singleflight.Group

	mapManager       *MapManager
	debuginfoManager *debuginfo.Manager
}

func NewInfoManager(logger log.Logger, reg prometheus.Registerer, mm *MapManager, dim *debuginfo.Manager, profilingDuration time.Duration) *InfoManager {
	return &InfoManager{
		logger:  logger,
		metrics: newMetrics(reg),
		// TODO(kakkoyun): Convert loading cache.
		// - Does loading cache makes sure only one loading at a time?
		cache: burrow.New(
			burrow.WithMaximumSize(5000),
			// TODO(kakkoyun): Remove the comment below.
			// @nocommit: Add jitter so we don't have to recompute the information
			// at the same time for many processes if many are evicted.
			// -- This should be good because the cache entries won't be created at the same and
			// -- they won't be accessed at the same time.
			burrow.WithExpireAfterAccess(10*profilingDuration),
			burrow.WithStatsCounter(cache.NewBurrowStatsCounter(logger, reg, "process_info_cache")),
		),
		mapManager:       mm,
		debuginfoManager: dim,
		sfg:              singleflight.Group{},
	}
}

type Info struct {
	// TODO(kakkoyun): Put all the following fields in a struct.
	// - PerfMaps
	// - Unwind Information
	Mappings Mappings
}

// ObtainInfo collects the required information for a process.
func (im *InfoManager) ObtainInfo(ctx context.Context, pid int) error {
	// Cache will keep the value as long as the process is sends to the event channel.
	// See the cache initialization for the eviction policy and the eviction TTL.
	_, exists := im.cache.GetIfPresent(pid)
	if exists {
		return nil
	}

	_, err, _ := im.sfg.Do(strconv.Itoa(pid), func() (interface{}, error) {
		mappings, err := im.mapManager.MappingsForPID(pid)
		if err != nil {
			return nil, err
		}

		// Upload debug information of the discovered object files.
		if im.debuginfoManager != nil {
			if err := im.ensureDebugInfoUploaded(ctx, pid, mappings); err != nil {
				level.Warn(im.logger).Log("msg", "failed to upload debug information", "err", err)
			}
		}

		im.cache.Put(pid, Info{
			Mappings: mappings,
		})
		return nil, nil
	})

	return err
}

// TODO(kakkoyun) Add metrics !!
func (im *InfoManager) ensureDebugInfoUploaded(ctx context.Context, pid int, mappings Mappings) error {
	di := im.debuginfoManager
	// FIXME: !!!
	// TODO(kakkoyun)
	// resultCh := make(chan error) // Create struct.
	// for _, objectFile := range objectFiles {
	// 	go func(objectFile *objectfile.ObjectFile) {
	// 		// Retry logic.
	// 		resultCh <- im.debuginfoManager.Upload(ctx, objectFile)
	// 	}(objectFile)
	// }

	// TODO(kakkoyun): Retry logic.
	// TODO(kakkoyun): Immediately call extractOrFindDebugInfo.
	// TODO(kakkoyun): How to keep track of success and failures?
	// TODO(kakkoyun): Permanent failure?

	// errgroup.WithContext doesn't work for this use case, we want to continue uploading even if one fails.
	g := errgroup.Group{}
	// Arbitrary limit per request for back-pressure.
	gSize := 4
	g.SetLimit(gSize)

	type uploadResult struct {
		objFile *objectfile.ObjectFile

		Error error
	}
	ch := make(chan uploadResult, gSize)

	var multiErr *multierror.Error
	for _, m := range mappings {
		if !m.IsOpen() {
			// TODO(kakkoyun): Do we need this check?
			multiErr = multierror.Append(multiErr, fmt.Errorf("mapping %s is not open", m.Pathname))
			continue
		}

		objFile := m.objFile
		logger := log.With(im.logger, "buildid", objFile.BuildID, "path", objFile.Path)

		// We upload the debug information files concurrently. In case
		// of two files with the same buildID are extracted at the same
		// time, they will be written to the same file.
		//
		// Most of the time, the file is, erm, eventually consistent-ish,
		// and once all the writers are done, the debug file looks as an ELF
		// with the correct bytes.
		//
		// However, I don't believe there's any guarantees on this, so the
		// files aren't getting corrupted most of the time by sheer luck.
		//
		// The singleflight group makes sure that we don't try to extract
		// the same buildID concurrently.
		if err := di.ExtractOrFindDebugInfo(ctx, m.Root(), objFile); err != nil {
			level.Error(logger).Log("msg", "failed to ensure debuginfo is uploaded", "err", err)
		}

		g.Go(func() error {
			// TODO(kakkoyun): Add retry logic.
			if err := di.Upload(ctx, objFile); err != nil {
				ch <- uploadResult{objFile: objFile, Error: err}
			}
			ch <- uploadResult{objFile: objFile, Error: nil}

			// No need to return error here, we want to continue uploading even if one fails.
			return nil
		})
	}

	go func() {
		defer close(ch)

		if err := g.Wait(); err != nil {
			level.Error(im.logger).Log("msg", "failed to ensure debuginfo is uploaded", "err", err)
		}
	}()

	return multiErr.ErrorOrNil()
}

func (im *InfoManager) InfoForPID(pid int) (*Info, error) {
	v, ok := im.cache.GetIfPresent(pid)
	if !ok {
		// understand why an item might not be in cache
		return nil, fmt.Errorf("not in cache")
	}

	info, ok := v.(Info)
	if !ok {
		panic("received the wrong type in the info cache")
	}

	return &info, nil
}

func (i *Info) Normalize(addr uint64) (uint64, error) {
	m := i.Mappings.MappingForAddr(addr)
	if m == nil {
		return 0, errors.New("mapping is nil")
	}

	// Transform the address using calculated base address for the binary.
	normalizedAddr, err := m.Normalize(addr)
	if err != nil {
		return 0, fmt.Errorf("failed to get normalized address from object file: %w", err)
	}

	return normalizedAddr, nil
}
