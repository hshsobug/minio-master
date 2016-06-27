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
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"sync"

	"github.com/klauspost/reedsolomon"
)

// isSuccessDecodeBlocks - do we have all the blocks to be
// successfully decoded?. Input encoded blocks ordered matrix.
func isSuccessDecodeBlocks(enBlocks [][]byte, dataBlocks int) bool {
	// Count number of data and parity blocks that were read.
	var successDataBlocksCount = 0
	var successParityBlocksCount = 0
	for index := range enBlocks {
		if enBlocks[index] == nil {
			continue
		}
		// block index lesser than data blocks, update data block count.
		if index < dataBlocks {
			successDataBlocksCount++
			continue
		} // else { // update parity block count.
		successParityBlocksCount++
	}
	// Returns true if we have atleast dataBlocks + 1 parity.
	return successDataBlocksCount == dataBlocks || successDataBlocksCount+successParityBlocksCount >= dataBlocks+1
}

// isSuccessDataBlocks - do we have all the data blocks?
// Input encoded blocks ordered matrix.
func isSuccessDataBlocks(enBlocks [][]byte, dataBlocks int) bool {
	// Count number of data blocks that were read.
	var successDataBlocksCount = 0
	for index := range enBlocks[:dataBlocks] {
		if enBlocks[index] == nil {
			continue
		}
		// block index lesser than data blocks, update data block count.
		if index < dataBlocks {
			successDataBlocksCount++
		}
	}
	// Returns true if we have atleast the dataBlocks.
	return successDataBlocksCount >= dataBlocks
}

// getOrderedDisks - get ordered disks from erasure distribution.
// returns ordered slice of disks from their actual distribution.
func getOrderedDisks(distribution []int, disks []StorageAPI, blockCheckSums []checkSumInfo) (orderedDisks []StorageAPI, orderedBlockCheckSums []checkSumInfo) {
	orderedDisks = make([]StorageAPI, len(disks))
	orderedBlockCheckSums = make([]checkSumInfo, len(disks))
	// From disks gets ordered disks.
	for index := range disks {
		blockIndex := distribution[index]
		orderedDisks[blockIndex-1] = disks[index]
		orderedBlockCheckSums[blockIndex-1] = blockCheckSums[index]
	}
	return orderedDisks, orderedBlockCheckSums
}

// Return readable disks slice from which we can read parallelly.
func getReadDisks(orderedDisks []StorageAPI, index int, dataBlocks int) (readDisks []StorageAPI, nextIndex int, err error) {
	readDisks = make([]StorageAPI, len(orderedDisks))
	dataDisks := 0
	parityDisks := 0
	// Count already read data and parity chunks.
	for i := 0; i < index; i++ {
		if orderedDisks[i] == nil {
			continue
		}
		if i < dataBlocks {
			dataDisks++
		} else {
			parityDisks++
		}
	}

	// Sanity checks - we should never have this situation.
	if dataDisks == dataBlocks {
		return nil, 0, errUnexpected
	}
	if dataDisks+parityDisks >= dataBlocks+1 {
		return nil, 0, errUnexpected
	}

	// Find the disks from which next set of parallel reads should happen.
	for i := index; i < len(orderedDisks); i++ {
		if orderedDisks[i] == nil {
			continue
		}
		if i < dataBlocks {
			dataDisks++
		} else {
			parityDisks++
		}
		readDisks[i] = orderedDisks[i]
		if dataDisks == dataBlocks {
			return readDisks, i + 1, nil
		}
		if dataDisks+parityDisks == dataBlocks+1 {
			return readDisks, i + 1, nil
		}
	}
	return nil, 0, errXLReadQuorum
}

