/*
 * Minio Cloud Storage, (C) 2015, 2016 Minio, Inc.
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
	"errors"
	"net/http"
	"os"
	"strings"

	router "github.com/gorilla/mux"
)

// newObjectLayer - initialize any object layer depending on the number of disks.
func newObjectLayer(disks, ignoredDisks []string) (ObjectLayer, error) {
	if len(disks) == 1 {
		exportPath := disks[0]
		// Initialize FS object layer.
		return newFSObjects(exportPath)
	}
	// TODO: use dsync to block other concurrently booting up nodes.
	// Initialize XL object layer.
	objAPI, err := newXLObjects(disks, ignoredDisks)
	if err == errXLWriteQuorum {
		return objAPI, errors.New("Disks are different with last minio server run.")
	}
	return objAPI, err
}

func newObjectLayerFactory(disks, ignoredDisks []string) func() ObjectLayer {
	var objAPI ObjectLayer
	// FIXME: This needs to be go-routine safe.
	return func() ObjectLayer {
		var err error
		if objAPI != nil {
			return objAPI
		}

		// Acquire a distributed lock to ensure only one of the nodes
		// initializes the format.json.
		nsMutex.Lock(minioMetaBucket, formatConfigFile)
		defer nsMutex.Unlock(minioMetaBucket, formatConfigFile)
		objAPI, err = newObjectLayer(disks, ignoredDisks)
		if err != nil {
			return nil
		}
		// Migrate bucket policy from configDir to .minio.sys/buckets/
		err = migrateBucketPolicyConfig(objAPI)
		fatalIf(err, "Unable to migrate bucket policy from config directory")

		err = cleanupOldBucketPolicyConfigs()
		fatalIf(err, "Unable to clean up bucket policy from config directory.")

		// Register the callback that should be called when the process shuts down.
		globalShutdownCBs.AddObjectLayerCB(func() errCode {
			if sErr := objAPI.Shutdown(); sErr != nil {
				return exitFailure
			}
			return exitSuccess
		})

		// Initialize a new event notifier.
		err = initEventNotifier(objAPI)
		fatalIf(err, "Unable to initialize event notification queue")

		// Initialize and load bucket policies.
		err = initBucketPolicies(objAPI)
		fatalIf(err, "Unable to load all bucket policies")

		return objAPI
	}
}

// configureServer handler returns final handler for the http server.
func configureServerHandler(srvCmdConfig serverCmdConfig) http.Handler {
	// Initialize storage rpc servers for every disk that is hosted on this node.
	storageRPCs, err := newRPCServer(srvCmdConfig)
	fatalIf(err, "Unable to initialize storage RPC server.")

	// Initialize and monitor shutdown signals.
	err = initGracefulShutdown(os.Exit)
	fatalIf(err, "Unable to initialize graceful shutdown operation")

	newObjectLayerFn := newObjectLayerFactory(srvCmdConfig.disks, srvCmdConfig.ignoredDisks)
	// Initialize API.
	apiHandlers := objectAPIHandlers{
		ObjectAPI: newObjectLayerFn,
	}

	// Initialize Web.
	webHandlers := &webAPIHandlers{
		ObjectAPI: newObjectLayerFn,
	}

	// Initialize router.
	mux := router.NewRouter()

	// Register all routers.
	registerStorageRPCRouters(mux, storageRPCs)
	initDistributedNSLock(mux, srvCmdConfig)

	// FIXME: till net/rpc auth is brought in "minio control" can be enabled only though
	// this env variable.
	if os.Getenv("MINIO_CONTROL") != "" {
		registerControlRPCRouter(mux, ctrlHandlers)
	}

	// set environmental variable MINIO_BROWSER=off to disable minio web browser.
	// By default minio web browser is enabled.
	if !strings.EqualFold(os.Getenv("MINIO_BROWSER"), "off") {
		registerWebRouter(mux, webHandlers)
	}

	registerAPIRouter(mux, apiHandlers)
	// Add new routers here.

	// List of some generic handlers which are applied for all
	// incoming requests.
	var handlerFns = []HandlerFunc{
		// Limits the number of concurrent http requests.
		setRateLimitHandler,
		// Limits all requests size to a maximum fixed limit
		setRequestSizeLimitHandler,
		// Adds 'crossdomain.xml' policy handler to serve legacy flash clients.
		setCrossDomainPolicy,
		// Redirect some pre-defined browser request paths to a static location prefix.
		setBrowserRedirectHandler,
		// Validates if incoming request is for restricted buckets.
		setPrivateBucketHandler,
		// Adds cache control for all browser requests.
		setBrowserCacheControlHandler,
		// Validates all incoming requests to have a valid date header.
		setTimeValidityHandler,
		// CORS setting for all browser API requests.
		setCorsHandler,
		// Validates all incoming URL resources, for invalid/unsupported
		// resources client receives a HTTP error.
		setIgnoreResourcesHandler,
		// Auth handler verifies incoming authorization headers and
		// routes them accordingly. Client receives a HTTP error for
		// invalid/unsupported signatures.
		setAuthHandler,
		// Add new handlers here.
	}

	// Register rest of the handlers.
	return registerHandlers(mux, handlerFns...)
}
