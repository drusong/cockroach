// Copyright 2016 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package backupccl

import (
	"bytes"
	"compress/gzip"
	"context"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	descpb "github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/storage/cloud"
	"github.com/cockroachdb/cockroach/pkg/storage/cloudimpl"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/errors"
)

// Files that may appear in a backup directory.
const (
	// BackupManifestName is the file name used for serialized BackupManifest
	// protos.
	BackupManifestName = "BACKUP"
	// BackupNewManifestName is a future name for the serialized BackupManifest
	// proto.
	BackupNewManifestName = "BACKUP_MANIFEST"

	// BackupPartitionDescriptorPrefix is the file name prefix for serialized
	// BackupPartitionDescriptor protos.
	BackupPartitionDescriptorPrefix = "BACKUP_PART"
	// BackupManifestCheckpointName is the file name used to store the serialized
	// BackupManifest proto while the backup is in progress.
	BackupManifestCheckpointName = "BACKUP-CHECKPOINT"
	// BackupStatisticsFileName is the file name used to store the serialized
	// table statistics for the tables being backed up.
	BackupStatisticsFileName = "BACKUP-STATISTICS"
	// BackupSentinelWriteFile is a file that we write to the backup directory to
	// ensure that we have write privileges to the directory. Nothing should check
	// for its existence since we don't guarantee that it's cleaned up after it is
	// written (for example, we may not have DELETE permissions for the
	// destination, which should be allowed).
	BackupSentinelWriteFile = "COCKROACH-BACKUP-PLACEHOLDER"
)

const (
	// BackupFormatDescriptorTrackingVersion added tracking of complete DBs.
	BackupFormatDescriptorTrackingVersion uint32 = 1
	// ZipType is the format of a GZipped compressed file.
	ZipType = "application/x-gzip"

	dateBasedFolderName = "/20060102/150405.00"
	latestFileName      = "LATEST"
)

// BackupFileDescriptors is an alias on which to implement sort's interface.
type BackupFileDescriptors []BackupManifest_File

func (r BackupFileDescriptors) Len() int      { return len(r) }
func (r BackupFileDescriptors) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r BackupFileDescriptors) Less(i, j int) bool {
	if cmp := bytes.Compare(r[i].Span.Key, r[j].Span.Key); cmp != 0 {
		return cmp < 0
	}
	return bytes.Compare(r[i].Span.EndKey, r[j].Span.EndKey) < 0
}

// ReadBackupManifestFromURI creates an export store from the given URI, then
// reads and unmarshals a BackupManifest at the standard location in the
// export storage.
func ReadBackupManifestFromURI(
	ctx context.Context,
	uri string,
	user string,
	makeExternalStorageFromURI cloud.ExternalStorageFromURIFactory,
	encryption *jobspb.BackupEncryptionOptions,
) (BackupManifest, error) {
	exportStore, err := makeExternalStorageFromURI(ctx, uri, user)

	if err != nil {
		return BackupManifest{}, err
	}
	defer exportStore.Close()
	return readBackupManifestFromStore(ctx, exportStore, encryption)
}

func readBackupManifestFromStore(
	ctx context.Context,
	exportStore cloud.ExternalStorage,
	encryption *jobspb.BackupEncryptionOptions,
) (BackupManifest, error) {

	backupManifest, err := readBackupManifest(ctx, exportStore, BackupManifestName,
		encryption)
	if err != nil {
		newManifest, newErr := readBackupManifest(ctx, exportStore, BackupNewManifestName,
			encryption)
		if newErr != nil {
			return BackupManifest{}, err
		}
		backupManifest = newManifest
	}
	backupManifest.Dir = exportStore.Conf()
	// TODO(dan): Sanity check this BackupManifest: non-empty EndTime,
	// non-empty Paths, and non-overlapping Spans and keyranges in Files.
	return backupManifest, nil
}

func containsManifest(ctx context.Context, exportStore cloud.ExternalStorage) (bool, error) {
	r, err := exportStore.ReadFile(ctx, BackupManifestName)
	if err != nil {
		if errors.Is(err, cloudimpl.ErrFileDoesNotExist) {
			return false, nil
		}
		return false, err
	}
	r.Close()
	return true, nil
}

