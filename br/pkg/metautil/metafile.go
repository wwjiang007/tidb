// Copyright 2021 PingCAP, Inc. Licensed under Apache-2.0.

package metautil

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/docker/go-units"
	"github.com/gogo/protobuf/proto"
	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/errors"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/kvproto/pkg/encryptionpb"
	"github.com/pingcap/log"
	berrors "github.com/pingcap/tidb/br/pkg/errors"
	"github.com/pingcap/tidb/br/pkg/logutil"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/br/pkg/summary"
	"github.com/pingcap/tidb/br/pkg/utils"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/statistics/util"
	"github.com/pingcap/tidb/pkg/tablecodec"
	tidbutil "github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/encrypt"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	// LockFile represents file name
	LockFile = "backup.lock"
	// MetaFile represents file name
	MetaFile = "backupmeta"
	// MetaJSONFile represents backup meta json file name
	MetaJSONFile = "jsons/backupmeta.json"
	// MaxBatchSize represents the internal channel buffer size of MetaWriter and MetaReader.
	MaxBatchSize = 1024

	// MetaFileSize represents the limit size of one MetaFile
	MetaFileSize = 128 * units.MiB

	// CrypterIvLen represents the length of iv of crypter method
	CrypterIvLen = 16
)

const (
	// MetaV1 represents the old version of backupmeta.
	// because the old version doesn't have version field, so set it to 0 for compatibility.
	MetaV1 = iota
	// MetaV2 represents the new version of backupmeta.
	MetaV2
)

// Encrypt encrypts the content according to CipherInfo.
func Encrypt(content []byte, cipher *backuppb.CipherInfo) (encryptedContent, iv []byte, err error) {
	if len(content) == 0 || cipher == nil {
		return content, iv, nil
	}

	switch cipher.CipherType {
	case encryptionpb.EncryptionMethod_PLAINTEXT:
		return content, iv, nil
	case encryptionpb.EncryptionMethod_AES128_CTR,
		encryptionpb.EncryptionMethod_AES192_CTR,
		encryptionpb.EncryptionMethod_AES256_CTR:
		// generate random iv for aes crypter
		iv = make([]byte, CrypterIvLen)
		_, err = rand.Read(iv)
		if err != nil {
			return content, iv, errors.Trace(err)
		}
		encryptedContent, err = encrypt.AESEncryptWithCTR(content, cipher.CipherKey, iv)
		return
	default:
		return content, iv, errors.Annotate(berrors.ErrInvalidArgument, "cipher type invalid")
	}
}

func DecryptFullBackupMetaIfNeeded(metaData []byte, cipherInfo *backuppb.CipherInfo) ([]byte, error) {
	if cipherInfo == nil || !utils.IsEffectiveEncryptionMethod(cipherInfo.CipherType) {
		return metaData, nil
	}
	// the prefix of backup meta file is iv(16 bytes) for ctr mode if encryption method is valid
	iv := metaData[:CrypterIvLen]
	decryptBackupMeta, err := utils.Decrypt(metaData[len(iv):], cipherInfo, iv)
	if err != nil {
		return nil, errors.Annotate(err, "decrypt failed with wrong key")
	}
	return decryptBackupMeta, nil
}

// walkLeafMetaFile walks the leaves of the given metafile, and deal with it by calling the function `output`.
// Notice: the function `output` should be thread safe.
func walkLeafMetaFile(
	ctx context.Context,
	storage storage.ExternalStorage,
	file *backuppb.MetaFile,
	cipher *backuppb.CipherInfo,
	output func(*backuppb.MetaFile)) error {
	if file == nil {
		return nil
	}
	if len(file.MetaFiles) == 0 {
		output(file)
		return nil
	}
	eg, ectx := errgroup.WithContext(ctx)
	workers := tidbutil.NewWorkerPool(8, "download files workers")
	for _, node := range file.MetaFiles {
		workers.ApplyOnErrorGroup(eg, func() error {
			content, err := storage.ReadFile(ectx, node.Name)
			if err != nil {
				return errors.Trace(err)
			}

			decryptContent, err := utils.Decrypt(content, cipher, node.CipherIv)
			if err != nil {
				return errors.Trace(err)
			}

			checksum := sha256.Sum256(decryptContent)
			if !bytes.Equal(node.Sha256, checksum[:]) {
				return berrors.ErrInvalidMetaFile.GenWithStackByArgs(fmt.Sprintf(
					"checksum mismatch expect %x, got %x", node.Sha256, checksum[:]))
			}

			child := &backuppb.MetaFile{}
			if err = proto.Unmarshal(decryptContent, child); err != nil {
				return errors.Trace(err)
			}

			// the max depth of the root metafile is only 1.
			// ASSERT: len(child.MetaFiles) == 0
			if err = walkLeafMetaFile(ectx, storage, child, cipher, output); err != nil {
				return errors.Trace(err)
			}

			return nil
		})
	}
	return eg.Wait()
}

