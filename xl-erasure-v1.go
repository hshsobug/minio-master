/*
 * Minio Cloud Storage, (C) 2016 Minio, Inc.
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

package main

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	slashpath "path"
	"strings"

	"path"
	"sync"

	"github.com/klauspost/reedsolomon"
)

const (
	// XL erasure metadata file.
	xlMetaV1File = "file.json"
)

// XL layer structure.
type XL struct {
	ReedSolomon  reedsolomon.Encoder // Erasure encoder/decoder.
	DataBlocks   int
	ParityBlocks int
	storageDisks []StorageAPI
	readQuorum   int
	writeQuorum  int
}

// errUnexpected - returned for any unexpected error.
var errUnexpected = errors.New("Unexpected error - please report at https://github.com/minio/minio/issues")

// newXL instantiate a new XL.
func newXL(disks []StorageAPI) (StorageAPI, error) {
	// Initialize XL.
	xl := &XL{}

	// Calculate data and parity blocks.
	dataBlocks, parityBlocks := len(disks)/2, len(disks)/2

	// Initialize reed solomon encoding.
	rs, err := reedsolomon.New(dataBlocks, parityBlocks)
	if err != nil {
		return nil, err
	}

	// Save the reedsolomon.
	xl.DataBlocks = dataBlocks
	xl.ParityBlocks = parityBlocks
	xl.ReedSolomon = rs

	// Save all the initialized storage disks.
	xl.storageDisks = disks

	// Figure out read and write quorum based on number of storage disks.
	// Read quorum should be always N/2 + 1 (due to Vandermonde matrix
	// erasure requirements)
	xl.readQuorum = len(xl.storageDisks)/2 + 1

	// Write quorum is assumed if we have total disks + 3
	// parity. (Need to discuss this again)
	xl.writeQuorum = len(xl.storageDisks)/2 + 3
	if xl.writeQuorum > len(xl.storageDisks) {
		xl.writeQuorum = len(xl.storageDisks)
	}

	// Return successfully initialized.
	return xl, nil
}

// MakeVol - make a volume.
func (xl XL) MakeVol(volume string) error {
	if !isValidVolname(volume) {
		return errInvalidArgument
	}

	// Err counters.
	createVolErr := 0       // Count generic create vol errs.
	volumeExistsErrCnt := 0 // Count all errVolumeExists errs.

	// Initialize sync waitgroup.
	var wg = &sync.WaitGroup{}

	// Initialize list of errors.
	var dErrs = make([]error, len(xl.storageDisks))

	// Make a volume entry on all underlying storage disks.
	for index, disk := range xl.storageDisks {
		wg.Add(1)
		// Make a volume inside a go-routine.
		go func(index int, disk StorageAPI) {
			defer wg.Done()
			if disk == nil {
				return
			}
			dErrs[index] = disk.MakeVol(volume)
		}(index, disk)
	}

	// Wait for all make vol to finish.
	wg.Wait()

	// Loop through all the concocted errors.
	for _, err := range dErrs {
		if err == nil {
			continue
		}
		// if volume already exists, count them.
		if err == errVolumeExists {
			volumeExistsErrCnt++
			continue
		}

		// Update error counter separately.
		createVolErr++
	}
	// Return err if all disks report volume exists.
	if volumeExistsErrCnt == len(xl.storageDisks) {
		return errVolumeExists
	} else if createVolErr > len(xl.storageDisks)-xl.writeQuorum {
		// Return errWriteQuorum if errors were more than
		// allowed write quorum.
		return errWriteQuorum
	}
	return nil
}

// DeleteVol - delete a volume.
func (xl XL) DeleteVol(volume string) error {
	if !isValidVolname(volume) {
		return errInvalidArgument
	}

	// Collect if all disks report volume not found.
	var volumeNotFoundErrCnt int

	var wg = &sync.WaitGroup{}
	var dErrs = make([]error, len(xl.storageDisks))

	// Remove a volume entry on all underlying storage disks.
	for index, disk := range xl.storageDisks {
		wg.Add(1)
		// Delete volume inside a go-routine.
		go func(index int, disk StorageAPI) {
			defer wg.Done()
			dErrs[index] = disk.DeleteVol(volume)
		}(index, disk)
	}

	// Wait for all the delete vols to finish.
	wg.Wait()

	// Loop through concocted errors and return anything unusual.
	for _, err := range dErrs {
		if err != nil {
			// We ignore error if errVolumeNotFound or errDiskNotFound
			if err == errVolumeNotFound || err == errDiskNotFound {
				volumeNotFoundErrCnt++
				continue
			}
			return err
		}
	}
	// Return err if all disks report volume not found.
	if volumeNotFoundErrCnt == len(xl.storageDisks) {
		return errVolumeNotFound
	}
	return nil
}

// ListVols - list volumes.
func (xl XL) ListVols() (volsInfo []VolInfo, err error) {
	// Initialize sync waitgroup.
	var wg = &sync.WaitGroup{}

	// Success vols map carries successful results of ListVols from each disks.
	var successVols = make([][]VolInfo, len(xl.storageDisks))
	for index, disk := range xl.storageDisks {
		wg.Add(1) // Add each go-routine to wait for.
		go func(index int, disk StorageAPI) {
			// Indicate wait group as finished.
			defer wg.Done()

			// Initiate listing.
			vlsInfo, _ := disk.ListVols()
			successVols[index] = vlsInfo
		}(index, disk)
	}

	// For all the list volumes running in parallel to finish.
	wg.Wait()

	// Loop through success vols and get aggregated usage values.
	var vlsInfo []VolInfo
	var total, free int64
	for _, vlsInfo = range successVols {
		if len(vlsInfo) <= 1 {
			continue
		}
		var vlInfo VolInfo
		for _, vlInfo = range vlsInfo {
			if vlInfo.Name == "" {
				continue
			}
			break
		}
		free += vlInfo.Free
		total += vlInfo.Total
	}

	// Save the updated usage values back into the vols.
	for _, vlInfo := range vlsInfo {
		vlInfo.Free = free
		vlInfo.Total = total
		volsInfo = append(volsInfo, vlInfo)
	}

	// NOTE: The assumption here is that volumes across all disks in
	// readQuorum have consistent view i.e they all have same number
	// of buckets. This is essentially not verified since healing
	// should take care of this.
	return volsInfo, nil
}

// getAllVolInfo - list bucket volume info from all disks.
// Returns error slice indicating the failed volume stat operations.
func (xl XL) getAllVolInfo(volume string) ([]VolInfo, []error) {
	// Create errs and volInfo slices of storageDisks size.
	var errs = make([]error, len(xl.storageDisks))
	var volsInfo = make([]VolInfo, len(xl.storageDisks))

	// Allocate a new waitgroup.
	var wg = &sync.WaitGroup{}
	for index, disk := range xl.storageDisks {
		wg.Add(1)
		// Stat volume on all the disks in a routine.
		go func(index int, disk StorageAPI) {
			defer wg.Done()
			volInfo, err := disk.StatVol(volume)
			if err != nil {
				errs[index] = err
				return
			}
			volsInfo[index] = volInfo
		}(index, disk)
	}

	// Wait for all the Stat operations to finish.
	wg.Wait()

	// Return the concocted values.
	return volsInfo, errs
}

// listAllVolInfo - list all stat volume info from all disks.
// Returns
// - stat volume info for all online disks.
// - boolean to indicate if healing is necessary.
// - error if any.
func (xl XL) listAllVolInfo(volume string) ([]VolInfo, bool, error) {
	volsInfo, errs := xl.getAllVolInfo(volume)
	notFoundCount := 0
	for _, err := range errs {
		if err == errVolumeNotFound {
			notFoundCount++
			// If we have errors with file not found greater than allowed read
			// quorum we return err as errFileNotFound.
			if notFoundCount > len(xl.storageDisks)-xl.readQuorum {
				return nil, false, errVolumeNotFound
			}
		}
	}

	// Calculate online disk count.
	onlineDiskCount := 0
	for index := range errs {
		if errs[index] == nil {
			onlineDiskCount++
		}
	}

	var heal bool
	// If online disks count is lesser than configured disks, most
	// probably we need to heal the file, additionally verify if the
	// count is lesser than readQuorum, if not we throw an error.
	if onlineDiskCount < len(xl.storageDisks) {
		// Online disks lesser than total storage disks, needs to be
		// healed. unless we do not have readQuorum.
		heal = true
		// Verify if online disks count are lesser than readQuorum
		// threshold, return an error if yes.
		if onlineDiskCount < xl.readQuorum {
			return nil, false, errReadQuorum
		}
	}

	// Return success.
	return volsInfo, heal, nil
}

// StatVol - get volume stat info.
func (xl XL) StatVol(volume string) (volInfo VolInfo, err error) {
	if !isValidVolname(volume) {
		return VolInfo{}, errInvalidArgument
	}

	// List and figured out if we need healing.
	volsInfo, heal, err := xl.listAllVolInfo(volume)
	if err != nil {
		return VolInfo{}, err
	}

	// Heal for missing entries.
	if heal {
		go func() {
			// Create volume if missing on disks.
			for index, volInfo := range volsInfo {
				if volInfo.Name != "" {
					continue
				}
				// Volinfo name would be an empty string, create it.
				xl.storageDisks[index].MakeVol(volume)
			}
		}()
	}

	// Loop through all statVols, calculate the actual usage values.
	var total, free int64
	for _, volInfo = range volsInfo {
		if volInfo.Name == "" {
			continue
		}
		free += volInfo.Free
		total += volInfo.Total
	}
	// Update the aggregated values.
	volInfo.Free = free
	volInfo.Total = total
	return volInfo, nil
}

// isLeafDirectoryXL - check if a given path is leaf directory. i.e
// if it contains file xlMetaV1File
func isLeafDirectoryXL(disk StorageAPI, volume, leafPath string) (isLeaf bool) {
	_, err := disk.StatFile(volume, path.Join(leafPath, xlMetaV1File))
	return err == nil
}

// ListDir - return all the entries at the given directory path.
// If an entry is a directory it will be returned with a trailing "/".
func (xl XL) ListDir(volume, dirPath string) (entries []string, err error) {
	if !isValidVolname(volume) {
		return nil, errInvalidArgument
	}

	// Count for list errors encountered.
	var listErrCount = 0

	// Loop through and return the first success entry based on the
	// selected random disk.
	for listErrCount < len(xl.storageDisks) {
		// Choose a random disk on each attempt, do not hit the same disk all the time.
		randIndex := rand.Intn(len(xl.storageDisks) - 1)
		disk := xl.storageDisks[randIndex] // Pick a random disk.
		// Initiate a list operation, if successful filter and return quickly.
		if entries, err = disk.ListDir(volume, dirPath); err == nil {
			for i, entry := range entries {
				isLeaf := isLeafDirectoryXL(disk, volume, path.Join(dirPath, entry))
				isDir := strings.HasSuffix(entry, slashSeparator)
				if isDir && isLeaf {
					entries[i] = strings.TrimSuffix(entry, slashSeparator)
				}
			}
			// We got the entries successfully return.
			return entries, nil
		}
		listErrCount++ // Update list error count.
	}
	// Return error at the end.
	return nil, err
}

// Object API.

// StatFile - stat a file
func (xl XL) StatFile(volume, path string) (FileInfo, error) {
	if !isValidVolname(volume) {
		return FileInfo{}, errInvalidArgument
	}
	if !isValidPath(path) {
		return FileInfo{}, errInvalidArgument
	}

	_, metadata, heal, err := xl.listOnlineDisks(volume, path)
	if err != nil {
		return FileInfo{}, err
	}

	if heal {
		// Heal in background safely, since we already have read quorum disks.
		go func() {
			hErr := xl.healFile(volume, path)
			errorIf(hErr, "Unable to heal file "+volume+"/"+path+".")
		}()
	}

	// Return file info.
	return FileInfo{
		Volume:  volume,
		Name:    path,
		Size:    metadata.Stat.Size,
		ModTime: metadata.Stat.ModTime,
		Mode:    os.FileMode(0644),
	}, nil
}

// deleteXLFiles - delete all XL backend files.
func (xl XL) deleteXLFiles(volume, path string) error {
	errCount := 0
	// Update meta data file and remove part file
	for index, disk := range xl.storageDisks {
		erasureFilePart := slashpath.Join(path, fmt.Sprintf("file.%d", index))
		err := disk.DeleteFile(volume, erasureFilePart)
		if err != nil {
			errCount++

			// We can safely allow DeleteFile errors up to len(xl.storageDisks) - xl.writeQuorum
			// otherwise return failure.
			if errCount <= len(xl.storageDisks)-xl.writeQuorum {
				continue
			}

			return err
		}

		xlMetaV1FilePath := slashpath.Join(path, "file.json")
		err = disk.DeleteFile(volume, xlMetaV1FilePath)
		if err != nil {
			errCount++

			// We can safely allow DeleteFile errors up to len(xl.storageDisks) - xl.writeQuorum
			// otherwise return failure.
			if errCount <= len(xl.storageDisks)-xl.writeQuorum {
				continue
			}

			return err
		}
	}
	// Return success.
	return nil
}

// DeleteFile - delete a file
func (xl XL) DeleteFile(volume, path string) error {
	if !isValidVolname(volume) {
		return errInvalidArgument
	}
	if !isValidPath(path) {
		return errInvalidArgument
	}

	// Delete all XL files.
	return xl.deleteXLFiles(volume, path)
}

// RenameFile - rename file.
func (xl XL) RenameFile(srcVolume, srcPath, dstVolume, dstPath string) error {
	// Validate inputs.
	if !isValidVolname(srcVolume) {
		return errInvalidArgument
	}
	if !isValidPath(srcPath) {
		return errInvalidArgument
	}
	if !isValidVolname(dstVolume) {
		return errInvalidArgument
	}
	if !isValidPath(dstPath) {
		return errInvalidArgument
	}

	// Initialize sync waitgroup.
	var wg = &sync.WaitGroup{}

	// Initialize list of errors.
	var errs = make([]error, len(xl.storageDisks))

	// Rename file on all underlying storage disks.
	for index, disk := range xl.storageDisks {
		// Append "/" as srcPath and dstPath are either leaf-dirs or non-leaf-dris.
		// If srcPath is an object instead of prefix we just rename the leaf-dir and
		// not rename the part and metadata files separately.
		wg.Add(1)
		go func(index int, disk StorageAPI) {
			defer wg.Done()
			err := disk.RenameFile(srcVolume, retainSlash(srcPath), dstVolume, retainSlash(dstPath))
			if err != nil {
				errs[index] = err
			}
			errs[index] = nil
		}(index, disk)
	}

	// Wait for all RenameFile to finish.
	wg.Wait()

	// Gather err count.
	var errCount = 0
	for _, err := range errs {
		if err == nil {
			continue
		}
		errCount++
	}
	// We can safely allow RenameFile errors up to len(xl.storageDisks) - xl.writeQuorum
	// otherwise return failure. Cleanup successful renames.
	if errCount > len(xl.storageDisks)-xl.writeQuorum {
		// Special condition if readQuorum exists, then return success.
		if errCount <= len(xl.storageDisks)-xl.readQuorum {
			return nil
		}
		// Ignore errors here, delete all successfully written files.
		xl.deleteXLFiles(dstVolume, dstPath)
		return errWriteQuorum
	}
	return nil
}