// compressData compresses data buffer and returns compressed
// bytes (i.e. gzip format).
func compressData(descBuf []byte) ([]byte, error) {
	gzipBuf := bytes.NewBuffer([]byte{})
	gz := gzip.NewWriter(gzipBuf)
	if _, err := gz.Write(descBuf); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return gzipBuf.Bytes(), nil
}

// DecompressData decompresses gzip data buffer and
// returns decompressed bytes.
func DecompressData(descBytes []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewBuffer(descBytes))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return ioutil.ReadAll(r)
}

// readBackupManifest reads and unmarshals a BackupManifest from filename in
// the provided export store.
func readBackupManifest(
	ctx context.Context,
	exportStore cloud.ExternalStorage,
	filename string,
	encryption *jobspb.BackupEncryptionOptions,
) (BackupManifest, error) {
	r, err := exportStore.ReadFile(ctx, filename)
	if err != nil {
		return BackupManifest{}, err
	}
	defer r.Close()
	descBytes, err := ioutil.ReadAll(r)
	if err != nil {
		return BackupManifest{}, err
	}
	if encryption != nil {
		encryptionKey, err := getEncryptionKey(ctx, encryption, exportStore.Settings(),
			exportStore.ExternalIOConf())
		if err != nil {
			return BackupManifest{}, err
		}
		descBytes, err = storageccl.DecryptFile(descBytes, encryptionKey)
		if err != nil {
			return BackupManifest{}, err
		}
	}

	fileType := http.DetectContentType(descBytes)
	if fileType == ZipType {
		descBytes, err = DecompressData(descBytes)
		if err != nil {
			return BackupManifest{}, errors.Wrap(
				err, "decompressing backup manifest")
		}
	}
	var backupManifest BackupManifest
	if err := protoutil.Unmarshal(descBytes, &backupManifest); err != nil {
		if encryption == nil && storageccl.AppearsEncrypted(descBytes) {
			return BackupManifest{}, errors.Wrapf(
				err, "file appears encrypted -- try specifying one of \"%s\" or \"%s\"",
				backupOptEncPassphrase, backupOptEncKMS)
		}
		return BackupManifest{}, err
	}
	for _, d := range backupManifest.Descriptors {
		// Calls to GetTable are generally frowned upon.
		// This specific call exists to provide backwards compatibility with
		// backups created prior to version 19.1. Starting in v19.1 the
		// ModificationTime is always written in backups for all versions
		// of table descriptors. In earlier cockroach versions only later
		// table descriptor versions contain a non-empty ModificationTime.
		// Later versions of CockroachDB use the MVCC timestamp to fill in
		// the ModificationTime for table descriptors. When performing a restore
		// we no longer have access to that MVCC timestamp but we can set it
		// to a value we know will be safe.
		if t := d.GetTable(); t == nil {
			continue
		} else if t.Version == 1 && t.ModificationTime.IsEmpty() {
			t.ModificationTime = hlc.Timestamp{WallTime: 1}
		}
	}
	return backupManifest, err
}

func readBackupPartitionDescriptor(
	ctx context.Context,
	exportStore cloud.ExternalStorage,
	filename string,
	encryption *jobspb.BackupEncryptionOptions,
) (BackupPartitionDescriptor, error) {
	r, err := exportStore.ReadFile(ctx, filename)
	if err != nil {
		return BackupPartitionDescriptor{}, err
	}
	defer r.Close()
	descBytes, err := ioutil.ReadAll(r)
	if err != nil {
		return BackupPartitionDescriptor{}, err
	}
	if encryption != nil {
		encryptionKey, err := getEncryptionKey(ctx, encryption, exportStore.Settings(),
			exportStore.ExternalIOConf())
		if err != nil {
			return BackupPartitionDescriptor{}, err
		}
		descBytes, err = storageccl.DecryptFile(descBytes, encryptionKey)
		if err != nil {
			return BackupPartitionDescriptor{}, err
		}
	}

	fileType := http.DetectContentType(descBytes)
	if fileType == ZipType {
		descBytes, err = DecompressData(descBytes)
		if err != nil {
			return BackupPartitionDescriptor{}, errors.Wrap(
				err, "decompressing backup partition descriptor")
		}
	}
	var backupManifest BackupPartitionDescriptor
	if err := protoutil.Unmarshal(descBytes, &backupManifest); err != nil {
		return BackupPartitionDescriptor{}, err
	}
	return backupManifest, err
}