// Table wraps the schema and files of a table.
type Table struct {
	DB               *model.DBInfo
	Info             *model.TableInfo
	Crc64Xor         uint64
	TotalKvs         uint64
	TotalBytes       uint64
	FilesOfPhysicals map[int64][]*backuppb.File
	TiFlashReplicas  int
	Stats            *util.JSONTable
	StatsFileIndexes []*backuppb.StatsFileIndex
}

// MetaReader wraps a reader to read both old and new version of backupmeta.
type MetaReader struct {
	storage    storage.ExternalStorage
	backupMeta *backuppb.BackupMeta
	cipher     *backuppb.CipherInfo
}

// NewMetaReader creates MetaReader.
func NewMetaReader(
	backupMeta *backuppb.BackupMeta,
	storage storage.ExternalStorage,
	cipher *backuppb.CipherInfo) *MetaReader {
	return &MetaReader{
		storage:    storage,
		backupMeta: backupMeta,
		cipher:     cipher,
	}
}

func (reader *MetaReader) readDDLs(ctx context.Context, output func([]byte)) error {
	// Read backupmeta v1 metafiles.
	// if the backupmeta equals to v1, or doesn't not exists(old version).
	if reader.backupMeta.Version == MetaV1 {
		output(reader.backupMeta.Ddls)
		return nil
	}
	// Read backupmeta v2 metafiles.
	outputFn := func(m *backuppb.MetaFile) {
		for _, s := range m.Ddls {
			output(s)
		}
	}
	return walkLeafMetaFile(ctx, reader.storage, reader.backupMeta.DdlIndexes, reader.cipher, outputFn)
}

func (reader *MetaReader) readSchemas(ctx context.Context, output func(*backuppb.Schema)) error {
	// Read backupmeta v1 metafiles.
	for _, s := range reader.backupMeta.Schemas {
		output(s)
	}
	// Read backupmeta v2 metafiles.
	outputFn := func(m *backuppb.MetaFile) {
		for _, s := range m.Schemas {
			output(s)
		}
	}
	return walkLeafMetaFile(ctx, reader.storage, reader.backupMeta.SchemaIndex, reader.cipher, outputFn)
}

func (reader *MetaReader) readDataFiles(ctx context.Context, output func(*backuppb.File)) error {
	// Read backupmeta v1 data files.
	for _, f := range reader.backupMeta.Files {
		output(f)
	}
	// Read backupmeta v2 data files.
	outputFn := func(m *backuppb.MetaFile) {
		for _, f := range m.DataFiles {
			output(f)
		}
	}
	return walkLeafMetaFile(ctx, reader.storage, reader.backupMeta.FileIndex, reader.cipher, outputFn)
}

// ArchiveSize return the size of Archive data
func ArchiveSize(files []*backuppb.File) uint64 {
	total := uint64(0)
	for _, file := range files {
		total += file.Size_
	}
	return total
}

// ArchiveTablesSize return the size of archive tables
func ArchiveTablesSize(tables []*Table) uint64 {
	totalSize := uint64(0)
	for _, table := range tables {
		totalSize += ArchiveTableSize(table)
	}
	return totalSize
}

// ArchiveTableSize return the size of archive table
func ArchiveTableSize(table *Table) uint64 {
	totalSize := uint64(0)
	for _, files := range table.FilesOfPhysicals {
		for _, file := range files {
			totalSize += file.GetSize_()
		}
	}
	return totalSize
}

type ChecksumStats struct {
	Crc64Xor   uint64
	TotalKvs   uint64
	TotalBytes uint64
}

func (stats ChecksumStats) ChecksumExists() bool {
	if stats.Crc64Xor == 0 && stats.TotalKvs == 0 && stats.TotalBytes == 0 {
		return false
	}
	return true
}

