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

// getDataBlocks - fetches the data block only part of the input encoded blocks.
func getDataBlocks(enBlocks [][]byte, dataBlocks int, curBlockSize int) []byte {
	var data []byte
	for _, block := range enBlocks[:dataBlocks] {
		data = append(data, block...)
	}
	data = data[:curBlockSize]
	return data
}

// checkBlockSize return the size of a single block.
// The first non-zero size is returned,
// or 0 if all blocks are size 0.
func checkBlockSize(blocks [][]byte) int {
	for _, block := range blocks {
		if len(block) != 0 {
			return len(block)
		}
	}
	return 0
}

// calculate the blockSize based on input length and total number of
// data blocks.
func getEncodedBlockLen(inputLen int64, dataBlocks int) (curBlockSize int64) {
	curBlockSize = (inputLen + int64(dataBlocks) - 1) / int64(dataBlocks)
	return curBlockSize
}