// readTableStatistics reads and unmarshals a StatsTable from filename in
// the provided export store, and returns its pointer.
func readTableStatistics(
	ctx context.Context,
	exportStore cloud.ExternalStorage,
	filename string,
	encryption *jobspb.BackupEncryptionOptions,
) (*StatsTable, error) {
	r, err := exportStore.ReadFile(ctx, filename)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	statsBytes, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if encryption != nil {
		encryptionKey, err := getEncryptionKey(ctx, encryption, exportStore.Settings(),
			exportStore.ExternalIOConf())
		if err != nil {
			return nil, err
		}
		statsBytes, err = storageccl.DecryptFile(statsBytes, encryptionKey)
		if err != nil {
			return nil, err
		}
	}
	var tableStats StatsTable
	if err := protoutil.Unmarshal(statsBytes, &tableStats); err != nil {
		return nil, err
	}
	return &tableStats, err
}

func writeBackupManifest(
	ctx context.Context,
	settings *cluster.Settings,
	exportStore cloud.ExternalStorage,
	filename string,
	encryption *jobspb.BackupEncryptionOptions,
	desc *BackupManifest,
) error {
	sort.Sort(BackupFileDescriptors(desc.Files))

	descBuf, err := protoutil.Marshal(desc)
	if err != nil {
		return err
	}
	descBuf, err = compressData(descBuf)
	if err != nil {
		return errors.Wrap(err, "compressing backup manifest")
	}

	if encryption != nil {
		encryptionKey, err := getEncryptionKey(ctx, encryption, settings, exportStore.ExternalIOConf())
		if err != nil {
			return err
		}
		descBuf, err = storageccl.EncryptFile(descBuf, encryptionKey)
		if err != nil {
			return err
		}
	}

	return exportStore.WriteFile(ctx, filename, bytes.NewReader(descBuf))
}

func getEncryptionKey(
	ctx context.Context,
	encryption *jobspb.BackupEncryptionOptions,
	settings *cluster.Settings,
	ioConf base.ExternalIODirConfig,
) ([]byte, error) {
	if encryption == nil {
		return nil, errors.New("FileEncryptionOptions is nil when retrieving encryption key")
	}
	switch encryption.Mode {
	case jobspb.EncryptionMode_Passphrase:
		return encryption.Key, nil
	case jobspb.EncryptionMode_KMS:
		// Contact the selected KMS to derive the decrypted data key.
		kms, err := cloud.KMSFromURI(encryption.KMSInfo.Uri, &backupKMSEnv{
			settings: settings,
			conf:     &ioConf,
		})
		if err != nil {
			return nil, err
		}

		defer func() {
			_ = kms.Close()
		}()

		plaintextDataKey, err := kms.Decrypt(ctx, encryption.KMSInfo.EncryptedDataKey)
		if err != nil {
			return nil, errors.Wrap(err, "failed to decrypt data key")
		}

		return plaintextDataKey, nil
	}

	return nil, errors.New("invalid encryption mode")
}

// writeBackupPartitionDescriptor writes metadata (containing a locality KV and
// partial file listing) for a partitioned BACKUP to one of the stores in the
// backup.
func writeBackupPartitionDescriptor(
	ctx context.Context,
	exportStore cloud.ExternalStorage,
	filename string,
	encryption *jobspb.BackupEncryptionOptions,
	desc *BackupPartitionDescriptor,
) error {
	descBuf, err := protoutil.Marshal(desc)
	if err != nil {
		return err
	}
	descBuf, err = compressData(descBuf)
	if err != nil {
		return errors.Wrap(err, "compressing backup partition descriptor")
	}
	if encryption != nil {
		encryptionKey, err := getEncryptionKey(ctx, encryption, exportStore.Settings(),
			exportStore.ExternalIOConf())
		if err != nil {
			return err
		}
		descBuf, err = storageccl.EncryptFile(descBuf, encryptionKey)
		if err != nil {
			return err
		}
	}

	return exportStore.WriteFile(ctx, filename, bytes.NewReader(descBuf))
}

