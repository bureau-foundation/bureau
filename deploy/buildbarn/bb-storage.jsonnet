// SPDX-License-Identifier: Apache-2.0
//
// bb-storage configuration for Bureau's Buildbarn deployment.
//
// Single-node CAS (content-addressable storage) and AC (action cache).
// Bazel connects with --remote_cache=grpc://host:8980.
//
// Storage uses block-device-based files that are allocated as sparse files.
// The declared sizeBytes values do not consume disk until written. The
// block rotation scheme (oldBlocks/currentBlocks/newBlocks) provides
// self-cleaning without a separate garbage collection process.
//
// Persistence is enabled so the cache survives container restarts.
// Epoch checkpoints are written every 5 minutes to the persistent_state
// directories.

{
  grpcServers: [{
    listenAddresses: [':8980'],
    authenticationPolicy: { allow: {} },
  }],

  // 16 MB max message size covers large protobuf tree messages that Bazel
  // sends for deeply nested source trees.
  maximumMessageSizeBytes: 16 * 1024 * 1024,

  global: {
    diagnosticsHttpServer: {
      httpServers: [{
        listenAddresses: [':9980'],
        authenticationPolicy: { allow: {} },
      }],
      enablePrometheus: true,
      enablePprof: true,
      enableActiveSpans: true,
    },
  },

  contentAddressableStorage: {
    backend: {
      'local': {
        keyLocationMapOnBlockDevice: {
          file: {
            path: '/storage-cas/key_location_map',
            sizeBytes: 400 * 1024 * 1024,
          },
        },
        keyLocationMapMaximumGetAttempts: 16,
        keyLocationMapMaximumPutAttempts: 64,
        oldBlocks: 8,
        currentBlocks: 24,
        newBlocks: 3,
        blocksOnBlockDevice: {
          source: {
            file: {
              path: '/storage-cas/blocks',
              // 32 GB total block storage (sparse). Effective capacity is
              // roughly 29.5 GB after accounting for spare blocks:
              // 32 GB * 35/(35+3) where 35 = old+current+new, 3 = spare.
              sizeBytes: 32 * 1024 * 1024 * 1024,
            },
          },
          spareBlocks: 3,
        },
        persistent: {
          stateDirectoryPath: '/storage-cas/persistent_state',
          minimumEpochInterval: '300s',
        },
      },
    },
    getAuthorizer: { allow: {} },
    putAuthorizer: { allow: {} },
    findMissingAuthorizer: { allow: {} },
  },

  actionCache: {
    backend: {
      'local': {
        keyLocationMapOnBlockDevice: {
          file: {
            path: '/storage-ac/key_location_map',
            sizeBytes: 1024 * 1024,
          },
        },
        keyLocationMapMaximumGetAttempts: 16,
        keyLocationMapMaximumPutAttempts: 64,
        oldBlocks: 8,
        currentBlocks: 24,
        newBlocks: 1,
        blocksOnBlockDevice: {
          source: {
            file: {
              path: '/storage-ac/blocks',
              // 20 MB for action results (each result is a few KB).
              sizeBytes: 20 * 1024 * 1024,
            },
          },
          spareBlocks: 3,
        },
        persistent: {
          stateDirectoryPath: '/storage-ac/persistent_state',
          minimumEpochInterval: '300s',
        },
      },
    },
    getAuthorizer: { allow: {} },
    putAuthorizer: { allow: {} },
  },
}