// CalculateChecksumStatsOnFiles returns the ChecksumStats for the given files
func (table *Table) CalculateChecksumStatsOnFiles() ChecksumStats {
	var stats ChecksumStats
	for _, files := range table.FilesOfPhysicals {
		for _, file := range files {
			stats.Crc64Xor ^= file.Crc64Xor
			stats.TotalKvs += file.TotalKvs
			stats.TotalBytes += file.TotalBytes
		}
	}
	return stats
}

// ReadDDLs reads the ddls from the backupmeta.
// This function is compatible with the old backupmeta.
func (reader *MetaReader) ReadDDLs(ctx context.Context) ([]byte, error) {
	var err error
	ch := make(chan any, MaxBatchSize)
	errCh := make(chan error)
	go func() {
		if err = reader.readDDLs(ctx, func(s []byte) { ch <- s }); err != nil {
			errCh <- errors.Trace(err)
		}
		close(ch)
	}()

	var ddlBytes []byte
	var ddlBytesArray [][]byte
	for {
		itemCount := 0
		err := receiveBatch(ctx, errCh, ch, MaxBatchSize, func(item any) error {
			itemCount++
			if reader.backupMeta.Version == MetaV1 {
				ddlBytes = item.([]byte)
			} else {
				// we collect all ddls from files.
				ddlBytesArray = append(ddlBytesArray, item.([]byte))
			}
			return nil
		})
		if err != nil {
			return nil, errors.Trace(err)
		}

		// finish read
		if itemCount == 0 {
			if len(ddlBytesArray) != 0 {
				ddlBytes = mergeDDLs(ddlBytesArray)
			}
			return ddlBytes, nil
		}
	}
}

type readSchemaConfig struct {
	skipFiles bool
	skipStats bool
}

// ReadSchemaOption describes some extra option of reading the config.
type ReadSchemaOption func(*readSchemaConfig)

// SkipFiles is the configuration which will make the schema reader skip all files.
// This is useful when only schema information is needed.
func SkipFiles(conf *readSchemaConfig) {
	conf.skipFiles = true
}

func SkipStats(conf *readSchemaConfig) {
	conf.skipStats = true
}

// GetBasic returns a basic copy of the backup meta.
func (reader *MetaReader) GetBasic() backuppb.BackupMeta {
	return *reader.backupMeta
}

// ReadSchemasFiles reads the schema and datafiles from the backupmeta.
// This function is compatible with the old backupmeta.
func (reader *MetaReader) ReadSchemasFiles(ctx context.Context, output chan<- *Table, opts ...ReadSchemaOption) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cfg := readSchemaConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	ch := make(chan any, MaxBatchSize)
	schemaCh := make(chan *backuppb.Schema, MaxBatchSize)
	// Make sure these 2 goroutine avoid to blocked by the errCh.
	// And the second error in the errCh is not the root cause error.
	errCh := make(chan error, 2)
	// download and parse metafile
	go func() {
		defer close(schemaCh)
		if err := reader.readSchemas(cctx, func(s *backuppb.Schema) {
			if cfg.skipStats {
				s.Stats = nil
				s.StatsIndex = nil
			}
			select {
			case <-cctx.Done():
			case schemaCh <- s:
			}
		}); err != nil {
			errCh <- errors.Trace(err)
		}
	}()
	// parse the schema
	go func() {
		defer close(ch)
		eg, ectx := errgroup.WithContext(cctx)
		workers := tidbutil.NewWorkerPool(8, "parse schema workers")
		for {
			select {
			case <-ectx.Done():
				errCh <- errors.Trace(ectx.Err())
				return
			case s, ok := <-schemaCh:
				if !ok {
					if err := eg.Wait(); err != nil {
						errCh <- err
					}
					return
				}
				workers.ApplyOnErrorGroup(eg, func() error {
					table, err := parseSchemaFile(s)
					if err != nil {
						return errors.Trace(err)
					}
					select {
					case <-ectx.Done():
					case ch <- table:
					}
					return nil
				})
			}
		}
	}()

	// It's not easy to balance memory and time costs for current structure.
	// put all files in memory due to https://github.com/pingcap/br/issues/705
	var fileMap map[int64][]*backuppb.File
	if !cfg.skipFiles {
		fileCh := make(chan *backuppb.File, MaxBatchSize)
		fileErrCh := make(chan error, 1)
		fileMap = make(map[int64][]*backuppb.File)
		go func() {
			defer close(fileCh)
			err := reader.readDataFiles(cctx, func(file *backuppb.File) {
				select {
				case <-cctx.Done():
				case fileCh <- file:
				}
			})
			if err != nil {
				fileErrCh <- err
			}
		}()
	generateFileMapDone:
		for {
			select {
			case <-cctx.Done():
				return errors.Trace(cctx.Err())
			case err := <-fileErrCh:
				return errors.Trace(err)
			case file, ok := <-fileCh:
				if !ok {
					break generateFileMapDone
				}
				physicalID := tablecodec.DecodeTableID(file.GetStartKey())
				if physicalID == 0 {
					log.Panic("tableID must not equal to 0", logutil.File(file))
				}
				fileMap[physicalID] = append(fileMap[physicalID], file)
			}
		}
	}

	for {
		// table ID -> *Table
		tableMap := make(map[int64]*Table, MaxBatchSize)
		err := receiveBatch(cctx, errCh, ch, MaxBatchSize, func(item any) error {
			table := item.(*Table)
			if table.Info != nil {
				if fileMap != nil {
					if files, ok := fileMap[table.Info.ID]; ok && len(files) > 0 {
						table.FilesOfPhysicals[table.Info.ID] = files
					}
					if table.Info.Partition != nil {
						// Partition table can have many table IDs (partition IDs).
						for _, p := range table.Info.Partition.Definitions {
							if files, ok := fileMap[p.ID]; ok && len(files) > 0 {
								table.FilesOfPhysicals[p.ID] = files
							}
						}
					}
				}
				tableMap[table.Info.ID] = table
			} else {
				// empty database
				tableMap[table.DB.ID] = table
			}
			return nil
		})
		if err != nil {
			return errors.Trace(err)
		}
		if len(tableMap) == 0 {
			// We have read all tables.
			return nil
		}
		for _, table := range tableMap {
			output <- table
		}
	}
}

