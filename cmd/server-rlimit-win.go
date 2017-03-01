// +build windows

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

package cmd

import "github.com/minio/minio/pkg/sys"

func setMaxOpenFiles() error {
	// Golang uses Win32 file API (CreateFile, WriteFile, ReadFile,
	// CloseHandle, etc.), then you don't have a limit on open files
	// (well, you do but it is based on your resources like memory).
	return nil
}

func setMaxMemory() error {
	// Get total RAM.
	stats, err := sys.GetStats()
	if err != nil {
		// Ignore sys.ErrNotImplemented error.
		if err == sys.ErrNotImplemented {
			err = nil
		}

		return err
	}

	// Set 50% of total RAM to globalMaxCacheSize.
	if stats.TotalRAM >= minRAMSize {
		globalMaxCacheSize = stats.TotalRAM / 2
	}

	return nil
}
