/*
 * MinIO Cloud Storage, (C) 2016-2020 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	xhttp "github.com/minio/minio/cmd/http"
	"github.com/minio/minio/cmd/logger"
	"github.com/minio/minio/pkg/mimedb"
	"github.com/minio/minio/pkg/sync/errgroup"
)

func (er erasureObjects) getUploadIDDir(bucket, object, uploadID string) string {
	return pathJoin(er.getMultipartSHADir(bucket, object), uploadID)
}

// getUploadIDLockPath returns the name of the Lock in the form of
// bucket/object/uploadID. For locking, the path bucket/object/uploadID
// is locked instead of multipart-sha256-Dir/uploadID as it is more
// readable in the list-locks output which helps in debugging.
func (er erasureObjects) getUploadIDLockPath(bucket, object, uploadID string) string {
	return pathJoin(bucket, object, uploadID)
}

func (er erasureObjects) getMultipartSHADir(bucket, object string) string {
	return getSHA256Hash([]byte(pathJoin(bucket, object)))
}

// checkUploadIDExists - verify if a given uploadID exists and is valid.
func (er erasureObjects) checkUploadIDExists(ctx context.Context, bucket, object, uploadID string) error {
	_, err := er.getObjectInfo(ctx, minioMetaMultipartBucket, er.getUploadIDDir(bucket, object, uploadID))
	return err
}

// Removes part given by partName belonging to a mulitpart upload from minioMetaBucket
func (er erasureObjects) removeObjectPart(bucket, object, uploadID string, partNumber int) {
	curpartPath := pathJoin(bucket, object, uploadID, fmt.Sprintf("part.%d", partNumber))
	storageDisks := er.getDisks()

	g := errgroup.WithNErrs(len(storageDisks))
	for index, disk := range storageDisks {
		if disk == nil {
			continue
		}
		index := index
		g.Go(func() error {
			// Ignoring failure to remove parts that weren't present in CompleteMultipartUpload
			// requests. er.json is the authoritative source of truth on which parts constitute
			// the object. The presence of parts that don't belong in the object doesn't affect correctness.
			_ = storageDisks[index].DeleteFile(minioMetaMultipartBucket, curpartPath)
			return nil
		}, index)
	}
	g.Wait()
}

// commitAllFileInfo - commit `er.json` from source prefix to destination prefix in the given slice of disks.
func commitAllFileInfo(ctx context.Context, disks []StorageAPI, srcBucket, srcPrefix, dstBucket, dstPrefix string, quorum int) ([]StorageAPI, error) {

	g := errgroup.WithNErrs(len(disks))

	// Rename `er.json` to all disks in parallel.
	for index := range disks {
		index := index
		g.Go(func() error {
			if disks[index] == nil {
				return errDiskNotFound
			}

			// Delete any dangling directories.
			defer disks[index].DeleteFile(srcBucket, srcPrefix)

			// Renames `er.json` from source prefix to destination prefix.
			return disks[index].RenameMetadata(srcBucket, srcPrefix, dstBucket, dstPrefix)
		}, index)
	}

	// Wait for all the routines.
	mErrs := g.Wait()

	err := reduceWriteQuorumErrs(ctx, mErrs, objectOpIgnoredErrs, quorum)
	return evalDisks(disks, mErrs), err
}

// ListMultipartUploads - lists all the pending multipart
// uploads for a particular object in a bucket.
//
// Implements minimal S3 compatible ListMultipartUploads API. We do
// not support prefix based listing, this is a deliberate attempt
// towards simplification of multipart APIs.
// The resulting ListMultipartsInfo structure is unmarshalled directly as XML.
func (er erasureObjects) ListMultipartUploads(ctx context.Context, bucket, object, keyMarker, uploadIDMarker, delimiter string, maxUploads int) (result ListMultipartsInfo, e error) {
	if err := checkListMultipartArgs(ctx, bucket, object, keyMarker, uploadIDMarker, delimiter, er); err != nil {
		return result, err
	}

	result.MaxUploads = maxUploads
	result.KeyMarker = keyMarker
	result.Prefix = object
	result.Delimiter = delimiter

	for _, disk := range er.getLoadBalancedDisks() {
		if disk == nil {
			continue
		}
		uploadIDs, err := disk.ListDir(minioMetaMultipartBucket, er.getMultipartSHADir(bucket, object), -1)
		if err != nil {
			if err == errFileNotFound {
				return result, nil
			}
			logger.LogIf(ctx, err)
			return result, err
		}
		for i := range uploadIDs {
			uploadIDs[i] = strings.TrimSuffix(uploadIDs[i], SlashSeparator)
		}
		sort.Strings(uploadIDs)
		for _, uploadID := range uploadIDs {
			if len(result.Uploads) == maxUploads {
				break
			}
			result.Uploads = append(result.Uploads, MultipartInfo{Object: object, UploadID: uploadID})
		}
		break
	}

	return result, nil
}

// newMultipartUpload - wrapper for initializing a new multipart
// request; returns a unique upload id.
//
// Internally this function creates 'uploads.json' associated for the
// incoming object at
// '.minio.sys/multipart/bucket/object/uploads.json' on all the
// disks. `uploads.json` carries metadata regarding on-going multipart
// operation(s) on the object.
func (er erasureObjects) newMultipartUpload(ctx context.Context, bucket string, object string, meta map[string]string) (string, error) {

	onlineDisks := er.getDisks()
	parityBlocks := globalStorageClass.GetParityForSC(meta[xhttp.AmzStorageClass])
	if parityBlocks == 0 {
		parityBlocks = len(onlineDisks) / 2
	}
	dataBlocks := len(onlineDisks) - parityBlocks

	fi := newFileInfo(object, dataBlocks, parityBlocks)

	// we now know the number of blocks this object needs for data and parity.
	// establish the writeQuorum using this data
	writeQuorum := dataBlocks + 1

	if meta["content-type"] == "" {
		contentType := mimedb.TypeByExtension(path.Ext(object))
		meta["content-type"] = contentType
	}
	fi.ModTime = UTCNow()
	fi.Metadata = meta

	uploadID := mustGetUUID()
	uploadIDPath := er.getUploadIDDir(bucket, object, uploadID)
	tempUploadIDPath := uploadID

	// Delete the tmp path later in case we fail to commit (ignore
	// returned errors) - this will be a no-op in case of a commit
	// success.
	defer er.deleteObject(ctx, minioMetaTmpBucket, tempUploadIDPath, writeQuorum, false)

	var partsMetadata = make([]FileInfo, len(onlineDisks))
	for i := range onlineDisks {
		partsMetadata[i] = fi
	}

	var err error
	// Write updated `er.json` to all disks.
	onlineDisks, err = writeUniqueFileInfo(ctx, onlineDisks, minioMetaTmpBucket, tempUploadIDPath, partsMetadata, writeQuorum)
	if err != nil {
		return "", toObjectErr(err, minioMetaTmpBucket, tempUploadIDPath)
	}

	// Attempt to rename temp upload object to actual upload path object
	_, err = rename(ctx, onlineDisks, minioMetaTmpBucket, tempUploadIDPath, minioMetaMultipartBucket, uploadIDPath, true, writeQuorum, nil)
	if err != nil {
		return "", toObjectErr(err, minioMetaMultipartBucket, uploadIDPath)
	}

	// Return success.
	return uploadID, nil
}

// NewMultipartUpload - initialize a new multipart upload, returns a
// unique id. The unique id returned here is of UUID form, for each
// subsequent request each UUID is unique.
//
// Implements S3 compatible initiate multipart API.
func (er erasureObjects) NewMultipartUpload(ctx context.Context, bucket, object string, opts ObjectOptions) (string, error) {
	if err := checkNewMultipartArgs(ctx, bucket, object, er); err != nil {
		return "", err
	}
	// No metadata is set, allocate a new one.
	if opts.UserDefined == nil {
		opts.UserDefined = make(map[string]string)
	}
	return er.newMultipartUpload(ctx, bucket, object, opts.UserDefined)
}

// CopyObjectPart - reads incoming stream and internally erasure codes
// them. This call is similar to put object part operation but the source
// data is read from an existing object.
//
// Implements S3 compatible Upload Part Copy API.
func (er erasureObjects) CopyObjectPart(ctx context.Context, srcBucket, srcObject, dstBucket, dstObject, uploadID string, partID int, startOffset int64, length int64, srcInfo ObjectInfo, srcOpts, dstOpts ObjectOptions) (pi PartInfo, e error) {

	if err := checkNewMultipartArgs(ctx, srcBucket, srcObject, er); err != nil {
		return pi, err
	}

	partInfo, err := er.PutObjectPart(ctx, dstBucket, dstObject, uploadID, partID, NewPutObjReader(srcInfo.Reader, nil, nil), dstOpts)
	if err != nil {
		return pi, toObjectErr(err, dstBucket, dstObject)
	}

	// Success.
	return partInfo, nil
}

// PutObjectPart - reads incoming stream and internally erasure codes
// them. This call is similar to single put operation but it is part
// of the multipart transaction.
//
// Implements S3 compatible Upload Part API.
func (er erasureObjects) PutObjectPart(ctx context.Context, bucket, object, uploadID string, partID int, r *PutObjReader, opts ObjectOptions) (pi PartInfo, e error) {
	data := r.Reader
	if err := checkPutObjectPartArgs(ctx, bucket, object, er); err != nil {
		return pi, err
	}

	// Validate input data size and it can never be less than zero.
	if data.Size() < -1 {
		logger.LogIf(ctx, errInvalidArgument, logger.Application)
		return pi, toObjectErr(errInvalidArgument)
	}

	var partsMetadata []FileInfo
	var errs []error
	uploadIDPath := er.getUploadIDDir(bucket, object, uploadID)

	// Validates if upload ID exists.
	if err := er.checkUploadIDExists(ctx, bucket, object, uploadID); err != nil {
		return pi, toObjectErr(err, bucket, object, uploadID)
	}

	// Read metadata associated with the object from all disks.
	partsMetadata, errs = readAllFileInfo(er.getDisks(), minioMetaMultipartBucket,
		uploadIDPath)

	// get Quorum for this object
	_, writeQuorum, err := objectQuorumFromMeta(ctx, er, partsMetadata, errs)
	if err != nil {
		return pi, toObjectErr(err, bucket, object)
	}

	reducedErr := reduceWriteQuorumErrs(ctx, errs, objectOpIgnoredErrs, writeQuorum)
	if reducedErr == errERWriteQuorum {
		return pi, toObjectErr(reducedErr, bucket, object)
	}

	// List all online disks.
	onlineDisks, modTime := listOnlineDisks(er.getDisks(), partsMetadata, errs)

	// Pick one from the first valid metadata.
	fi, err := pickValidFileInfo(ctx, partsMetadata, modTime, writeQuorum)
	if err != nil {
		return pi, err
	}

	onlineDisks = shuffleDisks(onlineDisks, fi.Erasure.Distribution)

	// Need a unique name for the part being written in minioMetaBucket to
	// accommodate concurrent PutObjectPart requests

	partSuffix := fmt.Sprintf("part.%d", partID)
	tmpPart := mustGetUUID()
	tmpPartPath := path.Join(tmpPart, partSuffix)

	// Delete the temporary object part. If PutObjectPart succeeds there would be nothing to delete.
	defer er.deleteObject(ctx, minioMetaTmpBucket, tmpPart, writeQuorum, false)

	erasure, err := NewErasure(ctx, fi.Erasure.DataBlocks, fi.Erasure.ParityBlocks, fi.Erasure.BlockSize)
	if err != nil {
		return pi, toObjectErr(err, bucket, object)
	}

	// Fetch buffer for I/O, returns from the pool if not allocates a new one and returns.
	var buffer []byte
	switch size := data.Size(); {
	case size == 0:
		buffer = make([]byte, 1) // Allocate atleast a byte to reach EOF
	case size == -1 || size >= fi.Erasure.BlockSize:
		buffer = er.bp.Get()
		defer er.bp.Put(buffer)
	case size < fi.Erasure.BlockSize:
		// No need to allocate fully fi.Erasure.BlockSize buffer if the incoming data is smaller.
		buffer = make([]byte, size, 2*size+int64(fi.Erasure.ParityBlocks+fi.Erasure.DataBlocks-1))
	}

	if len(buffer) > int(fi.Erasure.BlockSize) {
		buffer = buffer[:fi.Erasure.BlockSize]
	}
	writers := make([]io.Writer, len(onlineDisks))
	for i, disk := range onlineDisks {
		if disk == nil {
			continue
		}
		writers[i] = newBitrotWriter(disk, minioMetaTmpBucket, tmpPartPath, erasure.ShardFileSize(data.Size()), DefaultBitrotAlgorithm, erasure.ShardSize())
	}

	n, err := erasure.Encode(ctx, data, writers, buffer, fi.Erasure.DataBlocks+1)
	closeBitrotWriters(writers)
	if err != nil {
		return pi, toObjectErr(err, bucket, object)
	}

	// Should return IncompleteBody{} error when reader has fewer bytes
	// than specified in request header.
	if n < data.Size() {
		return pi, IncompleteBody{}
	}

	for i := range writers {
		if writers[i] == nil {
			onlineDisks[i] = nil
		}
	}

	// Validates if upload ID exists.
	if err := er.checkUploadIDExists(ctx, bucket, object, uploadID); err != nil {
		return pi, toObjectErr(err, bucket, object, uploadID)
	}

	// Rename temporary part file to its final location.
	partPath := path.Join(uploadIDPath, partSuffix)
	onlineDisks, err = rename(ctx, onlineDisks, minioMetaTmpBucket, tmpPartPath, minioMetaMultipartBucket, partPath, false, writeQuorum, nil)
	if err != nil {
		return pi, toObjectErr(err, minioMetaMultipartBucket, partPath)
	}

	// Read metadata again because it might be updated with parallel upload of another part.
	partsMetadata, errs = readAllFileInfo(onlineDisks, minioMetaMultipartBucket, uploadIDPath)
	reducedErr = reduceWriteQuorumErrs(ctx, errs, objectOpIgnoredErrs, writeQuorum)
	if reducedErr == errERWriteQuorum {
		return pi, toObjectErr(reducedErr, bucket, object)
	}

	// Get current highest version based on re-read partsMetadata.
	onlineDisks, modTime = listOnlineDisks(onlineDisks, partsMetadata, errs)

	// Pick one from the first valid metadata.
	fi, err = pickValidFileInfo(ctx, partsMetadata, modTime, writeQuorum)
	if err != nil {
		return pi, err
	}

	// Once part is successfully committed, proceed with updating ER metadata.
	fi.ModTime = UTCNow()

	md5hex := r.MD5CurrentHexString()

	// Add the current part.
	fi.AddObjectPart(partID, md5hex, n, data.ActualSize())

	for i, disk := range onlineDisks {
		if disk == OfflineDisk {
			continue
		}
		partsMetadata[i].Size = fi.Size
		partsMetadata[i].ModTime = fi.ModTime
		partsMetadata[i].Parts = fi.Parts
		partsMetadata[i].Erasure.AddChecksumInfo(ChecksumInfo{
			PartNumber: partID,
			Algorithm:  DefaultBitrotAlgorithm,
			Hash:       bitrotWriterSum(writers[i]),
		})
	}

	// Write all the checksum metadata.
	tempFileInfoPath := mustGetUUID()

	// Cleanup in case of er.json writing failure
	defer er.deleteObject(ctx, minioMetaTmpBucket, tempFileInfoPath, writeQuorum, false)

	// Writes a unique `er.json` each disk carrying new checksum related information.
	onlineDisks, err = writeUniqueFileInfo(ctx, onlineDisks, minioMetaTmpBucket, tempFileInfoPath, partsMetadata, writeQuorum)
	if err != nil {
		return pi, toObjectErr(err, minioMetaTmpBucket, tempFileInfoPath)
	}

	if _, err = commitAllFileInfo(ctx, onlineDisks, minioMetaTmpBucket, tempFileInfoPath, minioMetaMultipartBucket, uploadIDPath, writeQuorum); err != nil {
		return pi, toObjectErr(err, minioMetaMultipartBucket, uploadIDPath)
	}

	// Return success.
	return PartInfo{
		PartNumber:   partID,
		ETag:         md5hex,
		LastModified: fi.ModTime,
		Size:         fi.Size,
		ActualSize:   data.ActualSize(),
	}, nil
}

// ListObjectParts - lists all previously uploaded parts for a given
// object and uploadID.  Takes additional input of part-number-marker
// to indicate where the listing should begin from.
//
// Implements S3 compatible ListObjectParts API. The resulting
// ListPartsInfo structure is marshaled directly into XML and
// replied back to the client.
func (er erasureObjects) ListObjectParts(ctx context.Context, bucket, object, uploadID string, partNumberMarker, maxParts int, opts ObjectOptions) (result ListPartsInfo, e error) {
	if err := checkListPartsArgs(ctx, bucket, object, er); err != nil {
		return result, err
	}
	if err := er.checkUploadIDExists(ctx, bucket, object, uploadID); err != nil {
		return result, toObjectErr(err, bucket, object, uploadID)
	}

	uploadIDPath := er.getUploadIDDir(bucket, object, uploadID)

	storageDisks := er.getDisks()

	// Read metadata associated with the object from all disks.
	partsMetadata, errs := readAllFileInfo(storageDisks, minioMetaMultipartBucket, uploadIDPath)

	// get Quorum for this object
	_, writeQuorum, err := objectQuorumFromMeta(ctx, er, partsMetadata, errs)
	if err != nil {
		return result, toObjectErr(err, minioMetaMultipartBucket, uploadIDPath)
	}

	reducedErr := reduceWriteQuorumErrs(ctx, errs, objectOpIgnoredErrs, writeQuorum)
	if reducedErr == errERWriteQuorum {
		return result, toObjectErr(reducedErr, minioMetaMultipartBucket, uploadIDPath)
	}

	_, modTime := listOnlineDisks(storageDisks, partsMetadata, errs)

	// Pick one from the first valid metadata.
	fi, err := pickValidFileInfo(ctx, partsMetadata, modTime, writeQuorum)
	if err != nil {
		return result, err
	}

	// Populate the result stub.
	result.Bucket = bucket
	result.Object = object
	result.UploadID = uploadID
	result.MaxParts = maxParts
	result.PartNumberMarker = partNumberMarker
	result.UserDefined = fi.Metadata

	// For empty number of parts or maxParts as zero, return right here.
	if len(fi.Parts) == 0 || maxParts == 0 {
		return result, nil
	}

	// Limit output to maxPartsList.
	if maxParts > maxPartsList {
		maxParts = maxPartsList
	}

	// Only parts with higher part numbers will be listed.
	partIdx := objectPartIndex(fi.Parts, partNumberMarker)
	parts := fi.Parts
	if partIdx != -1 {
		parts = fi.Parts[partIdx+1:]
	}
	count := maxParts
	for _, part := range parts {
		result.Parts = append(result.Parts, PartInfo{
			PartNumber:   part.Number,
			ETag:         part.ETag,
			LastModified: fi.ModTime,
			Size:         part.Size,
		})
		count--
		if count == 0 {
			break
		}
	}
	// If listed entries are more than maxParts, we set IsTruncated as true.
	if len(parts) > len(result.Parts) {
		result.IsTruncated = true
		// Make sure to fill next part number marker if IsTruncated is
		// true for subsequent listing.
		nextPartNumberMarker := result.Parts[len(result.Parts)-1].PartNumber
		result.NextPartNumberMarker = nextPartNumberMarker
	}
	return result, nil
}

// CompleteMultipartUpload - completes an ongoing multipart
// transaction after receiving all the parts indicated by the client.
// Returns an md5sum calculated by concatenating all the individual
// md5sums of all the parts.
//
// Implements S3 compatible Complete multipart API.
func (er erasureObjects) CompleteMultipartUpload(ctx context.Context, bucket string, object string, uploadID string, parts []CompletePart, opts ObjectOptions) (oi ObjectInfo, e error) {
	if err := checkCompleteMultipartArgs(ctx, bucket, object, er); err != nil {
		return oi, err
	}

	if err := er.checkUploadIDExists(ctx, bucket, object, uploadID); err != nil {
		return oi, toObjectErr(err, bucket, object, uploadID)
	}

	// Check if an object is present as one of the parent dir.
	// -- FIXME. (needs a new kind of lock).
	if er.parentDirIsObject(ctx, bucket, path.Dir(object)) {
		return oi, toObjectErr(errFileParentIsFile, bucket, object)
	}

	// Calculate s3 compatible md5sum for complete multipart.
	s3MD5 := getCompleteMultipartMD5(parts)

	uploadIDPath := er.getUploadIDDir(bucket, object, uploadID)

	storageDisks := er.getDisks()

	// Read metadata associated with the object from all disks.
	partsMetadata, errs := readAllFileInfo(storageDisks, minioMetaMultipartBucket, uploadIDPath)

	// get Quorum for this object
	_, writeQuorum, err := objectQuorumFromMeta(ctx, er, partsMetadata, errs)
	if err != nil {
		return oi, toObjectErr(err, bucket, object)
	}

	reducedErr := reduceWriteQuorumErrs(ctx, errs, objectOpIgnoredErrs, writeQuorum)
	if reducedErr == errERWriteQuorum {
		return oi, toObjectErr(reducedErr, bucket, object)
	}

	onlineDisks, modTime := listOnlineDisks(storageDisks, partsMetadata, errs)

	// Calculate full object size.
	var objectSize int64

	// Calculate consolidated actual size.
	var objectActualSize int64

	// Pick one from the first valid metadata.
	fi, err := pickValidFileInfo(ctx, partsMetadata, modTime, writeQuorum)
	if err != nil {
		return oi, err
	}

	// Order online disks in accordance with distribution order.
	onlineDisks = shuffleDisks(onlineDisks, fi.Erasure.Distribution)

	// Order parts metadata in accordance with distribution order.
	partsMetadata = shufflePartsMetadata(partsMetadata, fi.Erasure.Distribution)

	// Save current er meta for validation.
	var currentFi = fi

	// Allocate parts similar to incoming slice.
	fi.Parts = make([]ObjectPartInfo, len(parts))

	// Validate each part and then commit to disk.
	for i, part := range parts {
		partIdx := objectPartIndex(currentFi.Parts, part.PartNumber)
		// All parts should have same part number.
		if partIdx == -1 {
			invp := InvalidPart{
				PartNumber: part.PartNumber,
				GotETag:    part.ETag,
			}
			return oi, invp
		}

		// ensure that part ETag is canonicalized to strip off extraneous quotes
		part.ETag = canonicalizeETag(part.ETag)
		if currentFi.Parts[partIdx].ETag != part.ETag {
			invp := InvalidPart{
				PartNumber: part.PartNumber,
				ExpETag:    currentFi.Parts[partIdx].ETag,
				GotETag:    part.ETag,
			}
			return oi, invp
		}

		// All parts except the last part has to be atleast 5MB.
		if (i < len(parts)-1) && !isMinAllowedPartSize(currentFi.Parts[partIdx].ActualSize) {
			return oi, PartTooSmall{
				PartNumber: part.PartNumber,
				PartSize:   currentFi.Parts[partIdx].ActualSize,
				PartETag:   part.ETag,
			}
		}

		// Save for total object size.
		objectSize += currentFi.Parts[partIdx].Size

		// Save the consolidated actual size.
		objectActualSize += currentFi.Parts[partIdx].ActualSize

		// Add incoming parts.
		fi.Parts[i] = ObjectPartInfo{
			Number:     part.PartNumber,
			Size:       currentFi.Parts[partIdx].Size,
			ActualSize: currentFi.Parts[partIdx].ActualSize,
		}
	}

	// Save the final object size and modtime.
	fi.Size = objectSize
	fi.ModTime = UTCNow()

	// Save successfully calculated md5sum.
	fi.Metadata["etag"] = s3MD5

	// Save the consolidated actual size.
	fi.Metadata[ReservedMetadataPrefix+"actual-size"] = strconv.FormatInt(objectActualSize, 10)

	// Update all er metadata, make sure to not modify fields like
	// checksum which are different on each disks.
	for index := range partsMetadata {
		partsMetadata[index].Size = fi.Size
		partsMetadata[index].ModTime = fi.ModTime
		partsMetadata[index].Metadata = fi.Metadata
		partsMetadata[index].Parts = fi.Parts
	}

	tempFileInfoPath := mustGetUUID()

	// Cleanup in case of failure
	defer er.deleteObject(ctx, minioMetaTmpBucket, tempFileInfoPath, writeQuorum, false)

	// Write unique `er.json` for each disk.
	if onlineDisks, err = writeUniqueFileInfo(ctx, onlineDisks, minioMetaTmpBucket, tempFileInfoPath, partsMetadata, writeQuorum); err != nil {
		return oi, toObjectErr(err, minioMetaTmpBucket, tempFileInfoPath)
	}

	var rErr error
	onlineDisks, rErr = commitAllFileInfo(ctx, onlineDisks, minioMetaTmpBucket, tempFileInfoPath, minioMetaMultipartBucket, uploadIDPath, writeQuorum)
	if rErr != nil {
		return oi, toObjectErr(rErr, minioMetaMultipartBucket, uploadIDPath)
	}

	if er.isObject(bucket, object) {
		// Deny if WORM is enabled
		if isWORMEnabled(bucket) {
			if _, err := er.getObjectInfo(ctx, bucket, object); err == nil {
				return ObjectInfo{}, ObjectAlreadyExists{Bucket: bucket, Object: object}
			}
		}

		// Rename if an object already exists to temporary location.
		newUniqueID := mustGetUUID()

		// Delete success renamed object.
		defer er.deleteObject(ctx, minioMetaTmpBucket, newUniqueID, writeQuorum, false)

		// NOTE: Do not use online disks slice here: the reason is that existing object should be purged
		// regardless of `er.json` status and rolled back in case of errors. Also allow renaming of the
		// existing object if it is not present in quorum disks so users can overwrite stale objects.
		_, err = rename(ctx, er.getDisks(), bucket, object, minioMetaTmpBucket, newUniqueID, true, writeQuorum, []error{errFileNotFound})
		if err != nil {
			return oi, toObjectErr(err, bucket, object)
		}
	}

	// Remove parts that weren't present in CompleteMultipartUpload request.
	for _, curpart := range currentFi.Parts {
		if objectPartIndex(fi.Parts, curpart.Number) == -1 {
			// Delete the missing part files. e.g,
			// Request 1: NewMultipart
			// Request 2: PutObjectPart 1
			// Request 3: PutObjectPart 2
			// Request 4: CompleteMultipartUpload --part 2
			// N.B. 1st part is not present. This part should be removed from the storage.
			er.removeObjectPart(bucket, object, uploadID, curpart.Number)
		}
	}

	// Rename the multipart object to final location.
	if onlineDisks, err = rename(ctx, onlineDisks, minioMetaMultipartBucket, uploadIDPath, bucket, object, true, writeQuorum, nil); err != nil {
		return oi, toObjectErr(err, bucket, object)
	}

	// Check if there is any offline disk and add it to the MRF list
	for i := 0; i < len(onlineDisks); i++ {
		if onlineDisks[i] == nil || storageDisks[i] == nil {
			er.addPartialUpload(bucket, object)
		}
	}

	// Success, return object info.
	return fi.ToObjectInfo(bucket, object), nil
}

// AbortMultipartUpload - aborts an ongoing multipart operation
// signified by the input uploadID. This is an atomic operation
// doesn't require clients to initiate multiple such requests.
//
// All parts are purged from all disks and reference to the uploadID
// would be removed from the system, rollback is not possible on this
// operation.
//
// Implements S3 compatible Abort multipart API, slight difference is
// that this is an atomic idempotent operation. Subsequent calls have
// no affect and further requests to the same uploadID would not be honored.
func (er erasureObjects) AbortMultipartUpload(ctx context.Context, bucket, object, uploadID string) error {
	if err := checkAbortMultipartArgs(ctx, bucket, object, er); err != nil {
		return err
	}
	// Validates if upload ID exists.
	if err := er.checkUploadIDExists(ctx, bucket, object, uploadID); err != nil {
		return toObjectErr(err, bucket, object, uploadID)
	}

	uploadIDPath := er.getUploadIDDir(bucket, object, uploadID)

	// Read metadata associated with the object from all disks.
	partsMetadata, errs := readAllFileInfo(er.getDisks(), minioMetaMultipartBucket, uploadIDPath)

	// get Quorum for this object
	_, writeQuorum, err := objectQuorumFromMeta(ctx, er, partsMetadata, errs)
	if err != nil {
		return toObjectErr(err, bucket, object, uploadID)
	}

	// Cleanup all uploaded parts.
	if err = er.deleteObject(ctx, minioMetaMultipartBucket, uploadIDPath, writeQuorum, false); err != nil {
		return toObjectErr(err, bucket, object, uploadID)
	}

	// Successfully purged.
	return nil
}

// Clean-up the old multipart uploads. Should be run in a Go routine.
func (er erasureObjects) cleanupStaleMultipartUploads(ctx context.Context, cleanupInterval, expiry time.Duration, doneCh chan struct{}) {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-doneCh:
			return
		case <-ticker.C:
			var disk StorageAPI
			for _, d := range er.getLoadBalancedDisks() {
				if d != nil {
					disk = d
					break
				}
			}
			if disk == nil {
				continue
			}
			er.cleanupStaleMultipartUploadsOnDisk(ctx, disk, expiry)
		}
	}
}

// Remove the old multipart uploads on the given disk.
func (er erasureObjects) cleanupStaleMultipartUploadsOnDisk(ctx context.Context, disk StorageAPI, expiry time.Duration) {
	now := time.Now()
	shaDirs, err := disk.ListDir(minioMetaMultipartBucket, "", -1)
	if err != nil {
		return
	}
	for _, shaDir := range shaDirs {
		uploadIDDirs, err := disk.ListDir(minioMetaMultipartBucket, shaDir, -1)
		if err != nil {
			continue
		}
		for _, uploadIDDir := range uploadIDDirs {
			uploadIDPath := pathJoin(shaDir, uploadIDDir)
			fi, err := disk.StatFile(minioMetaMultipartBucket, pathJoin(uploadIDPath, xlStorageFormatFile))
			if err != nil {
				continue
			}
			if now.Sub(fi.ModTime) > expiry {
				er.deleteObject(ctx, minioMetaMultipartBucket, uploadIDPath, len(er.getDisks())/2+1, false)
			}
		}
	}
}