func parseSchemaFile(s *backuppb.Schema) (*Table, error) {
	dbInfo := &model.DBInfo{}
	if err := json.Unmarshal(s.Db, dbInfo); err != nil {
		return nil, errors.Trace(err)
	}

	var tableInfo *model.TableInfo
	if s.Table != nil {
		tableInfo = &model.TableInfo{}
		if err := json.Unmarshal(s.Table, tableInfo); err != nil {
			return nil, errors.Trace(err)
		}
	}
	var stats *util.JSONTable
	if s.Stats != nil {
		stats = &util.JSONTable{}
		if err := json.Unmarshal(s.Stats, stats); err != nil {
			return nil, errors.Trace(err)
		}
	}
	var statsFileIndexes []*backuppb.StatsFileIndex
	if len(s.StatsIndex) > 0 {
		statsFileIndexes = s.StatsIndex
	}

	return &Table{
		DB:               dbInfo,
		Info:             tableInfo,
		Crc64Xor:         s.Crc64Xor,
		TotalKvs:         s.TotalKvs,
		TotalBytes:       s.TotalBytes,
		FilesOfPhysicals: make(map[int64][]*backuppb.File),
		TiFlashReplicas:  int(s.TiflashReplicas),
		Stats:            stats,
		StatsFileIndexes: statsFileIndexes,
	}, nil
}

func receiveBatch(
	ctx context.Context, errCh chan error, ch <-chan any, maxBatchSize int,
	collectItem func(any) error,
) error {
	batchSize := 0
	for {
		select {
		case <-ctx.Done():
			return errors.Trace(ctx.Err())
		case err := <-errCh:
			return errors.Trace(err)
		case s, ok := <-ch:
			if !ok {
				return nil
			}
			if err := collectItem(s); err != nil {
				return errors.Trace(err)
			}
		}
		// Return if the batch is large enough.
		batchSize++
		if batchSize >= maxBatchSize {
			return nil
		}
	}
}

// AppendOp represents the operation type of meta.
type AppendOp int

const (
	// AppendMetaFile represents the MetaFile type.
	AppendMetaFile AppendOp = 0
	// AppendDataFile represents the DataFile type.
	// it records the file meta from tikv.
	AppendDataFile AppendOp = 1
	// AppendSchema represents the schema from tidb.
	AppendSchema AppendOp = 2
	// AppendDDL represents the ddls before last backup.
	AppendDDL AppendOp = 3
)