// writeTableStatistics writes a StatsTable object to a file of the filename
// to the specified exportStore. It will be encrypted according to the encryption
// option given.
func writeTableStatistics(
	ctx context.Context,
	exportStore cloud.ExternalStorage,
	filename string,
	encryption *jobspb.BackupEncryptionOptions,
	stats *StatsTable,
) error {
	statsBuf, err := protoutil.Marshal(stats)
	if err != nil {
		return err
	}
	if encryption != nil {
		encryptionKey, err := getEncryptionKey(ctx, encryption, exportStore.Settings(),
			exportStore.ExternalIOConf())
		if err != nil {
			return err
		}
		statsBuf, err = storageccl.EncryptFile(statsBuf, encryptionKey)
		if err != nil {
			return err
		}
	}
	return exportStore.WriteFile(ctx, filename, bytes.NewReader(statsBuf))
}

func loadBackupManifests(
	ctx context.Context,
	uris []string,
	user string,
	makeExternalStorageFromURI cloud.ExternalStorageFromURIFactory,
	encryption *jobspb.BackupEncryptionOptions,
) ([]BackupManifest, error) {
	backupManifests := make([]BackupManifest, len(uris))

	for i, uri := range uris {
		desc, err := ReadBackupManifestFromURI(ctx, uri, user, makeExternalStorageFromURI,
			encryption)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to read backup descriptor")
		}
		backupManifests[i] = desc
	}
	if len(backupManifests) == 0 {
		return nil, errors.Newf("no backups found")
	}
	return backupManifests, nil
}

// getLocalityInfo takes a list of stores and their URIs, along with the main
// backup manifest searches each for the locality pieces listed in the the
// main manifest, returning the mapping.
func getLocalityInfo(
	ctx context.Context,
	stores []cloud.ExternalStorage,
	uris []string,
	mainBackupManifest BackupManifest,
	encryption *jobspb.BackupEncryptionOptions,
	prefix string,
) (jobspb.RestoreDetails_BackupLocalityInfo, error) {
	var info jobspb.RestoreDetails_BackupLocalityInfo
	// Now get the list of expected partial per-store backup manifest filenames
	// and attempt to find them.
	urisByOrigLocality := make(map[string]string)
	for _, filename := range mainBackupManifest.PartitionDescriptorFilenames {
		if prefix != "" {
			filename = path.Join(prefix, filename)
		}
		found := false
		for i, store := range stores {
			if desc, err := readBackupPartitionDescriptor(ctx, store, filename, encryption); err == nil {
				if desc.BackupID != mainBackupManifest.ID {
					return info, errors.Errorf(
						"expected backup part to have backup ID %s, found %s",
						mainBackupManifest.ID, desc.BackupID,
					)
				}
				origLocalityKV := desc.LocalityKV
				kv := roachpb.Tier{}
				if err := kv.FromString(origLocalityKV); err != nil {
					return info, errors.Wrapf(err, "reading backup manifest from %s", uris[i])
				}
				if _, ok := urisByOrigLocality[origLocalityKV]; ok {
					return info, errors.Errorf("duplicate locality %s found in backup", origLocalityKV)
				}
				urisByOrigLocality[origLocalityKV] = uris[i]
				found = true
				break
			}
		}
		if !found {
			return info, errors.Errorf("expected manifest %s not found in backup locations", filename)
		}
	}
	info.URIsByOriginalLocalityKV = urisByOrigLocality
	return info, nil
}

// findPriorBackupNames finds "appended" incremental backups, as done by
// findPriorBackupLocations and appends the backup manifest file name to
// the URI.
func findPriorBackupNames(ctx context.Context, store cloud.ExternalStorage) ([]string, error) {
	prev, err := store.ListFiles(ctx, "[0-9]*/[0-9]*.[0-9][0-9]/"+BackupManifestName)
	if err != nil {
		return nil, errors.Wrap(err, "reading previous backup layers")
	}
	sort.Strings(prev)
	return prev, nil
}

