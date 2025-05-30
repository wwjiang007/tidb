// Copyright 2024 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logclient

import (
	"context"

	"github.com/pingcap/errors"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/kvproto/pkg/encryptionpb"
	"github.com/pingcap/tidb/br/pkg/checkpoint"
	"github.com/pingcap/tidb/br/pkg/glue"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/br/pkg/stream"
	"github.com/pingcap/tidb/br/pkg/utils/iter"
	"github.com/pingcap/tidb/pkg/domain"
)

var (
	FilterFilesByRegion = filterFilesByRegion
	PitrIDMapsFilename  = pitrIDMapsFilename
)

func (metaname *MetaName) Meta() Meta {
	return metaname.meta
}

func NewMetaName(meta Meta, name string) *MetaName {
	return &MetaName{meta: meta, name: name}
}

func NewMigrationBuilder(shiftStartTS, startTS, restoredTS uint64) *WithMigrationsBuilder {
	return &WithMigrationsBuilder{
		shiftStartTS: shiftStartTS,
		startTS:      startTS,
		restoredTS:   restoredTS,
	}
}

func (m *MetaWithMigrations) StoreId() int64 {
	return m.meta.StoreId
}

func (m *MetaWithMigrations) Meta() *backuppb.Metadata {
	return m.meta
}

func (m *PhysicalWithMigrations) PhysicalLength() uint64 {
	return m.physical.Item.Length
}

func (m *PhysicalWithMigrations) Physical() *backuppb.DataFileGroup {
	return m.physical.Item
}

func (rc *LogClient) TEST_saveIDMap(
	ctx context.Context,
	m *stream.TableMappingManager,
	logCheckpointMetaManager checkpoint.LogMetaManagerT,
) error {
	return rc.SaveIdMapWithFailPoints(ctx, m, logCheckpointMetaManager)
}

func (rc *LogClient) TEST_initSchemasMap(
	ctx context.Context,
	restoreTS uint64,
	logCheckpointMetaManager checkpoint.LogMetaManagerT,
) ([]*backuppb.PitrDBMap, error) {
	return rc.loadSchemasMap(ctx, restoreTS, logCheckpointMetaManager)
}

// readStreamMetaByTS is used for streaming task. collect all meta file by TS, it is for test usage.
func (lm *LogFileManager) ReadStreamMeta(ctx context.Context) ([]*MetaName, error) {
	metas, err := lm.streamingMeta(ctx)
	if err != nil {
		return nil, err
	}
	r := iter.CollectAll(ctx, metas)
	if r.Err != nil {
		return nil, errors.Trace(r.Err)
	}
	return r.Item, nil
}

func TEST_NewLogClient(clusterID, startTS, restoreTS, upstreamClusterID uint64, dom *domain.Domain, se glue.Session) *LogClient {
	return &LogClient{
		dom:               dom,
		unsafeSession:     se,
		upstreamClusterID: upstreamClusterID,
		restoreID:         0,
		LogFileManager: &LogFileManager{
			startTS:   startTS,
			restoreTS: restoreTS,
		},
		clusterID: clusterID,
	}
}

func (rc *LogClient) SetUseCheckpoint() {
	rc.useCheckpoint = true
}

func TEST_NewLogFileManager(startTS, restoreTS, shiftStartTS uint64, helper streamMetadataHelper) *LogFileManager {
	return &LogFileManager{
		startTS:      startTS,
		restoreTS:    restoreTS,
		shiftStartTS: shiftStartTS,
		helper:       helper,
	}
}

type FakeStreamMetadataHelper struct {
	streamMetadataHelper

	Data []byte
}

func (helper *FakeStreamMetadataHelper) ReadFile(
	ctx context.Context,
	path string,
	offset uint64,
	length uint64,
	compressionType backuppb.CompressionType,
	storage storage.ExternalStorage,
	encryptionInfo *encryptionpb.FileEncryptionInfo,
) ([]byte, error) {
	return helper.Data[offset : offset+length], nil
}

func (w *WithMigrations) AddIngestedSSTs(extPath string) {
	w.fullBackups = append(w.fullBackups, extPath)
}

func (w *WithMigrations) SetRestoredTS(ts uint64) {
	w.restoredTS = ts
}

func (w *WithMigrations) SetStartTS(ts uint64) {
	w.startTS = ts
}

func (w *WithMigrations) CompactionDirs() []string {
	return w.compactionDirs
}