func (op AppendOp) name() string {
	var name string
	switch op {
	case AppendMetaFile:
		name = "metafile"
	case AppendDataFile:
		name = "datafile"
	case AppendSchema:
		name = "schema"
	case AppendDDL:
		name = "ddl"
	default:
		log.Panic("unsupport op type", zap.Any("op", op))
	}
	return name
}

// appends item to MetaFile
func (op AppendOp) appendFile(a *backuppb.MetaFile, b any) (dataFileSize int, size int, itemCount int) {
	switch op {
	case AppendMetaFile:
		metaFile := b.(*backuppb.File)
		a.MetaFiles = append(a.MetaFiles, metaFile)
		size += metaFile.Size()
		itemCount++
	case AppendDataFile:
		// receive a batch of file because we need write and default sst are adjacent.
		files := b.([]*backuppb.File)
		a.DataFiles = append(a.DataFiles, files...)
		for _, f := range files {
			itemCount++
			size += f.Size()
			dataFileSize += int(f.Size_)
		}
	case AppendSchema:
		a.Schemas = append(a.Schemas, b.(*backuppb.Schema))
		itemCount++
		size += b.(*backuppb.Schema).Size()
	case AppendDDL:
		a.Ddls = append(a.Ddls, b.([]byte))
		itemCount++
		size += len(b.([]byte))
	}
	return dataFileSize, size, itemCount
}

type sizedMetaFile struct {
	// A stack like array, we always append to the last node.
	root         *backuppb.MetaFile
	dataFileSize int
	size         int
	itemNum      int
	sizeLimit    int
}

// NewSizedMetaFile represents the sizedMetaFile.
func NewSizedMetaFile(sizeLimit int) *sizedMetaFile {
	return &sizedMetaFile{
		root: &backuppb.MetaFile{
			Schemas:   make([]*backuppb.Schema, 0),
			DataFiles: make([]*backuppb.File, 0),
			RawRanges: make([]*backuppb.RawRange, 0),
		},
		sizeLimit: sizeLimit,
	}
}

func (f *sizedMetaFile) append(file any, op AppendOp) bool {
	// append to root
	// 	TODO maybe use multi level index
	dataFileSize, size, itemCount := op.appendFile(f.root, file)
	f.itemNum += itemCount
	f.size += size
	f.dataFileSize += dataFileSize
	// f.size would reset outside
	return f.size > f.sizeLimit
}

// MetaWriter represents wraps a writer, and the MetaWriter should be compatible with old version of backupmeta.
type MetaWriter struct {
	storage           storage.ExternalStorage
	metafileSizeLimit int
	// a flag to control whether we generate v1 or v2 meta.
	useV2Meta  bool
	backupMeta *backuppb.BackupMeta
	// used to generate MetaFile name.
	metafileSizes  map[string]int
	metafileSeqNum map[string]int
	metafiles      *sizedMetaFile
	// the start time of StartWriteMetas
	// it's use to calculate the time costs.
	start time.Time
	// wg waits StartWriterMetas exits
	wg sync.WaitGroup
	// internal item channel
	metasCh chan any
	errCh   chan error

	// records the total item of in one write meta job.
	flushedItemNum int

	// the filename that backupmeta has flushed into.
	metaFileName string

	cipher *backuppb.CipherInfo

	// records the total datafile size
	totalDataFileSize int

	// records the total metafile size for backupmeta v2
	totalMetaFileSize uint64
}

// NewMetaWriter creates MetaWriter.
func NewMetaWriter(
	storage storage.ExternalStorage,
	metafileSizeLimit int,
	useV2Meta bool,
	metaFileName string,
	cipher *backuppb.CipherInfo,
) *MetaWriter {
	if len(metaFileName) == 0 {
		metaFileName = MetaFile
	}

	return &MetaWriter{
		start:             time.Now(),
		storage:           storage,
		metafileSizeLimit: metafileSizeLimit,
		useV2Meta:         useV2Meta,
		// keep the compatibility for old backupmeta.Ddls
		// old version: Ddls, _ := json.Marshal(make([]*model.Job, 0))
		backupMeta:     &backuppb.BackupMeta{Ddls: []byte("[]")},
		metafileSizes:  make(map[string]int),
		metafiles:      NewSizedMetaFile(metafileSizeLimit),
		metafileSeqNum: make(map[string]int),
		metaFileName:   metaFileName,
		cipher:         cipher,
	}
}