// findPriorBackupLocations finds "appended" incremental backups by searching
// for the subdirectories matching the naming pattern (e.g. YYMMDD/HHmmss.ss).
// Using file-system searching rather than keeping an explicit list allows
// layers to be manually moved/removed/etc without needing to update/maintain
// said list.
func findPriorBackupLocations(ctx context.Context, store cloud.ExternalStorage) ([]string, error) {
	prev, err := store.ListFiles(ctx, "[0-9]*/[0-9]*.[0-9][0-9]/")
	if err != nil {
		return nil, errors.Wrap(err, "reading previous backup layers")
	}
	sort.Strings(prev)
	return prev, nil
}

// resolveBackupManifests resolves a list of list of URIs that point to the
// incremental layers (each of which can be partitioned) of backups into the
// actual backup manifests and metadata required to RESTORE. If only one layer
// is explicitly provided, it is inspected to see if it contains "appended"
// layers internally that are then expanded into the result layers returned,
// similar to if those layers had been specified in `from` explicitly.
func resolveBackupManifests(
	ctx context.Context,
	baseStores []cloud.ExternalStorage,
	mkStore cloud.ExternalStorageFromURIFactory,
	from [][]string,
	endTime hlc.Timestamp,
	encryption *jobspb.BackupEncryptionOptions,
	user string,
) (
	defaultURIs []string,
	mainBackupManifests []BackupManifest,
	localityInfo []jobspb.RestoreDetails_BackupLocalityInfo,
	_ error,
) {
	baseManifest, err := readBackupManifestFromStore(ctx, baseStores[0], encryption)
	if err != nil {
		return nil, nil, nil, err
	}

	// If explicit incremental backups were are passed, we simply load them one
	// by one as specified and return the results.
	if len(from) > 1 {
		defaultURIs = make([]string, len(from))
		localityInfo = make([]jobspb.RestoreDetails_BackupLocalityInfo, len(from))
		mainBackupManifests = make([]BackupManifest, len(from))

		for i, uris := range from {
			// The first URI in the list must contain the main BACKUP manifest.
			defaultURIs[i] = uris[0]

			stores := make([]cloud.ExternalStorage, len(uris))
			for j := range uris {
				stores[j], err = mkStore(ctx, uris[j], user)
				if err != nil {
					return nil, nil, nil, errors.Wrapf(err, "export configuration")
				}
				defer stores[j].Close()
			}

			mainBackupManifests[i], err = readBackupManifestFromStore(ctx, stores[0], encryption)
			if err != nil {
				return nil, nil, nil, err
			}
			if len(uris) > 1 {
				localityInfo[i], err = getLocalityInfo(
					ctx, stores, uris, mainBackupManifests[i], encryption, "", /* prefix */
				)
				if err != nil {
					return nil, nil, nil, err
				}
			}
		}
		if err != nil {
			return nil, nil, nil, err
		}
	} else {
		// Since incremental layers were *not* explicitly specified, search for any
		// automatically created incremental layers inside the base layer.
		prev, err := findPriorBackupNames(ctx, baseStores[0])
		if err != nil {
			if errors.Is(err, cloudimpl.ErrListingUnsupported) {
				log.Warningf(ctx, "storage sink %T does not support listing, only resolving the base backup", baseStores[0])
				// If we do not support listing, we have to just assume there are none
				// and restore the specified base.
				prev = nil
			} else {
				return nil, nil, nil, err
			}
		}

		numLayers := len(prev) + 1

		defaultURIs = make([]string, numLayers)
		mainBackupManifests = make([]BackupManifest, numLayers)
		localityInfo = make([]jobspb.RestoreDetails_BackupLocalityInfo, numLayers)

		// Setup the base layer explicitly.
		defaultURIs[0] = from[0][0]
		mainBackupManifests[0] = baseManifest
		localityInfo[0], err = getLocalityInfo(
			ctx, baseStores, from[0], baseManifest, encryption, "", /* prefix */
		)
		if err != nil {
			return nil, nil, nil, err
		}

		// If we discovered additional layers, handle them too.
		if numLayers > 1 {
			numPartitions := len(from[0])
			// We need the parsed baseURI for each partition to calculate the URI to
			// each layer in that partition below.
			baseURIs := make([]*url.URL, numPartitions)
			for i := range from[0] {
				baseURIs[i], err = url.Parse(from[0][i])
				if err != nil {
					return nil, nil, nil, err
				}
			}

			// For each layer, we need to load the base manifest then calculate the URI and the
			// locality info for each partition.
			for i := range prev {
				defaultManifestForLayer, err := readBackupManifest(ctx, baseStores[0], prev[i], encryption)
				if err != nil {
					return nil, nil, nil, err
				}
				mainBackupManifests[i+1] = defaultManifestForLayer

				// prev[i] is the path to the manifest file itself for layer i -- the
				// dirname piece of that path is the subdirectory in each of the
				// partitions in which we'll also expect to find a partition manifest.
				subDir := path.Dir(prev[i])
				partitionURIs := make([]string, numPartitions)
				for j := range baseURIs {
					u := *baseURIs[j] // NB: makes a copy to avoid mutating the baseURI.
					u.Path = path.Join(u.Path, subDir)
					partitionURIs[j] = u.String()
				}
				defaultURIs[i+1] = partitionURIs[0]
				localityInfo[i+1], err = getLocalityInfo(ctx, baseStores, partitionURIs, defaultManifestForLayer, encryption, subDir)
				if err != nil {
					return nil, nil, nil, err
				}
			}
		}
	}

	// Check that the requested target time, if specified, is valid for the list
	// of incremental backups resolved, truncating the results to the backup that
	// contains the target time.
	if !endTime.IsEmpty() {
		ok := false
		for i, b := range mainBackupManifests {
			// Find the backup that covers the requested time.
			if b.StartTime.Less(endTime) && endTime.LessEq(b.EndTime) {
				ok = true

				mainBackupManifests = mainBackupManifests[:i+1]
				defaultURIs = defaultURIs[:i+1]
				localityInfo = localityInfo[:i+1]

				// Ensure that the backup actually has revision history.
				if !endTime.Equal(b.EndTime) {
					if b.MVCCFilter != MVCCFilter_All {
						const errPrefix = "invalid RESTORE timestamp: restoring to arbitrary time requires that BACKUP for requested time be created with '%s' option."
						if i == 0 {
							return nil, nil, nil, errors.Errorf(
								errPrefix+" nearest backup time is %s", backupOptRevisionHistory, b.EndTime,
							)
						}
						return nil, nil, nil, errors.Errorf(
							errPrefix+" nearest BACKUP times are %s or %s",
							backupOptRevisionHistory, mainBackupManifests[i-1].EndTime, b.EndTime,
						)
					}
					// Ensure that the revision history actually covers the requested time -
					// while the BACKUP's start and end might contain the requested time for
					// example if start time is 0 (full backup), the revision history was
					// only captured since the GC window. Note that the RevisionStartTime is
					// the latest for ranges backed up.
					if endTime.LessEq(b.RevisionStartTime) {
						return nil, nil, nil, errors.Errorf(
							"invalid RESTORE timestamp: BACKUP for requested time only has revision history from %v", b.RevisionStartTime,
						)
					}
				}
				break
			}
		}

		if !ok {
			return nil, nil, nil, errors.Errorf(
				"invalid RESTORE timestamp: supplied backups do not cover requested time",
			)
		}
	}

	return defaultURIs, mainBackupManifests, localityInfo, nil
}