// parallelRead - reads chunks in parallel from the disks specified in []readDisks.
func parallelRead(volume, path string, readDisks []StorageAPI, orderedDisks []StorageAPI, enBlocks [][]byte, blockOffset int64, curChunkSize int64, bitRotVerify func(diskIndex int) bool) {
	// WaitGroup to synchronise the read go-routines.
	wg := &sync.WaitGroup{}

	// Read disks in parallel.
	for index := range readDisks {
		if readDisks[index] == nil {
			continue
		}
		wg.Add(1)
		// Reads chunk from readDisk[index] in routine.
		go func(index int) {
			defer wg.Done()

			// Verify bit rot for the file on this disk.
			if !bitRotVerify(index) {
				// So that we don't read from this disk for the next block.
				orderedDisks[index] = nil
				return
			}

			// Chunk writer.
			chunkWriter := bytes.NewBuffer(make([]byte, 0, curChunkSize))

			// CopyN - copies until current chunk size.
			err := copyN(chunkWriter, readDisks[index], volume, path, blockOffset, curChunkSize)
			if err != nil {
				// So that we don't read from this disk for the next block.
				orderedDisks[index] = nil
				return
			}

			// Copy the read blocks.
			enBlocks[index] = chunkWriter.Bytes()

			// Successfully read.
		}(index)
	}

	// Waiting for first routines to finish.
	wg.Wait()
}

// erasureReadFile - read bytes from erasure coded files and writes to given writer.
// Erasure coded files are read block by block as per given erasureInfo and data chunks
// are decoded into a data block. Data block is trimmed for given offset and length,
// then written to given writer. This function also supports bit-rot detection by
// verifying checksum of individual block's checksum.
func erasureReadFile(writer io.Writer, disks []StorageAPI, volume string, path string, partName string, eInfos []erasureInfo, offset int64, length int64, totalLength int64) (int64, error) {
	// Pick one erasure info.
	eInfo := pickValidErasureInfo(eInfos)

	// Gather previously calculated block checksums.
	blockCheckSums := metaPartBlockChecksums(disks, eInfos, partName)

	// []orderedDisks will have first eInfo.DataBlocks disks as data
	// disks and rest will be parity.
	orderedDisks, orderedBlockCheckSums := getOrderedDisks(eInfo.Distribution, disks, blockCheckSums)

	// bitRotVerify verifies if the file on a particular disk doesn't have bitrot
	// by verifying the hash of the contents of the file.
	bitRotVerify := func() func(diskIndex int) bool {
		verified := make([]bool, len(orderedDisks))
		// Return closure so that we have reference to []verified and
		// not recalculate the hash on it every time the function is
		// called for the same disk.
		return func(diskIndex int) bool {
			if verified[diskIndex] {
				// Already validated.
				return true
			}
			// Is this a valid block?
			isValid := isValidBlock(orderedDisks[diskIndex], volume, path, orderedBlockCheckSums[diskIndex])
			verified[diskIndex] = isValid
			return isValid
		}
	}()

	// Total bytes written to writer
	bytesWritten := int64(0)

	// chunkSize is roughly BlockSize/DataBlocks.
	// chunkSize is calculated such that chunkSize*DataBlocks accommodates BlockSize bytes.
	// So chunkSize*DataBlocks can be slightly larger than BlockSize if BlockSize is not divisible by
	// DataBlocks. The extra space will have 0-padding.
	chunkSize := getEncodedBlockLen(eInfo.BlockSize, eInfo.DataBlocks)

	// Get start and end block, also bytes to be skipped based on the input offset.
	startBlock, endBlock, bytesToSkip := getBlockInfo(offset, totalLength, eInfo.BlockSize)

	// For each block, read chunk from each disk. If we are able to read all the data disks then we don't
	// need to read parity disks. If one of the data disk is missing we need to read DataBlocks+1 number
	// of disks. Once read, we Reconstruct() missing data if needed and write it to the given writer.
	for block := startBlock; bytesWritten < length; block++ {
		// Each element of enBlocks holds curChunkSize'd amount of data read from its corresponding disk.
		enBlocks := make([][]byte, len(orderedDisks))

		// enBlocks data can have 0-padding hence we need to figure the exact number
		// of bytes we want to read from enBlocks.
		blockSize := eInfo.BlockSize

		// curChunkSize is chunkSize until end block.
		curChunkSize := chunkSize

		// We have endBlock, verify if we need to have padding.
		if block == endBlock && (totalLength%eInfo.BlockSize != 0) {
			// If this is the last block and size of the block is < BlockSize.
			curChunkSize = getEncodedBlockLen(totalLength%eInfo.BlockSize, eInfo.DataBlocks)

			// For the last block, the block size can be less than BlockSize.
			blockSize = totalLength % eInfo.BlockSize
		}

		// Block offset.
		// NOTE: That for the offset calculation we have to use chunkSize and
		// not curChunkSize. If we use curChunkSize for offset calculation
		// then it can result in wrong offset for the last block.
		blockOffset := block * chunkSize

		// nextIndex - index from which next set of parallel reads
		// should happen.
		nextIndex := 0

		for {
			// readDisks - disks from which we need to read in parallel.
			var readDisks []StorageAPI
			var err error
			readDisks, nextIndex, err = getReadDisks(orderedDisks, nextIndex, eInfo.DataBlocks)
			if err != nil {
				return bytesWritten, err
			}
			parallelRead(volume, path, readDisks, orderedDisks, enBlocks, blockOffset, curChunkSize, bitRotVerify)
			if isSuccessDecodeBlocks(enBlocks, eInfo.DataBlocks) {
				// If enough blocks are available to do rs.Reconstruct()
				break
			}
			if nextIndex == len(orderedDisks) {
				// No more disks to read from.
				return bytesWritten, errXLReadQuorum
			}
		}

		// If we have all the data blocks no need to decode, continue to write.
		if !isSuccessDataBlocks(enBlocks, eInfo.DataBlocks) {
			// Reconstruct the missing data blocks.
			if err := decodeData(enBlocks, eInfo.DataBlocks, eInfo.ParityBlocks); err != nil {
				return bytesWritten, err
			}
		}

		var outSize, outOffset int64
		// If this is start block, skip unwanted bytes.
		if block == startBlock {
			outOffset = bytesToSkip
		}

		// Total data to be read.
		outSize = blockSize
		if length-bytesWritten < blockSize {
			// We should not send more data than what was requested.
			outSize = length - bytesWritten
		}

		// Write data blocks.
		n, err := writeDataBlocks(writer, enBlocks, eInfo.DataBlocks, outOffset, outSize)
		if err != nil {
			return bytesWritten, err
		}

		// Update total bytes written.
		bytesWritten += n
	}

	// Success.
	return bytesWritten, nil
}