func (writer *MetaWriter) reset() {
	writer.metasCh = make(chan any, MaxBatchSize)
	writer.errCh = make(chan error)

	// reset flushedItemNum for next meta.
	writer.flushedItemNum = 0
}

// Update updates some property of backupmeta.
func (writer *MetaWriter) Update(f func(m *backuppb.BackupMeta)) {
	f(writer.backupMeta)
}

// Send sends the item to buffer.
func (writer *MetaWriter) Send(m any, _ AppendOp) error {
	select {
	case writer.metasCh <- m:
	// receive an error from StartWriteMetasAsync
	case err := <-writer.errCh:
		return errors.Trace(err)
	}
	return nil
}

func (writer *MetaWriter) close() {
	close(writer.metasCh)
}

// StartWriteMetasAsync writes four kind of meta into backupmeta.
// 1. file
// 2. schema
// 3. ddl
// 4. rawRange( raw kv )
// when useBackupMetaV2 enabled, it will generate multi-level index backupmetav2.
// else it will generate backupmeta as before for compatibility.
// User should call FinishWriteMetas after StartWriterMetasAsync.
func (writer *MetaWriter) StartWriteMetasAsync(ctx context.Context, op AppendOp) {
	writer.reset()
	writer.start = time.Now()
	writer.wg.Add(1)
	go func() {
		defer func() {
			close(writer.errCh)
			// close errCh before metaCh closed
			writer.wg.Done()
		}()
		for {
			select {
			case <-ctx.Done():
				log.Info("exit write metas by context done")
				return
			case meta, ok := <-writer.metasCh:
				if !ok {
					log.Info("write metas finished", zap.String("type", op.name()))
					return
				}
				needFlush := writer.metafiles.append(meta, op)
				if writer.useV2Meta && needFlush {
					err := writer.flushMetasV2(ctx, op)
					if err != nil {
						writer.errCh <- err
					}
				}
			}
		}
	}()
}

// FinishWriteMetas close the channel in StartWriteMetasAsync and flush the buffered data.
func (writer *MetaWriter) FinishWriteMetas(ctx context.Context, op AppendOp) error {
	writer.close()
	// always start one goroutine to write one kind of meta.
	writer.wg.Wait()
	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("MetaWriter.Finish", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		ctx = opentracing.ContextWithSpan(ctx, span1)
	}
	var err error
	// flush the buffered meta
	if !writer.useV2Meta {
		writer.fillMetasV1(ctx, op)
	} else {
		err = writer.flushMetasV2(ctx, op)
		if err != nil {
			return errors.Trace(err)
		}
	}

	costs := time.Since(writer.start)
	if op == AppendDataFile {
		summary.CollectSuccessUnit("backup ranges", writer.flushedItemNum, costs)
	}
	log.Info("finish the write metas", zap.Int("item", writer.flushedItemNum),
		zap.String("type", op.name()), zap.Duration("costs", costs))
	return nil
}

// FlushBackupMeta flush the `backupMeta` to `ExternalStorage`
func (writer *MetaWriter) FlushBackupMeta(ctx context.Context) error {
	// Set schema version
	if writer.useV2Meta {
		writer.backupMeta.Version = MetaV2
	} else {
		writer.backupMeta.Version = MetaV1
	}

	// update the total size of backup files (include data files and meta files)
	writer.backupMeta.BackupSize = writer.MetaFilesSize() + writer.ArchiveSize() + uint64(writer.backupMeta.Size())

	// Flush the writer.backupMeta to storage
	backupMetaData, err := proto.Marshal(writer.backupMeta)
	if err != nil {
		return errors.Trace(err)
	}
	log.Debug("backup meta", zap.Reflect("meta", writer.backupMeta))
	log.Info("save backup meta", zap.Int("size", len(backupMetaData)))

	encryptBuff, iv, err := Encrypt(backupMetaData, writer.cipher)
	if err != nil {
		return errors.Trace(err)
	}

	return writer.storage.WriteFile(ctx, writer.metaFileName, append(iv, encryptBuff...))
}