// TODO(anzoteh96): benchmark the performance of different search algorithms,
// e.g.  linear search, binary search, reverse linear search.
func getBackupIndexAtTime(backupManifests []BackupManifest, asOf hlc.Timestamp) (int, error) {
	if len(backupManifests) == 0 {
		return -1, errors.New("expected a nonempty backup manifest list, got an empty list")
	}
	backupManifestIndex := len(backupManifests) - 1
	if asOf.IsEmpty() {
		return backupManifestIndex, nil
	}
	for ind, b := range backupManifests {
		if asOf.Less(b.StartTime) {
			break
		}
		backupManifestIndex = ind
	}
	return backupManifestIndex, nil
}

// unwrapDescriptor takes a descriptor retrieved from a backup manifest and
// constructs the appropriate MutableDescriptor object implied by that object.
// It assumes and will panic if the ModificationTime for the descriptors are
// not set.
//
// TODO(ajwerner): This may prove problematic for backups of database
// descriptors without modification time.
func unwrapDescriptor(ctx context.Context, desc *descpb.Descriptor) catalog.MutableDescriptor {
	sqlbase.MaybeSetDescriptorModificationTimeFromMVCCTimestamp(ctx, desc, hlc.Timestamp{})
	table, database, typ, schema := sqlbase.TableFromDescriptor(desc, hlc.Timestamp{}),
		desc.GetDatabase(), desc.GetType(), desc.GetSchema()
	switch {
	case table != nil:
		return sqlbase.NewMutableExistingTableDescriptor(*table)
	case database != nil:
		return sqlbase.NewMutableExistingDatabaseDescriptor(*database)
	case typ != nil:
		return sqlbase.NewMutableExistingTypeDescriptor(*typ)
	case schema != nil:
		return sqlbase.NewMutableExistingSchemaDescriptor(*schema)
	default:
		log.Fatalf(ctx, "failed to unwrap descriptor of type %T", desc.Union)
		return nil // unreachable
	}
}

