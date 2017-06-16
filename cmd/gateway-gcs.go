/*
 * Minio Cloud Storage, (C) 2017 Minio, Inc.
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
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"

	"encoding/hex"

	"cloud.google.com/go/storage"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"

	"encoding/base64"

	"bytes"

	// this package contains the url code of go 1.8
	// due to compatibility with older versions
	// copied it to our rep

	"path/filepath"

	minio "github.com/minio/minio-go"
	"github.com/minio/minio-go/pkg/policy"
)

var (
	// ErrNotValidMultipartIdentifier the multipart identifier is not in the correct form
	ErrNotValidMultipartIdentifier = errors.New("Not a valid multipart identifier")
)

const (
	// ZZZZMinioPrefix is used for metadata and multiparts. The prefix is being filtered out,
	// hence the naming of ZZZZ (last prefix)
	ZZZZMinioPrefix = "ZZZZ-Minio"
)

// Convert Minio errors to minio object layer errors.
func gcsToObjectError(err error, params ...string) error {
	if err == nil {
		return nil
	}

	e, ok := err.(*Error)
	if !ok {
		// Code should be fixed if this function is called without doing traceError()
		// Else handling different situations in this function makes this function complicated.
		errorIf(err, "Expected type *Error")
		return err
	}

	err = e.e

	bucket := ""
	object := ""
	if len(params) >= 1 {
		bucket = params[0]
	}
	if len(params) == 2 {
		object = params[1]
	}

	// in some cases just a plain error is being returned
	switch err.Error() {
	case "storage: bucket doesn't exist":
		err = BucketNotFound{
			Bucket: bucket,
		}
		e.e = err
		return e
	case "storage: object doesn't exist":
		err = ObjectNotFound{
			Bucket: bucket,
			Object: object,
		}
		e.e = err
		return e
	}

	googleAPIErr, ok := err.(*googleapi.Error)
	if !ok {
		// We don't interpret non Minio errors. As minio errors will
		// have StatusCode to help to convert to object errors.
		return e
	}

	reason := googleAPIErr.Errors[0].Reason
	message := googleAPIErr.Errors[0].Message

	switch reason {
	case "required":
		// Anonymous users does not have storage.xyz access to project 123.
		fallthrough
	case "keyInvalid":
		fallthrough
	case "forbidden":
		err = PrefixAccessDenied{
			Bucket: bucket,
			Object: object,
		}
	case "invalid":
		err = BucketNameInvalid{
			Bucket: bucket,
		}
	case "notFound":
		if object != "" {
			err = ObjectNotFound{
				Bucket: bucket,
				Object: object,
			}
		} else {
			err = BucketNotFound{
				Bucket: bucket,
			}
		}
	case "conflict":
		if message == "You already own this bucket. Please select another name." {
			err = BucketAlreadyOwnedByYou{
				Bucket: bucket,
			}
		} else if message == "Sorry, that name is not available. Please try a different one." {
			err = BucketAlreadyExists{
				Bucket: bucket,
			}
		} else {
			err = BucketNotEmpty{
				Bucket: bucket,
			}
		}
	default:
		err = fmt.Errorf("Unsupported error reason: %s", reason)

	}

	_ = bucket
	_ = object

	e.e = err
	return e
}

// gcsGateway - Implements gateway for Minio and GCS compatible object storage servers.
type gcsGateway struct {
	client     *storage.Client
	anonClient *minio.Core
	projectID  string
	ctx        context.Context
}

// newGCSGateway returns gcs gatewaylayer
func newGCSGateway(args []string) (GatewayLayer, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("ProjectID expected")
	}

	profile := "default"
	// TODO: don't know where to set profile yet
	_ = profile

	endpoint := "storage.googleapis.com"
	secure := true

	projectID := args[0]

	ctx := context.Background()

	// Creates a client.
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}

	anonClient, err := minio.NewCore(endpoint, "", "", secure)
	if err != nil {
		return nil, err
	}

	return &gcsGateway{
		client:     client,
		projectID:  projectID,
		ctx:        ctx,
		anonClient: anonClient,
	}, nil
}

// Shutdown - save any gateway metadata to disk
// if necessary and reload upon next restart.
func (l *gcsGateway) Shutdown() error {
	// TODO
	return nil
}

// StorageInfo - Not relevant to GCS backend.
func (l *gcsGateway) StorageInfo() StorageInfo {
	return StorageInfo{}
}

// MakeBucket - Create a new container on GCS backend.
func (l *gcsGateway) MakeBucket(bucket string) error {
	// will never be called, only satisfy ObjectLayer interface
	return traceError(NotImplemented{})
}

// MakeBucket - Create a new container on GCS backend.
func (l *gcsGateway) MakeBucketWithLocation(bucket, location string) error {
	bkt := l.client.Bucket(bucket)

	// this will map s3 regions to google multi regions
	if v, ok := map[string]string{
		"ap-northeast-1": "asia",
		"ap-northeast-2": "asia",
		"ap-south-1":     "asia",
		"ap-southeast-1": "asia",
		"ap-southeast-2": "asia",
		"eu-central-1":   "eu",
		"eu-west-1":      "eu",
		"eu-west-2":      "eu",
		"ca-central-1":   "us",
		"sa-east-1":      "us",
		"us-east-1":      "us",
		"us-east-2":      "us",
		"us-west-1":      "us",
		"us-west-2":      "us",
	}[location]; ok {
		location = v
	}

	if err := bkt.Create(l.ctx, l.projectID, &storage.BucketAttrs{
		Location: location,
	}); err != nil {
		return gcsToObjectError(traceError(err), bucket)
	}

	return nil
}

// GetBucketInfo - Get bucket metadata..
func (l *gcsGateway) GetBucketInfo(bucket string) (BucketInfo, error) {
	attrs, err := l.client.Bucket(bucket).Attrs(l.ctx)
	if err != nil {
		return BucketInfo{}, gcsToObjectError(traceError(err), bucket)
	}

	return BucketInfo{
		Name:    attrs.Name,
		Created: attrs.Created,
	}, nil
}

// ListBuckets lists all GCS buckets
func (l *gcsGateway) ListBuckets() ([]BucketInfo, error) {
	it := l.client.Buckets(l.ctx, l.projectID)

	b := []BucketInfo{}
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}

		if err != nil {
			return []BucketInfo{}, gcsToObjectError(traceError(err))
		}

		b = append(b, BucketInfo{
			Name:    attrs.Name,
			Created: attrs.Created,
		})
	}

	return b, nil
}

// DeleteBucket delete a bucket on GCS
func (l *gcsGateway) DeleteBucket(bucket string) error {
	err := l.client.Bucket(bucket).Delete(l.ctx)
	if err != nil {
		return gcsToObjectError(traceError(err), bucket)
	}

	return nil
}

func toGCSPageToken(name string) string {
	length := uint16(len(name))

	b := []byte{
		0xa,
		byte(length & 0xFF),
	}

	length = length >> 7
	if length > 0 {
		b = append(b, byte(length&0xFF))
	}

	b = append(b, []byte(name)...)

	return base64.StdEncoding.EncodeToString(b)
}

// ListObjects - lists all blobs in GCS bucket filtered by prefix
func (l *gcsGateway) ListObjects(bucket string, prefix string, marker string, delimiter string, maxKeys int) (ListObjectsInfo, error) {
	it := l.client.Bucket(bucket).Objects(l.ctx, &storage.Query{Delimiter: delimiter, Prefix: prefix, Versions: false})

	isTruncated := false
	nextMarker := ""
	prefixes := []string{}

	// we'll set marker to continue
	it.PageInfo().Token = marker
	it.PageInfo().MaxSize = maxKeys

	objects := []ObjectInfo{}
	for {
		if len(objects) >= maxKeys {
			// check if there is one next object and
			// if that one next object is our hidden
			// metadata folder, then just break
			// otherwise we've truncated the output

			attrs, _ := it.Next()
			if attrs == nil {
			} else if attrs.Prefix == ZZZZMinioPrefix {
				break
			}

			isTruncated = true
			break
		}

		attrs, err := it.Next()
		if err == iterator.Done {
			break
		} else if err != nil {
			return ListObjectsInfo{}, gcsToObjectError(traceError(err), bucket, prefix)
		}

		nextMarker = toGCSPageToken(attrs.Name)

		if attrs.Prefix == ZZZZMinioPrefix {
			// we don't return our metadata prefix
			continue
		} else if attrs.Prefix != "" {
			prefixes = append(prefixes, attrs.Prefix)
			continue
		}

		objects = append(objects, ObjectInfo{
			Name:            attrs.Name,
			Bucket:          attrs.Bucket,
			ModTime:         attrs.Updated,
			Size:            attrs.Size,
			MD5Sum:          hex.EncodeToString(attrs.MD5),
			UserDefined:     attrs.Metadata,
			ContentType:     attrs.ContentType,
			ContentEncoding: attrs.ContentEncoding,
		})
	}

	return ListObjectsInfo{
		IsTruncated: isTruncated,
		NextMarker:  nextMarker,
		Prefixes:    prefixes,
		Objects:     objects,
	}, nil
}

// ListObjectsV2 - lists all blobs in GCS bucket filtered by prefix
func (l *gcsGateway) ListObjectsV2(bucket, prefix, continuationToken string, fetchOwner bool, delimiter string, maxKeys int) (ListObjectsV2Info, error) {
	it := l.client.Bucket(bucket).Objects(l.ctx, &storage.Query{Delimiter: delimiter, Prefix: prefix, Versions: false})

	isTruncated := false
	nextMarker := ""
	prefixes := []string{}

	objects := []ObjectInfo{}
	for {
		if maxKeys < len(objects) {
			isTruncated = true
			nextMarker = it.PageInfo().Token
			break
		}

		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}

		if err != nil {
			return ListObjectsV2Info{}, gcsToObjectError(traceError(err), bucket, prefix)
		}

		if attrs.Prefix != "" {
			prefixes = append(prefixes, attrs.Prefix)
			continue
		}

		objects = append(objects, fromGCSObjectInfo(attrs))
	}

	return ListObjectsV2Info{
		IsTruncated:           isTruncated,
		ContinuationToken:     continuationToken,
		NextContinuationToken: nextMarker,
		Prefixes:              prefixes,
		Objects:               objects,
	}, nil
}

// GetObject - reads an object from GCS. Supports additional
// parameters like offset and length which are synonymous with
// HTTP Range requests.
//
// startOffset indicates the starting read location of the object.
// length indicates the total length of the object.
func (l *gcsGateway) GetObject(bucket string, key string, startOffset int64, length int64, writer io.Writer) error {
	// if we want to mimic S3 behavior exactly, we need to verify if bucket exists first,
	// otherwise gcs will just return object not exist in case of non-existing bucket
	if _, err := l.client.Bucket(bucket).Attrs(l.ctx); err != nil {
		return gcsToObjectError(traceError(err), bucket)
	}

	object := l.client.Bucket(bucket).Object(key)
	r, err := object.NewRangeReader(l.ctx, startOffset, length)
	if err != nil {
		return gcsToObjectError(traceError(err), bucket, key)
	}

	defer r.Close()

	if _, err := io.Copy(writer, r); err != nil {
		return gcsToObjectError(traceError(err), bucket, key)
	}

	return nil
}

// fromGCSObjectInfo converts GCS BucketAttrs to gateway ObjectInfo
func fromGCSObjectInfo(attrs *storage.ObjectAttrs) ObjectInfo {
	return ObjectInfo{
		Name:            attrs.Name,
		Bucket:          attrs.Bucket,
		ModTime:         attrs.Updated,
		Size:            attrs.Size,
		MD5Sum:          hex.EncodeToString(attrs.MD5),
		UserDefined:     attrs.Metadata,
		ContentType:     attrs.ContentType,
		ContentEncoding: attrs.ContentEncoding,
	}
}

// GetObjectInfo - reads object info and replies back ObjectInfo
func (l *gcsGateway) GetObjectInfo(bucket string, object string) (ObjectInfo, error) {
	// if we want to mimic S3 behavior exactly, we need to verify if bucket exists first,
	// otherwise gcs will just return object not exist in case of non-existing bucket
	if _, err := l.client.Bucket(bucket).Attrs(l.ctx); err != nil {
		return ObjectInfo{}, gcsToObjectError(traceError(err), bucket)
	}

	attrs, err := l.client.Bucket(bucket).Object(object).Attrs(l.ctx)
	if err != nil {
		return ObjectInfo{}, gcsToObjectError(traceError(err), bucket, object)
	}

	return fromGCSObjectInfo(attrs), nil
}

// PutObject - Create a new object with the incoming data,
func (l *gcsGateway) PutObject(bucket string, key string, size int64, data io.Reader, metadata map[string]string, sha256sum string) (ObjectInfo, error) {
	// if we want to mimic S3 behavior exactly, we need to verify if bucket exists first,
	// otherwise gcs will just return object not exist in case of non-existing bucket
	if _, err := l.client.Bucket(bucket).Attrs(l.ctx); err != nil {
		return ObjectInfo{}, gcsToObjectError(traceError(err), bucket)
	}

	teeReader := data

	var sha256Writer hash.Hash
	if sha256sum == "" {
	} else if _, err := hex.DecodeString(sha256sum); err != nil {
		return ObjectInfo{}, gcsToObjectError(traceError(err), bucket, key)
	} else {
		sha256Writer = sha256.New()
		teeReader = io.TeeReader(teeReader, sha256Writer)
	}

	md5sum := metadata["md5Sum"]
	delete(metadata, "md5Sum")

	object := l.client.Bucket(bucket).Object(key)

	w := object.NewWriter(l.ctx)

	w.ContentType = metadata["content-type"]
	w.ContentEncoding = metadata["content-encoding"]
	w.Metadata = metadata

	_, err := io.Copy(w, teeReader)
	if err != nil {
		return ObjectInfo{}, gcsToObjectError(traceError(err), bucket, key)
	}

	err = w.Close()
	if err != nil {
		return ObjectInfo{}, gcsToObjectError(traceError(err), bucket, key)
	}

	attrs, err := object.Attrs(l.ctx)
	if err != nil {
		return ObjectInfo{}, gcsToObjectError(traceError(err), bucket, key)
	}

	if sha256sum == "" {
	} else if newSHA256sum := hex.EncodeToString(sha256Writer.Sum(nil)); newSHA256sum != sha256sum {
		object.Delete(l.ctx)
		return ObjectInfo{}, traceError(SHA256Mismatch{})
	}

	if md5sum == "" {
	} else if b, err := hex.DecodeString(md5sum); err != nil {
	} else if bytes.Compare(b, attrs.MD5) != 0 {
		object.Delete(l.ctx)
		return ObjectInfo{}, traceError(SignatureDoesNotMatch{})
	}

	return fromGCSObjectInfo(attrs), nil
}

// CopyObject - Copies a blob from source container to destination container.
func (l *gcsGateway) CopyObject(srcBucket string, srcObject string, destBucket string, destObject string, metadata map[string]string) (ObjectInfo, error) {
	src := l.client.Bucket(srcBucket).Object(srcObject)
	dst := l.client.Bucket(destBucket).Object(destObject)

	attrs, err := dst.CopierFrom(src).Run(l.ctx)
	if err != nil {
		return ObjectInfo{}, gcsToObjectError(traceError(err), destBucket, destObject)
	}

	return fromGCSObjectInfo(attrs), nil
}

// DeleteObject - Deletes a blob in bucket
func (l *gcsGateway) DeleteObject(bucket string, object string) error {
	err := l.client.Bucket(bucket).Object(object).Delete(l.ctx)
	if err != nil {
		return gcsToObjectError(traceError(err), bucket, object)
	}

	return nil
}

// ListMultipartUploads - lists all multipart uploads.
func (l *gcsGateway) ListMultipartUploads(bucket string, prefix string, keyMarker string, uploadIDMarker string, delimiter string, maxUploads int) (ListMultipartsInfo, error) {
	// TODO: implement prefix and prefixes, how does this work for Multiparts??
	prefix = fmt.Sprintf("%s/multipart-", ZZZZMinioPrefix)

	it := l.client.Bucket(bucket).Objects(l.ctx, &storage.Query{Delimiter: delimiter, Prefix: prefix, Versions: false})

	isTruncated := false
	// prefixes := []string{}
	nextMarker := ""

	uploads := []uploadMetadata{}
	for {
		if maxUploads <= len(uploads) {
			isTruncated = true
			nextMarker = it.PageInfo().Token
			break
		}

		attrs, err := it.Next()
		if err == iterator.Done {
			break
		} else if err != nil {
			return ListMultipartsInfo{}, gcsToObjectError(traceError(err), bucket)
		}

		if attrs.Prefix != "" {
			continue
		}

		objectKey, uploadID, partID, err := fromGCSMultipartKey(attrs.Name)
		if err != nil {
			continue
		} else if partID != 0 {
			continue
		}

		// we count only partID == 0
		uploads = append(uploads, uploadMetadata{
			Object:    objectKey,
			UploadID:  uploadID,
			Initiated: attrs.Created,
		})

	}

	return ListMultipartsInfo{
		Uploads:     uploads,
		IsTruncated: isTruncated,

		KeyMarker:          nextMarker,
		UploadIDMarker:     nextMarker,
		NextKeyMarker:      nextMarker,
		NextUploadIDMarker: nextMarker,
		MaxUploads:         maxUploads,
	}, nil

}

func fromGCSMultipartKey(s string) (key, uploadID string, partID int, err error) {
	// remove prefixes
	s = filepath.Base(s)

	parts := strings.Split(s, "-")
	if parts[0] != "multipart" {
		return "", "", 0, ErrNotValidMultipartIdentifier
	}

	if len(parts) != 4 {
		return "", "", 0, ErrNotValidMultipartIdentifier
	}

	key = unescape(parts[1])

	uploadID = parts[3]

	partID, err = strconv.Atoi(parts[3])
	if err != nil {
		return "", "", 0, err
	}

	return
}

func unescape(s string) string {
	s = strings.Replace(s, "%2D", "-", -1)
	s = strings.Replace(s, "%2F", "/", -1)
	s = strings.Replace(s, "%25", "%", -1)
	return s
}

func escape(s string) string {
	s = strings.Replace(s, "%", "%25", -1)
	s = strings.Replace(s, "/", "%2F", -1)
	s = strings.Replace(s, "-", "%2D", -1)
	return s
}

func toGCSMultipartKey(key string, uploadID string, partID int) string {
	// parts are allowed to be numbered from 1 to 10,000 (inclusive)

	// we need to encode the key because of possible slashes
	return fmt.Sprintf("%s/multipart-%s-%s-%05d", ZZZZMinioPrefix, escape(key), uploadID, partID)
}

// NewMultipartUpload - upload object in multiple parts
func (l *gcsGateway) NewMultipartUpload(bucket string, key string, metadata map[string]string) (uploadID string, err error) {
	// generate new uploadid
	uploadID = mustGetUUID()
	uploadID = strings.Replace(uploadID, "-", "", -1)

	// generate name for part zero
	partZeroKey := toGCSMultipartKey(key, uploadID, 0)

	// we are writing a 0 sized object to hold the metadata
	w := l.client.Bucket(bucket).Object(partZeroKey).NewWriter(l.ctx)
	w.ContentType = metadata["content-type"]
	w.ContentEncoding = metadata["content-encoding"]
	w.Metadata = metadata
	if err = w.Close(); err != nil {
		return "", gcsToObjectError(traceError(err), bucket, key)
	}

	return uploadID, nil
}

// CopyObjectPart - copy part of object to other bucket and object
func (l *gcsGateway) CopyObjectPart(srcBucket string, srcObject string, destBucket string, destObject string, uploadID string, partID int, startOffset int64, length int64) (info PartInfo, err error) {
	return PartInfo{}, traceError(NotSupported{})
}

// PutObjectPart puts a part of object in bucket
func (l *gcsGateway) PutObjectPart(bucket string, key string, uploadID string, partID int, size int64, data io.Reader, md5Hex string, sha256sum string) (PartInfo, error) {
	multipartKey := toGCSMultipartKey(key, uploadID, partID)

	info, err := l.PutObject(bucket, multipartKey, size, data, map[string]string{}, sha256sum)
	return PartInfo{
		PartNumber:   partID,
		LastModified: info.ModTime,
		ETag:         info.MD5Sum,
		Size:         info.Size,
	}, err
}

// ListObjectParts returns all object parts for specified object in specified bucket
func (l *gcsGateway) ListObjectParts(bucket string, key string, uploadID string, partNumberMarker int, maxParts int) (ListPartsInfo, error) {
	prefix := ""
	delimiter := "/"

	it := l.client.Bucket(bucket).Objects(l.ctx, &storage.Query{Delimiter: delimiter, Prefix: prefix, Versions: false})

	isTruncated := false

	it.PageInfo().Token = toGCSMultipartKey(key, uploadID, partNumberMarker)
	it.PageInfo().MaxSize = maxParts

	nextPartnumberMarker := 0

	parts := []PartInfo{}
	for {
		if len(parts) >= maxParts {
			isTruncated = true
			break
		}

		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}

		if err != nil {
			return ListPartsInfo{}, gcsToObjectError(traceError(err), bucket, prefix)
		}

		if attrs.Prefix != "" {
			continue
		}

		_, _, partID, err := fromGCSMultipartKey(attrs.Name)
		if err != nil {
			continue
		} else if partID == 0 {
			// we'll ignore partID 0, it is our zero object, containing
			// metadata
			continue
		}

		nextPartnumberMarker = partID

		parts = append(parts, PartInfo{
			PartNumber:   partID,
			LastModified: attrs.Updated,
			ETag:         hex.EncodeToString(attrs.MD5),
			Size:         attrs.Size,
		})
	}

	return ListPartsInfo{
		IsTruncated:          isTruncated,
		NextPartNumberMarker: nextPartnumberMarker,
		Parts:                parts,
	}, nil
}

// AbortMultipartUpload aborts a ongoing multipart upload
func (l *gcsGateway) AbortMultipartUpload(bucket string, key string, uploadID string) error {

	delimiter := "/"
	prefix := fmt.Sprintf("%s/multipart-%s-%s", ZZZZMinioPrefix, key, uploadID)

	// iterate through all parts and delete them
	it := l.client.Bucket(bucket).Objects(l.ctx, &storage.Query{Delimiter: delimiter, Prefix: prefix, Versions: false})

	it.PageInfo().Token = toGCSPageToken(toGCSMultipartKey(key, uploadID, 0))

	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		} else if err != nil {
			return gcsToObjectError(traceError(err), bucket, key)
		}

		// on error continue deleting other parts
		l.client.Bucket(bucket).Object(attrs.Name).Delete(l.ctx)
	}

	// delete part zero, ignoring errors here, we want to clean up all remains
	_ = l.client.Bucket(bucket).Object(toGCSMultipartKey(key, uploadID, 0)).Delete(l.ctx)

	return nil
}

// CompleteMultipartUpload completes ongoing multipart upload and finalizes object

// Note that there is a limit (currently 32) to the number of components that can be composed in a single operation.

// There is a limit (currently 1024) to the total number of components for a given composite object. This means you can append to each object at most 1023 times.

// There is a per-project rate limit (currently 200) to the number of components you can compose per second. This rate counts both the components being appended to a composite object as well as the components being copied when the composite object of which they are a part is copied.
func (l *gcsGateway) CompleteMultipartUpload(bucket string, key string, uploadID string, uploadedParts []completePart) (ObjectInfo, error) {
	partZero := l.client.Bucket(bucket).Object(toGCSMultipartKey(key, uploadID, 0))

	partZeroAttrs, err := partZero.Attrs(l.ctx)
	if err != nil {
		return ObjectInfo{}, gcsToObjectError(traceError(err), bucket, key)
	}

	parts := make([]*storage.ObjectHandle, len(uploadedParts))
	for i, uploadedPart := range uploadedParts {
		object := l.client.Bucket(bucket).Object(toGCSMultipartKey(key, uploadID, uploadedPart.PartNumber))

		if etag, partErr := hex.DecodeString(uploadedPart.ETag); partErr != nil {
		} else if attrs, partErr := object.Attrs(l.ctx); partErr != nil {
			return ObjectInfo{}, gcsToObjectError(traceError(partErr), bucket, key)
		} else if bytes.Compare(attrs.MD5, etag) != 0 {
			return ObjectInfo{}, gcsToObjectError(traceError(InvalidPart{}), bucket, key)
		}

		parts[i] = object
	}

	if len(parts) > 32 {
		// we need to split up the compose of more than 32 parts
		// into subcomposes. This means that the first 32 parts will
		// compose to a composed-object-0, next parts to composed-object-1,
		// the final compose will compose composed-object* to 1.
		return ObjectInfo{}, traceError(NotSupported{})
	}

	dst := l.client.Bucket(bucket).Object(key)

	composer := dst.ComposerFrom(parts...)

	composer.ContentType = partZeroAttrs.ContentType
	composer.Metadata = partZeroAttrs.Metadata

	attrs, err := composer.Run(l.ctx)

	// cleanup, delete all parts
	for _, uploadedPart := range uploadedParts {
		l.client.Bucket(bucket).Object(toGCSMultipartKey(key, uploadID, uploadedPart.PartNumber)).Delete(l.ctx)
	}

	partZero.Delete(l.ctx)

	return fromGCSObjectInfo(attrs), gcsToObjectError(traceError(err), bucket, key)
}

// SetBucketPolicies - Set policy on bucket
func (l *gcsGateway) SetBucketPolicies(bucket string, policyInfo policy.BucketAccessPolicy) error {
	var policies []BucketAccessPolicy

	for prefix, policy := range policy.GetPolicies(policyInfo.Statements, bucket) {
		policies = append(policies, BucketAccessPolicy{
			Prefix: prefix,
			Policy: policy,
		})
	}

	prefix := bucket + "/*" // For all objects inside the bucket.

	if len(policies) != 1 {
		return traceError(NotImplemented{})
	} else if policies[0].Prefix != prefix {
		return traceError(NotImplemented{})
	}

	acl := l.client.Bucket(bucket).ACL()

	if policies[0].Policy == policy.BucketPolicyNone {
		if err := acl.Delete(l.ctx, storage.AllUsers); err != nil {
			return gcsToObjectError(traceError(err), bucket)
		}

		return nil
	}

	role := storage.RoleReader

	switch policies[0].Policy {
	case policy.BucketPolicyReadOnly:
		role = storage.RoleReader
	case policy.BucketPolicyWriteOnly:
		role = storage.RoleWriter
	case policy.BucketPolicyReadWrite:
		// not supported, google only has owner role
		return gcsToObjectError(traceError(NotSupported{}), bucket)
	default:
		return gcsToObjectError(traceError(fmt.Errorf("Unknown policy: %s", policies[0].Policy)), bucket)
	}

	if err := acl.Set(l.ctx, storage.AllUsers, role); err != nil {
		return gcsToObjectError(traceError(err), bucket)
	}

	return nil
}

// GetBucketPolicies - Get policy on bucket
func (l *gcsGateway) GetBucketPolicies(bucket string) (policy.BucketAccessPolicy, error) {
	acl := l.client.Bucket(bucket).ACL()

	rules, err := acl.List(l.ctx)
	if err != nil {
		return policy.BucketAccessPolicy{}, gcsToObjectError(traceError(err), bucket)
	}

	policyInfo := policy.BucketAccessPolicy{Version: "2012-10-17"}

	for _, r := range rules {
		if r.Entity != storage.AllUsers {
			continue
		}

		switch r.Role {
		case storage.RoleOwner:
			return policy.BucketAccessPolicy{}, gcsToObjectError(traceError(NotSupported{}), bucket)
		case storage.RoleReader:
			policyInfo.Statements = policy.SetPolicy(policyInfo.Statements, policy.BucketPolicyReadOnly, bucket, "")
		case storage.RoleWriter:
			policyInfo.Statements = policy.SetPolicy(policyInfo.Statements, policy.BucketPolicyWriteOnly, bucket, "")
		}
	}

	return policyInfo, nil
}

// DeleteBucketPolicies - Delete all policies on bucket
func (l *gcsGateway) DeleteBucketPolicies(bucket string) error {
	acl := l.client.Bucket(bucket).ACL()

	// this only removes the storage.AllUsers policies
	if err := acl.Delete(l.ctx, storage.AllUsers); err != nil {
		return gcsToObjectError(traceError(err), bucket)
	}

	return nil
}