// PartObjectChecksum - returns the checksum for the part name from the checksum slice.
func (e erasureInfo) PartObjectChecksum(partName string) checkSumInfo {
	for _, checksum := range e.Checksum {
		if checksum.Name == partName {
			return checksum
		}
	}
	return checkSumInfo{}
}

// xlMetaPartBlockChecksums - get block checksums for a given part.
func metaPartBlockChecksums(disks []StorageAPI, eInfos []erasureInfo, partName string) (blockCheckSums []checkSumInfo) {
	for index := range disks {
		if eInfos[index].IsValid() {
			// Save the read checksums for a given part.
			blockCheckSums = append(blockCheckSums, eInfos[index].PartObjectChecksum(partName))
		} else {
			blockCheckSums = append(blockCheckSums, checkSumInfo{})
		}
	}
	return blockCheckSums
}

// isValidBlock - calculates the checksum hash for the block and
// validates if its correct returns true for valid cases, false otherwise.
func isValidBlock(disk StorageAPI, volume, path string, blockCheckSum checkSumInfo) (ok bool) {
	// Disk is not available, not a valid block.
	if disk == nil {
		return false
	}
	// Read everything for a given block and calculate hash.
	hashWriter := newHash(blockCheckSum.Algorithm)
	hashBytes, err := hashSum(disk, volume, path, hashWriter)
	if err != nil {
		errorIf(err, "Unable to calculate checksum %s/%s", volume, path)
		return false
	}
	return hex.EncodeToString(hashBytes) == blockCheckSum.Hash
}

// decodeData - decode encoded blocks.
func decodeData(enBlocks [][]byte, dataBlocks, parityBlocks int) error {
	// Initialized reedsolomon.
	rs, err := reedsolomon.New(dataBlocks, parityBlocks)
	if err != nil {
		return err
	}

	// Reconstruct encoded blocks.
	err = rs.Reconstruct(enBlocks)
	if err != nil {
		return err
	}

	// Verify reconstructed blocks (parity).
	ok, err := rs.Verify(enBlocks)
	if err != nil {
		return err
	}
	if !ok {
		// Blocks cannot be reconstructed, corrupted data.
		err = errors.New("Verification failed after reconstruction, data likely corrupted.")
		return err
	}

	// Success.
	return nil
}