func loadSQLDescsFromBackupsAtTime(
	backupManifests []BackupManifest, asOf hlc.Timestamp,
) ([]catalog.Descriptor, BackupManifest) {
	lastBackupManifest := backupManifests[len(backupManifests)-1]

	unwrapDescriptors := func(raw []descpb.Descriptor) []catalog.Descriptor {
		ret := make([]catalog.Descriptor, 0, len(raw))
		for i := range raw {
			ret = append(ret, unwrapDescriptor(context.TODO(), &raw[i]))
		}
		return ret
	}
	if asOf.IsEmpty() {
		return unwrapDescriptors(lastBackupManifest.Descriptors), lastBackupManifest
	}

	for _, b := range backupManifests {
		if asOf.Less(b.StartTime) {
			break
		}
		lastBackupManifest = b
	}
	if len(lastBackupManifest.DescriptorChanges) == 0 {
		return unwrapDescriptors(lastBackupManifest.Descriptors), lastBackupManifest
	}

	byID := make(map[descpb.ID]*descpb.Descriptor, len(lastBackupManifest.Descriptors))
	for _, rev := range lastBackupManifest.DescriptorChanges {
		if asOf.Less(rev.Time) {
			break
		}
		if rev.Desc == nil {
			delete(byID, rev.ID)
		} else {
			byID[rev.ID] = rev.Desc
		}
	}

	allDescs := make([]catalog.Descriptor, 0, len(byID))
	for _, raw := range byID {
		// A revision may have been captured before it was in a DB that is
		// backed up -- if the DB is missing, filter the object.
		desc := unwrapDescriptor(context.TODO(), raw)
		var isObject bool
		switch desc.(type) {
		case catalog.TableDescriptor, catalog.TypeDescriptor:
			isObject = true
		}
		if isObject && byID[desc.GetParentID()] == nil {
			continue
		}
		allDescs = append(allDescs, desc)
	}
	return allDescs, lastBackupManifest
}

// sanitizeLocalityKV returns a sanitized version of the input string where all
// characters that are not alphanumeric or -, =, or _ are replaced with _.
func sanitizeLocalityKV(kv string) string {
	sanitizedKV := make([]byte, len(kv))
	for i := 0; i < len(kv); i++ {
		if (kv[i] >= 'a' && kv[i] <= 'z') ||
			(kv[i] >= 'A' && kv[i] <= 'Z') ||
			(kv[i] >= '0' && kv[i] <= '9') || kv[i] == '-' || kv[i] == '=' {
			sanitizedKV[i] = kv[i]
		} else {
			sanitizedKV[i] = '_'
		}
	}
	return string(sanitizedKV)
}

func readEncryptionOptions(
	ctx context.Context, src cloud.ExternalStorage,
) (*EncryptionInfo, error) {
	r, err := src.ReadFile(ctx, "encryption-info")
	if err != nil {
		return nil, errors.Wrap(err, "could not find or read encryption information")
	}
	defer r.Close()
	encInfoBytes, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, errors.Wrap(err, "could not find or read encryption information")
	}
	var encInfo EncryptionInfo
	if err := protoutil.Unmarshal(encInfoBytes, &encInfo); err != nil {
		return nil, err
	}
	return &encInfo, nil
}