// fillMetasV1 keep the compatibility for old version.
// for MetaV1, just put in backupMeta
func (writer *MetaWriter) fillMetasV1(_ context.Context, op AppendOp) {
	switch op {
	case AppendDataFile:
		writer.backupMeta.Files = writer.metafiles.root.DataFiles
	case AppendSchema:
		writer.backupMeta.Schemas = writer.metafiles.root.Schemas
		// calculate the stats file size
		for _, schema := range writer.metafiles.root.Schemas {
			for _, statsIndex := range schema.StatsIndex {
				writer.totalMetaFileSize += statsIndex.SizeEnc
			}
		}
	case AppendDDL:
		writer.backupMeta.Ddls = mergeDDLs(writer.metafiles.root.Ddls)
	default:
		log.Panic("unsupport op type", zap.Any("op", op))
	}
	writer.flushedItemNum += writer.metafiles.itemNum
}

func (writer *MetaWriter) flushMetasV2(ctx context.Context, op AppendOp) error {
	var index *backuppb.MetaFile
	switch op {
	case AppendSchema:
		if len(writer.metafiles.root.Schemas) == 0 {
			return nil
		}
		// calculate the stats file size
		for _, schema := range writer.metafiles.root.Schemas {
			for _, statsIndex := range schema.StatsIndex {
				writer.totalMetaFileSize += statsIndex.SizeEnc
			}
		}
		// Add the metafile to backupmeta and reset metafiles.
		if writer.backupMeta.SchemaIndex == nil {
			writer.backupMeta.SchemaIndex = &backuppb.MetaFile{}
		}
		index = writer.backupMeta.SchemaIndex
	case AppendDataFile:
		if len(writer.metafiles.root.DataFiles) == 0 {
			return nil
		}
		// Add the metafile to backupmeta and reset metafiles.
		if writer.backupMeta.FileIndex == nil {
			writer.backupMeta.FileIndex = &backuppb.MetaFile{}
		}
		index = writer.backupMeta.FileIndex
	case AppendDDL:
		if len(writer.metafiles.root.Ddls) == 0 {
			return nil
		}
		if writer.backupMeta.DdlIndexes == nil {
			writer.backupMeta.DdlIndexes = &backuppb.MetaFile{}
		}
		index = writer.backupMeta.DdlIndexes
	}
	content, err := writer.metafiles.root.Marshal()
	if err != nil {
		return errors.Trace(err)
	}

	name := op.name()
	writer.metafileSizes[name] += writer.metafiles.size
	writer.totalDataFileSize += writer.metafiles.dataFileSize

	// Flush metafiles to external storage.
	writer.metafileSeqNum["metafiles"]++
	fname := fmt.Sprintf("backupmeta.%s.%09d", name, writer.metafileSeqNum["metafiles"])

	encyptedContent, iv, err := Encrypt(content, writer.cipher)
	if err != nil {
		return errors.Trace(err)
	}

	writer.totalMetaFileSize += uint64(len(encyptedContent))
	if err = writer.storage.WriteFile(ctx, fname, encyptedContent); err != nil {
		return errors.Trace(err)
	}
	checksum := sha256.Sum256(content)
	file := &backuppb.File{
		Name:     fname,
		Sha256:   checksum[:],
		Size_:    uint64(len(content)),
		CipherIv: iv,
	}

	index.MetaFiles = append(index.MetaFiles, file)
	writer.flushedItemNum += writer.metafiles.itemNum
	writer.metafiles = NewSizedMetaFile(writer.metafiles.sizeLimit)
	return nil
}

// ArchiveSize represents the size of ArchiveSize.
func (writer *MetaWriter) ArchiveSize() uint64 {
	total := uint64(0)
	for _, file := range writer.backupMeta.Files {
		total += file.Size_
	}
	total += uint64(writer.totalDataFileSize)
	return total
}

// MetaFilesSize represents the size of meta files from backupmeta v2,
// must be called after everything finishes by `FinishWriteMetas`.
func (writer *MetaWriter) MetaFilesSize() uint64 {
	return writer.totalMetaFileSize
}

// Backupmeta clones a backupmeta.
func (writer *MetaWriter) Backupmeta() *backuppb.BackupMeta {
	clone := proto.Clone(writer.backupMeta)
	return clone.(*backuppb.BackupMeta)
}

// NewStatsWriter wraps the new function of stats writer
func (writer *MetaWriter) NewStatsWriter() *StatsWriter {
	return newStatsWriter(writer.storage, writer.cipher)
}

func mergeDDLs(ddls [][]byte) []byte {
	b := bytes.Join(ddls, []byte(`,`))
	b = append(b, 0)
	copy(b[1:], b[0:])
	b[0] = byte('[')
	b = append(b, ']')
	return b
}