func writeEncryptionOptions(
	ctx context.Context, opts *EncryptionInfo, dest cloud.ExternalStorage,
) error {
	buf, err := protoutil.Marshal(opts)
	if err != nil {
		return err
	}
	if err := dest.WriteFile(ctx, "encryption-info", bytes.NewReader(buf)); err != nil {
		return err
	}
	return nil
}

// createCheckpointIfNotExists creates a checkpoint file if it does not exist.
// This is used to lock out other BACKUPs (which check for this file during
// planning) from starting a backup to this location.
func createCheckpointIfNotExists(
	ctx context.Context,
	settings *cluster.Settings,
	exportStore cloud.ExternalStorage,
	encryption *jobspb.BackupEncryptionOptions,
) error {
	r, err := exportStore.ReadFile(ctx, BackupManifestCheckpointName)
	if err == nil {
		r.Close()
		// If the file already exists, then we don't need to create a new one.
		return nil
	}

	if !errors.Is(err, cloudimpl.ErrFileDoesNotExist) {
		return errors.Wrapf(err,
			"returned an unexpected error when checking for the existence of %s file",
			BackupManifestCheckpointName)
	}

	// If there is not checkpoint manifest yet, write one to lock out other
	// backups from starting to write to this destination.
	if err := writeBackupManifest(
		ctx, settings, exportStore, BackupManifestCheckpointName, encryption, &BackupManifest{},
	); err != nil {
		return errors.Wrapf(err, "writing checkpoint file %s", BackupManifestCheckpointName)
	}

	return nil
}

// checkForPreviousBackup ensures that the target location does not already
// contain a BACKUP or checkpoint, locking out accidental concurrent operations
// on that location. Note that the checkpoint file should be written as soon as
// the job actually starts.
func checkForPreviousBackup(
	ctx context.Context, exportStore cloud.ExternalStorage, readable string,
) error {
	r, err := exportStore.ReadFile(ctx, BackupManifestName)
	if err == nil {
		r.Close()
		return pgerror.Newf(pgcode.FileAlreadyExists,
			"%s already contains a %s file",
			readable, BackupManifestName)
	}

	if !errors.Is(err, cloudimpl.ErrFileDoesNotExist) {
		return errors.Wrapf(err,
			"%s returned an unexpected error when checking for the existence of %s file",
			readable, BackupManifestName)
	}

	r, err = exportStore.ReadFile(ctx, BackupManifestCheckpointName)
	if err == nil {
		r.Close()
		return pgerror.Newf(pgcode.FileAlreadyExists,
			"%s already contains a %s file (is another operation already in progress?)",
			readable, BackupManifestCheckpointName)
	}

	if !errors.Is(err, cloudimpl.ErrFileDoesNotExist) {
		return errors.Wrapf(err,
			"%s returned an unexpected error when checking for the existence of %s file",
			readable, BackupManifestCheckpointName)
	}

	return nil
}

// verifyWritableDestination writes a test file, verifying that the location is
// writable. This method will do a best-effort clean up of the temporary file.
// We don't require DELETE permissions on their backup directory, so we do not
// enforce that this file be deleted.
func verifyWriteableDestination(
	ctx context.Context,
	user string,
	makeCloudStorage cloud.ExternalStorageFromURIFactory,
	baseURI string,
) error {
	baseStore, err := makeCloudStorage(ctx, baseURI, user)
	if err != nil {
		return err
	}
	defer baseStore.Close()

	// Write arbitrary bytes to a sentinel file in the backup directory to ensure
	// that we're able to write to this directory.
	arbitraryBytes := bytes.NewReader([]byte("✇"))
	if err := baseStore.WriteFile(ctx, BackupSentinelWriteFile, arbitraryBytes); err != nil {
		return errors.Wrapf(err, "writing sentinel file to %s", baseURI)
	}

	if err := baseStore.Delete(ctx, BackupSentinelWriteFile); err != nil {
		// Don't require that we're able to clean up the sentinel file. Nothing
		// should check for it's existence so it should be fine to leave it around.
		// Let's still log if we can't clean up.
		log.Warningf(ctx,
			"could not clean up sentinel backup %s file in %s: %+v",
			BackupSentinelWriteFile, baseURI, err)
	}

	return nil
}
